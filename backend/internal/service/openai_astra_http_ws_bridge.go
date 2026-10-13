package service

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/util/responseheaders"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
)

const openAIAstraHTTPBridgeContextKey = "openai_astra_http_ws_bridge"

type openAIAstraHTTPBridgeRequest struct {
	executionScope      string
	outputBudgetPresent bool
}

// ctx_pool bridges full-input Responses for non-Astra models and downstream keys.
// Stateful continuations retain HTTP because a fresh private socket cannot
// safely reconstruct upstream stored state. The original canary remains
// available when automatic bridging is disabled.
func (s *OpenAIGatewayService) shouldBridgeAstraHTTP(c *gin.Context, account *Account, body []byte, decision OpenAIWSProtocolDecision) bool {
	if s == nil || s.cfg == nil || c == nil || c.Request == nil ||
		GetOpenAIClientTransport(c) != OpenAIClientTransportHTTP || getAPIKeyIDFromContext(c) <= 0 ||
		account == nil || !account.IsOpenAIOAuth() || account.IsOpenAIPassthroughEnabled() ||
		!account.IsOpenAIResponsesWebSocketV2Enabled() || decision.Transport != OpenAIUpstreamTransportResponsesWebsocketV2 ||
		isOpenAIResponsesCompactPath(c) || !strings.HasSuffix(strings.TrimRight(c.Request.URL.Path, "/"), "/responses") {
		return false
	}
	mode := account.ResolveOpenAIResponsesWebSocketV2Mode(s.cfg.Gateway.OpenAIWS.IngressModeDefault)
	if mode == "off" {
		return false
	}
	model := gjson.GetBytes(body, "model").String()
	// Astra HTTP ingress stays on the native HTTP endpoint unless its separate
	// experiment is explicitly enabled. The common ctx_pool switch must not
	// bypass that gate, including when an account maps a client alias to Astra.
	if s.cfg.Gateway.OpenAIWS.CtxPoolHTTPBridgeEnabled && mode == "ctx_pool" && !isOpenAIGPT6AstraModel(account.GetMappedModel(model)) {
		return strings.TrimSpace(gjson.GetBytes(body, "model").String()) != "" && openAIHTTPBridgeContinuationParam(body) == ""
	}
	if !s.cfg.Gateway.OpenAIWS.AstraHTTPBridgeEnabled {
		return false
	}
	optIn, _ := account.Extra["openai_astra_http_ws_bridge_enabled"].(bool)
	if ids := s.cfg.Gateway.OpenAIWS.AstraHTTPBridgeAPIKeyIDs; len(ids) > 0 {
		allowed := false
		for _, id := range ids {
			allowed = allowed || id == getAPIKeyIDFromContext(c)
		}
		if !allowed {
			return false
		}
	}
	return optIn && gjson.GetBytes(body, "model").String() == "gpt-6-astra" && account.GetMappedModel("gpt-6-astra") == "gpt-6-astra"
}

const openAIAstraNativeHTTPContextKey = "openai_astra_native_http"

// Retain only scalar protocol evidence for this request, never its input,
// answer, tool arguments or encrypted reasoning. Acceptance blocks invisible
// replay even when an HTTP reader has buffered all events from a nonstream call.
type openAIAstraNativeHTTPState struct {
	accepted         bool
	stream           bool
	usageKnown       bool
	usage            OpenAIUsage
	responseID       string
	lastSequence     int
	failureDelivered bool
	errorPayload     []byte
}

func prepareOpenAIAstraNativeHTTP(c *gin.Context, account *Account, body []byte, decision OpenAIWSProtocolDecision) {
	if c == nil {
		return
	}
	c.Set(openAIAstraNativeHTTPContextKey, nil)
	if account == nil || !account.IsOpenAIOAuth() || account.IsOpenAIPassthroughEnabled() ||
		GetOpenAIClientTransport(c) != OpenAIClientTransportHTTP || decision.Transport != OpenAIUpstreamTransportHTTPSSE ||
		isOpenAIResponsesCompactPath(c) || !isOpenAIGPT6AstraModel(account.GetMappedModel(gjson.GetBytes(body, "model").String())) {
		return
	}
	c.Set(openAIAstraNativeHTTPContextKey, &openAIAstraNativeHTTPState{stream: gjson.GetBytes(body, "stream").Bool(), lastSequence: -1})
}

func openAIAstraNativeHTTPStateFromContext(c *gin.Context) *openAIAstraNativeHTTPState {
	if c == nil {
		return nil
	}
	raw, _ := c.Get(openAIAstraNativeHTTPContextKey)
	state, _ := raw.(*openAIAstraNativeHTTPState)
	return state
}

func openAIAstraNativeHTTPAccepted(c *gin.Context) bool {
	state := openAIAstraNativeHTTPStateFromContext(c)
	return state != nil && state.accepted
}

func observeOpenAIAstraNativeHTTP(c *gin.Context, payload []byte, eventType string) {
	state := openAIAstraNativeHTTPStateFromContext(c)
	if state == nil || !gjson.ValidBytes(payload) {
		return
	}
	if sequence := gjson.GetBytes(payload, "sequence_number"); validOpenAIAstraUsageCount(sequence) && uint64(sequence.Int()) < uint64(^uint(0)>>1) {
		state.lastSequence = max(state.lastSequence, int(sequence.Int()))
	}
	id := gjson.GetBytes(payload, "response.id")
	if id.Type == gjson.String && len(id.String()) <= 256 && strings.TrimSpace(id.String()) != "" {
		state.responseID = id.String()
		if eventType == "response.created" || eventType == "response.in_progress" || eventType == "response.failed" {
			state.accepted = true
		}
	}
	for _, path := range []string{"usage", "response.usage"} {
		if gjson.GetBytes(payload, path).IsObject() {
			state.accepted = true
		}
	}
	for _, path := range []string{"usage", "response.usage"} {
		value := gjson.GetBytes(payload, path)
		if !validOpenAIAstraUsageCount(value.Get("input_tokens")) || !validOpenAIAstraUsageCount(value.Get("output_tokens")) {
			continue
		}
		if usage, known := openAIUsageFromGJSON(value); known {
			state.usageKnown = true
			if openAIStreamEventTypeIsTerminal(eventType) {
				if openAIUsageHasTokens(&usage) || !openAIUsageHasTokens(&state.usage) {
					state.usage = usage
				}
			} else {
				mergeOpenAIUsageNonZero(&state.usage, usage)
			}
		}
		break
	}
	if strings.HasPrefix(eventType, "response.") &&
		(strings.Contains(eventType, "_call.") || strings.Contains(eventType, "_call_") || strings.Contains(eventType, "tool")) {
		state.accepted = true
	}
	if strings.HasPrefix(eventType, "response.reasoning_") {
		state.accepted = true
	}
	if eventType == "response.output_item.added" || eventType == "response.output_item.done" {
		if item := gjson.GetBytes(payload, "item"); item.IsObject() && strings.TrimSpace(item.Get("type").String()) != "" {
			// Reasoning, an empty message and an empty tool argument field all
			// prove generation progress; none is permission to replay the input.
			state.accepted = true
		}
	}
	if openAIStreamDataStartsVisibleOutput(string(payload), eventType) {
		state.accepted = true
	}
	if eventType == "error" || eventType == "response.failed" {
		prefix := "error"
		if eventType == "response.failed" && gjson.GetBytes(payload, "response.error").IsObject() {
			prefix = "response.error"
		}
		code := gjson.GetBytes(payload, prefix+".code").String()
		errType := gjson.GetBytes(payload, prefix+".type").String()
		if code == "" {
			code = gjson.GetBytes(payload, "code").String()
		}
		message := sanitizeUpstreamErrorMessage(extractOpenAISSEErrorMessage(payload))
		status := openAIWSExplicitErrorHTTPStatus(payload)
		if status == 0 {
			status = openAIWSErrorHTTPStatusFromRaw(code, errType)
			if isOpenAIContextWindowError(message, payload) {
				status = http.StatusBadRequest
				if errType == "" {
					errType = "invalid_request_error"
				}
			}
		}
		if errType == "" {
			errType = "upstream_error"
		}
		state.errorPayload, _ = marshalOpenAIUpstreamJSON(gin.H{"error": gin.H{
			"code": code, "type": errType, "message": message, "status": status,
		}})
	}
}

func validOpenAIAstraUsageCount(value gjson.Result) bool {
	if value.Type != gjson.Number {
		return false
	}
	// Official Responses usage counts are integer JSON values. Reject absent,
	// string, fractional, negative and overflowing evidence rather than filling
	// missing token counts with zero for a failed inference request.
	count, err := strconv.ParseInt(value.Raw, 10, 64)
	return err == nil && count >= 0 && uint64(count) <= uint64(^uint(0)>>1)
}

func markOpenAIAstraNativeHTTPFailureDelivered(c *gin.Context) {
	if state := openAIAstraNativeHTTPStateFromContext(c); state != nil && state.accepted {
		state.failureDelivered = true
		MarkResponseCommitted(c)
	}
}

func (s *OpenAIGatewayService) writeOpenAIAstraNativeHTTPFailure(c *gin.Context, resp *http.Response, model string) {
	state := openAIAstraNativeHTTPStateFromContext(c)
	if state == nil || !state.accepted || state.failureDelivered || IsResponseCommitted(c) {
		return
	}
	if c.Request != nil && c.Request.Context().Err() != nil {
		return
	}
	status := http.StatusBadGateway
	code, errType, message := "upstream_stream_interrupted", "upstream_error", "Upstream response stream was interrupted"
	if len(state.errorPayload) > 0 {
		status = openAIWSErrorHTTPStatus(state.errorPayload)
		if value := gjson.GetBytes(state.errorPayload, "error.code").String(); value != "" {
			code = value
		}
		errType = gjson.GetBytes(state.errorPayload, "error.type").String()
		if value := gjson.GetBytes(state.errorPayload, "error.message").String(); value != "" {
			message = value
		}
	}
	setOpsUpstreamError(c, status, message, "")
	if state.stream && c.Writer.Written() {
		payload, _ := marshalOpenAIUpstreamJSON(gin.H{"error": gin.H{"code": code, "type": errType, "message": message}})
		_, _ = c.Writer.Write(append(append([]byte("event: response.failed\ndata: "), buildAstraHTTPBridgeFailedEvent(state.responseID, model, payload, state.lastSequence+1)...), '\n', '\n'))
		c.Writer.Flush()
		MarkOpsStreamFailure(c, errType, code, message, status)
	} else {
		if resp != nil {
			responseheaders.WriteFilteredHeaders(c.Writer.Header(), resp.Header, s.responseHeaderFilter)
		}
		c.JSON(status, gin.H{"error": gin.H{"code": code, "type": errType, "message": message}})
	}
	markOpenAIAstraNativeHTTPFailureDelivered(c)
}

func (s *OpenAIGatewayService) finishOpenAIAstraNativeHTTPFailure(ctx context.Context, c *gin.Context, resp *http.Response, originalModel, billingModel, mappedModel string, reasoningEffort, serviceTier *string, startTime time.Time) (*OpenAIForwardResult, error) {
	state := openAIAstraNativeHTTPStateFromContext(c)
	// Do not retain a failover cause: errors.As follows wrapped causes, which
	// would let the outer account loop replay this accepted generation.
	err := errors.New("astra native HTTP generation failed after acceptance")
	if ctx.Err() != nil {
		err = ctx.Err()
	} else {
		s.writeOpenAIAstraNativeHTTPFailure(c, resp, originalModel)
	}
	if state == nil || !state.usageKnown {
		return nil, err // Unknown usage is not a successful, zero-token result.
	}
	return &OpenAIForwardResult{
		RequestID: resp.Header.Get("x-request-id"), UpstreamHeaders: resp.Header, ResponseID: state.responseID,
		Usage: state.usage, Model: originalModel, BillingModel: billingModel, UpstreamModel: mappedModel,
		UpstreamResponseModel: observedUpstreamResponseModel(c), UpstreamResponseModelConflict: observedUpstreamResponseModelConflict(c),
		UpstreamResponseServiceTier: observedUpstreamResponseServiceTier(c), ServiceTier: resolvedOpenAIUpstreamServiceTier(c, serviceTier),
		ReasoningEffort: reasoningEffort, Stream: state.stream, Duration: time.Since(startTime),
	}, err
}

func buildAstraHTTPBridgeFailedEvent(responseID, model string, source []byte, sequence int) []byte {
	if responseID = strings.TrimSpace(responseID); responseID == "" {
		responseID = "resp_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	}
	code := strings.TrimSpace(gjson.GetBytes(source, "error.code").String())
	if code == "" {
		code = strings.TrimSpace(gjson.GetBytes(source, "response.error.code").String())
	}
	if code == "" {
		code = "upstream_stream_interrupted"
	}
	message := extractOpenAISSEErrorMessage(source)
	if message == "" {
		message = "Upstream response stream was interrupted"
	}
	errType := strings.TrimSpace(gjson.GetBytes(source, "error.type").String())
	if errType == "" {
		errType = strings.TrimSpace(gjson.GetBytes(source, "response.error.type").String())
	}
	if errType == "" {
		errType = "upstream_error"
	}
	// JSON primitives only; include required strict-client fields and never
	// manufacture usage for a connection failure.
	payload, _ := marshalOpenAIUpstreamJSON(gin.H{
		"type": "response.failed", "sequence_number": sequence,
		"response": gin.H{"id": responseID, "object": "response", "created_at": time.Now().Unix(),
			"model": model, "status": "failed", "output": []any{},
			"error": gin.H{"type": errType, "code": code, "message": message}},
	})
	return payload
}

func openAIHTTPBridgeContinuationParam(body []byte) string {
	rejectedParam := ""
	if strings.TrimSpace(gjson.GetBytes(body, "previous_response_id").String()) != "" {
		rejectedParam = "previous_response_id"
	}
	conversation := gjson.GetBytes(body, "conversation")
	if conversation.Exists() && conversation.Type != gjson.Null && strings.TrimSpace(conversation.String()) != "" {
		rejectedParam = "conversation"
	}
	gjson.GetBytes(body, "input").ForEach(func(_, item gjson.Result) bool {
		if item.Get("type").String() == "item_reference" {
			rejectedParam = "input.item_reference"
			return false
		}
		return true
	})
	return rejectedParam
}

func prepareOpenAIAstraHTTPBridge(c *gin.Context, account *Account, body []byte, rawScope string) (string, error) {
	rejectedParam := openAIHTTPBridgeContinuationParam(body)
	if rejectedParam != "" {
		err := fmt.Errorf("OpenAI HTTP WebSocket bridge requires full input; %s is unsupported", rejectedParam)
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"type": "invalid_request_error", "message": err.Error(), "param": rejectedParam}})
		MarkResponseCommitted(c)
		return "", err
	}
	// Every experiment is a fresh, full-input turn. Include both downstream and
	// upstream identity even for clients that supply no reliable thread/session.
	scope, _ := deriveOpenAISessionHashes(fmt.Sprintf("astra_http_ws:%d:%d:%s:%s", getAPIKeyIDFromContext(c), account.ID, rawScope, uuid.NewString()))
	c.Set(openAIAstraHTTPBridgeContextKey, openAIAstraHTTPBridgeRequest{executionScope: scope,
		outputBudgetPresent: gjson.GetBytes(body, "max_output_tokens").Exists() ||
			gjson.GetBytes(body, "max_tokens").Exists() || gjson.GetBytes(body, "max_completion_tokens").Exists()})
	return scope, nil
}

func openAIAstraHTTPBridgeFromContext(c *gin.Context) (openAIAstraHTTPBridgeRequest, bool) {
	if c == nil {
		return openAIAstraHTTPBridgeRequest{}, false
	}
	raw, exists := c.Get(openAIAstraHTTPBridgeContextKey)
	request, valid := raw.(openAIAstraHTTPBridgeRequest)
	return request, exists && valid
}

func (s *OpenAIGatewayService) astraHTTPBridgeReadTimeout() time.Duration {
	if s != nil && s.cfg != nil && s.cfg.Gateway.OpenAIWS.AstraHTTPBridgeReadTimeoutSeconds > 0 {
		return time.Duration(s.cfg.Gateway.OpenAIWS.AstraHTTPBridgeReadTimeoutSeconds) * time.Second
	}
	return time.Hour
}

func (s *OpenAIGatewayService) astraHTTPBridgeHeartbeat() time.Duration {
	if s != nil && s.cfg != nil && s.cfg.Gateway.OpenAIWS.AstraHTTPBridgeHeartbeatSeconds > 0 {
		return time.Duration(s.cfg.Gateway.OpenAIWS.AstraHTTPBridgeHeartbeatSeconds) * time.Second
	}
	return 15 * time.Second
}

// Only the blocking upstream read runs in a worker. The caller owns ALL Gin
// writer access, including comments, semantic events and terminal errors.
// Do not implement heartbeats by repeatedly canceling WS reads: canceling a
// read can close the upstream connection and discard the in-flight generation.
func readOpenAIWSWithHeartbeat(ctx context.Context, timeout time.Duration, ticks <-chan time.Time, read func(context.Context, time.Duration) ([]byte, error), heartbeat func() bool) ([]byte, error) {
	if ticks == nil || heartbeat == nil {
		return read(ctx, timeout)
	}
	type result struct {
		message []byte
		err     error
	}
	readDone := make(chan result, 1)
	readCtx, cancelRead := context.WithCancel(ctx)
	defer cancelRead()
	go func() {
		message, err := read(readCtx, timeout)
		readDone <- result{message: message, err: err}
	}()
	for {
		select {
		case got := <-readDone:
			return got.message, got.err
		case <-ticks:
			if !heartbeat() {
				// A failed downstream write need not cancel Request.Context.
				// Stop this in-flight read rather than hold its slot for an hour.
				cancelRead()
				got := <-readDone
				return got.message, got.err
			}
		}
	}
}
