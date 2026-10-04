package service

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
)

const openAIAstraHTTPBridgeContextKey = "openai_astra_http_ws_bridge"

type openAIAstraHTTPBridgeRequest struct {
	executionScope string
}

// Deliberately narrow experimental gate. Passthrough and other HTTP endpoints
// retain their existing protocol, even when a model name happens to match.
func (s *OpenAIGatewayService) shouldBridgeAstraHTTP(c *gin.Context, account *Account, body []byte, decision OpenAIWSProtocolDecision) bool {
	if s == nil || s.cfg == nil || !s.cfg.Gateway.OpenAIWS.AstraHTTPBridgeEnabled || c == nil || c.Request == nil ||
		GetOpenAIClientTransport(c) != OpenAIClientTransportHTTP || getAPIKeyIDFromContext(c) <= 0 ||
		account == nil || !account.IsOpenAIOAuth() || account.IsOpenAIPassthroughEnabled() ||
		!account.IsOpenAIResponsesWebSocketV2Enabled() || decision.Transport != OpenAIUpstreamTransportResponsesWebsocketV2 ||
		isOpenAIResponsesCompactPath(c) || !strings.HasSuffix(strings.TrimRight(c.Request.URL.Path, "/"), "/responses") {
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

func prepareOpenAIAstraHTTPBridge(c *gin.Context, account *Account, body []byte, rawScope string) (string, error) {
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
	if rejectedParam != "" {
		err := fmt.Errorf("experimental Astra HTTP bridge requires full input; %s is unsupported", rejectedParam)
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"type": "invalid_request_error", "message": err.Error(), "param": rejectedParam}})
		MarkResponseCommitted(c)
		return "", err
	}
	// Every experiment is a fresh, full-input turn. Include both downstream and
	// upstream identity even for clients that supply no reliable thread/session.
	scope, _ := deriveOpenAISessionHashes(fmt.Sprintf("astra_http_ws:%d:%d:%s:%s", getAPIKeyIDFromContext(c), account.ID, rawScope, uuid.NewString()))
	c.Set(openAIAstraHTTPBridgeContextKey, openAIAstraHTTPBridgeRequest{executionScope: scope})
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
