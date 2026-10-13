package service

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const openAIReasoningCheckpointLimit = 8 << 20

// These opaque items live only inside one request. Never put them in shared
// session storage, diagnostics, or an error message.
type openAIReasoningCheckpoints struct {
	enabled    bool
	blocked    bool
	usageKnown bool
	bytes      int
	items      []json.RawMessage
	seen       map[string]bool
}

type openAICheckpointContinuation struct {
	items  []json.RawMessage
	reason string
	cause  error
}

func (e *openAICheckpointContinuation) Error() string {
	return "upstream interrupted before output; reasoning checkpoint available"
}
func (e *openAICheckpointContinuation) Unwrap() error { return e.cause }

func newOpenAIReasoningCheckpoints(enabled bool) *openAIReasoningCheckpoints {
	return &openAIReasoningCheckpoints{enabled: enabled, seen: make(map[string]bool)}
}

func (p *openAIReasoningCheckpoints) observe(message []byte, eventType string) {
	if !p.enabled || p.blocked {
		return
	}
	// Presence is distinct from token count: a reported all-zero usage object
	// is still known usage and must not be discarded by a continuation.
	for _, path := range []string{"usage", "response.usage"} {
		if usage := gjson.GetBytes(message, path); usage.Exists() && usage.Type != gjson.Null {
			p.usageKnown = true
		}
	}
	// Tool lifecycle events are evidence of a started tool even if an upstream
	// omitted its output_item.added event. Fail closed for tool event variants.
	if strings.HasPrefix(eventType, "response.") &&
		(strings.Contains(eventType, "_call.") || strings.Contains(eventType, "_call_") || strings.Contains(eventType, "tool")) {
		p.blocked = true
		return
	}
	if strings.HasPrefix(eventType, "response.reasoning_summary") || strings.HasPrefix(eventType, "response.reasoning_text") {
		p.blocked = true
		return
	}
	if output := gjson.GetBytes(message, "response.output"); output.Exists() && output.Type != gjson.Null {
		if !output.IsArray() {
			p.blocked = true
			return
		}
		output.ForEach(func(_, item gjson.Result) bool {
			if !openAICheckpointReasoningIsPrivate(item) || openAICheckpointReasoningIsPartial(item) {
				p.blocked = true
				return false
			}
			return true
		})
		if p.blocked {
			return
		}
	}
	if isOpenAIWSTokenEvent(eventType) || eventType == "response.refusal.done" || strings.HasPrefix(eventType, "response.content_part.") {
		p.blocked = true
		return
	}
	if eventType != "response.output_item.added" && eventType != "response.output_item.done" {
		return
	}
	item := gjson.GetBytes(message, "item")
	if !openAICheckpointReasoningIsPrivate(item) {
		p.blocked = true // Includes uncompleted tool calls; never replay their effects.
		return
	}
	if eventType == "response.output_item.done" && openAICheckpointReasoningIsPartial(item) {
		p.blocked = true
		return
	}
	if eventType != "response.output_item.done" || item.Get("encrypted_content").String() == "" {
		return
	}
	id := item.Get("id").String()
	if id == "" || p.seen[id] {
		return
	}
	if p.bytes+len(item.Raw) > openAIReasoningCheckpointLimit {
		p.blocked = true
		p.items = nil
		return
	}
	p.bytes += len(item.Raw)
	p.items = append(p.items, json.RawMessage(item.Raw))
	p.seen[id] = true
}

func openAICheckpointReasoningIsPrivate(item gjson.Result) bool {
	if !item.IsObject() || item.Get("type").String() != "reasoning" {
		return false
	}
	for _, field := range []string{"summary", "content"} {
		value := item.Get(field)
		if value.Exists() && value.Type != gjson.Null && (!value.IsArray() || len(value.Array()) != 0) {
			return false
		}
	}
	return true
}

func openAICheckpointReasoningIsPartial(item gjson.Result) bool {
	status := item.Get("status")
	return status.Exists() && status.Type != gjson.Null && status.String() != "completed"
}

func (p *openAIReasoningCheckpoints) canContinue(ctx context.Context, wroteDownstream bool, usage *OpenAIUsage) bool {
	return p.enabled && !p.blocked && !p.usageKnown && len(p.items) > 0 && !wroteDownstream && ctx.Err() == nil &&
		usage != nil && *usage == (OpenAIUsage{})
}

func (p *openAIReasoningCheckpoints) continuation(reason string, cause error) error {
	return &openAICheckpointContinuation{items: p.items, reason: reason, cause: cause}
}

func includeOpenAIEncryptedReasoning(payload map[string]any) {
	var include []string
	if raw, err := json.Marshal(payload["include"]); err == nil {
		_ = json.Unmarshal(raw, &include)
	}
	for _, value := range include {
		if value == "reasoning.encrypted_content" {
			return
		}
	}
	payload["include"] = append(include, "reasoning.encrypted_content")
}

func openAICheckpointInput(reqBody map[string]any) ([]json.RawMessage, bool) {
	// Stateful references and explicit output budgets cannot be faithfully
	// reconstructed using only completed reasoning items.
	for _, key := range []string{"previous_response_id", "conversation", "max_output_tokens"} {
		if value, exists := reqBody[key]; exists && value != nil {
			return nil, false
		}
	}
	input := reqBody["input"]
	if text, ok := input.(string); ok {
		item, _ := json.Marshal(map[string]any{"role": "user", "content": text})
		if len(item) > openAIReasoningCheckpointLimit {
			return nil, false
		}
		return []json.RawMessage{item}, true
	}
	raw, err := json.Marshal(input)
	var items []json.RawMessage
	if err != nil || len(raw) > openAIReasoningCheckpointLimit || json.Unmarshal(raw, &items) != nil || len(items) == 0 {
		return nil, false
	}
	for _, item := range items {
		if gjson.GetBytes(item, "type").String() == "item_reference" {
			return nil, false
		}
	}
	return items, true
}

func (s *OpenAIGatewayService) forwardOpenAIWSV2(
	ctx context.Context, c *gin.Context, account *Account, reqBody map[string]any,
	clientPromptCacheKey, executionScope, token string, decision OpenAIWSProtocolDecision,
	isCodexCLI, reqStream bool, originalModel, mappedModel string, startTime time.Time,
	attempt int, lastFailureReason string, agentTaskRecoveryTried *bool,
) (*OpenAIForwardResult, error) {
	bridgeRequest, bridge := openAIAstraHTTPBridgeFromContext(c)
	enabled := s != nil && s.cfg != nil && s.cfg.Gateway.OpenAIWS.CtxPoolCheckpointRecoveryEnabled &&
		bridge && !bridgeRequest.outputBudgetPresent && account != nil && account.Type == AccountTypeOAuth
	var input []json.RawMessage
	if enabled {
		input, enabled = openAICheckpointInput(reqBody)
	}
	if enabled {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, startTime.Add(s.astraHTTPBridgeReadTimeout()))
		defer cancel()
	}
	result, err := s.forwardOpenAIWSV2Once(ctx, c, account, reqBody, clientPromptCacheKey, executionScope, token,
		decision, isCodexCLI, reqStream, originalModel, mappedModel, startTime, attempt, lastFailureReason,
		agentTaskRecoveryTried, enabled)
	var checkpoint *openAICheckpointContinuation
	if !enabled || !errors.As(err, &checkpoint) {
		return result, err
	}
	next := maps.Clone(reqBody)
	input = append(input, checkpoint.items...)
	next["input"] = input
	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{Platform: PlatformOpenAI, AccountID: account.ID,
		Kind: "checkpoint_continuation", Stage: "inference", Reason: checkpoint.reason})
	logOpenAIWSModeInfo("checkpoint_continuation account_id=%d retained_items=%d reason=%s", account.ID, len(checkpoint.items), checkpoint.reason)
	// Same account, owner, scope, model, effort and request deadline. The first
	// private lease is already released. No further continuation is permitted.
	result, err = s.forwardOpenAIWSV2Once(ctx, c, account, next, clientPromptCacheKey, executionScope, token,
		decision, isCodexCLI, reqStream, originalModel, mappedModel, startTime, attempt, lastFailureReason,
		agentTaskRecoveryTried, false)
	// A fresh dial/write can fail before the inner stream cleanup is installed.
	// Once a heartbeat committed SSE, finish it explicitly rather than leaving
	// the client waiting forever or falling back to another inference request.
	if err != nil && !IsResponseCommitted(c) && !errors.Is(ctx.Err(), context.Canceled) {
		if reqStream && c.Writer.Written() {
			_, _ = c.Writer.Write(append(append([]byte("data: "), buildAstraHTTPBridgeFailedEvent("", originalModel, nil, 0)...), '\n', '\n'))
			c.Writer.Flush()
		} else {
			c.JSON(502, gin.H{"error": gin.H{"code": "upstream_stream_interrupted", "type": "upstream_error", "message": "Upstream response stream was interrupted"}})
		}
		MarkResponseCommitted(c) // Never let the outer fallback replay this accepted continuation.
	}
	return result, err
}
