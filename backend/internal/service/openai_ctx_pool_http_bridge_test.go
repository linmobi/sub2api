package service

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestCtxPoolHTTPBridge_AllModelsKeysAndStreamModes(t *testing.T) {
	for _, model := range []string{"gpt-6-astra", "gpt-6.1-sol", "gpt-6-luna", "future-model"} {
		for _, keyID := range []int64{11, 12} {
			for _, stream := range []bool{true, false} {
				t.Run(fmt.Sprintf("%s/key%d/stream%t", model, keyID, stream), func(t *testing.T) {
					conn := &openAIWSCaptureConn{events: [][]byte{[]byte(`{"type":"response.completed","response":{"id":"resp_generic","object":"response","model":"` + model + `","status":"completed","output":[],"usage":{"input_tokens":3,"output_tokens":2}}}`)}}
					svc, account, c, rec, dialer := astraHTTPBridgeFixture(t, conn)
					svc.cfg.Gateway.OpenAIWS.CtxPoolHTTPBridgeEnabled = true
					svc.cfg.Gateway.OpenAIWS.AstraHTTPBridgeEnabled = false
					svc.cfg.Gateway.OpenAIWS.AstraHTTPBridgeAPIKeyIDs = []int64{999}
					delete(account.Extra, "openai_astra_http_ws_bridge_enabled")
					account.Extra["openai_oauth_responses_websockets_v2_mode"] = "ctx_pool"
					c.Set("api_key", &APIKey{ID: keyID})
					result, err := svc.Forward(context.Background(), c, account, []byte(fmt.Sprintf(`{"model":%q,"stream":%t,"input":"synthetic full input"}`, model, stream)))
					require.NoError(t, err)
					require.True(t, result.OpenAIWSMode)
					require.Equal(t, 1, dialer.DialCount())
					httpClient, ok := svc.httpUpstream.(*httpUpstreamRecorder)
					require.True(t, ok)
					require.Nil(t, httpClient.lastReq)
					require.True(t, conn.closed, "each HTTP turn must discard its private upstream connection")
					require.Equal(t, model, conn.lastWrite["model"])
					store, ok := conn.lastWrite["store"].(bool)
					require.True(t, ok)
					require.False(t, store)
					if stream {
						require.Contains(t, rec.Header().Get("Content-Type"), "text/event-stream")
						require.Equal(t, 1, strings.Count(rec.Body.String(), `"type":"response.completed"`))
					} else {
						require.Contains(t, rec.Header().Get("Content-Type"), "application/json")
						require.True(t, gjson.Valid(rec.Body.String()), "non-stream responses must be JSON, not SSE")
						require.Equal(t, "completed", gjson.Get(rec.Body.String(), "status").String())
					}
				})
			}
		}
	}
}

func TestCtxPoolHTTPBridge_SafetyGatesAndContinuationFallback(t *testing.T) {
	for _, tc := range []struct {
		name   string
		body   string
		mutate func(*OpenAIGatewayService, *Account)
	}{
		{"mode_off", `{"model":"gpt-6.1-sol","input":"hello"}`, func(_ *OpenAIGatewayService, a *Account) {
			a.Extra["openai_oauth_responses_websockets_v2_mode"] = "off"
		}},
		{"disabled", `{"model":"gpt-6.1-sol","input":"hello"}`, func(s *OpenAIGatewayService, _ *Account) { s.cfg.Gateway.OpenAIWS.CtxPoolHTTPBridgeEnabled = false }},
		{"force_http", `{"model":"gpt-6.1-sol","input":"hello"}`, func(s *OpenAIGatewayService, _ *Account) { s.cfg.Gateway.OpenAIWS.ForceHTTP = true }},
		{"previous_response", `{"model":"gpt-6.1-sol","previous_response_id":"resp_prior","input":"hello"}`, nil},
		{"conversation", `{"model":"gpt-6.1-sol","conversation":"conv_prior","input":"hello"}`, nil},
		{"item_reference", `{"model":"gpt-6.1-sol","input":[{"type":"item_reference","id":"item_prior"}]}`, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, account, c, _, dialer := astraHTTPBridgeFixture(t, &openAIWSCaptureConn{})
			svc.cfg.Gateway.OpenAIWS.CtxPoolHTTPBridgeEnabled = true
			svc.cfg.Gateway.OpenAIWS.AstraHTTPBridgeEnabled = false
			account.Extra["openai_oauth_responses_websockets_v2_mode"] = "ctx_pool"
			if tc.mutate != nil {
				tc.mutate(svc, account)
			}
			httpClient, ok := svc.httpUpstream.(*httpUpstreamRecorder)
			require.True(t, ok)
			httpClient.resp = &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_http\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n"))}
			result, err := svc.Forward(context.Background(), c, account, []byte(tc.body))
			require.NoError(t, err)
			require.False(t, result.OpenAIWSMode)
			require.NotNil(t, httpClient.lastReq)
			require.Zero(t, dialer.DialCount())
		})
	}
}

func TestCtxPoolHTTPBridge_MappedModelAndInterruptedStream(t *testing.T) {
	conn := &openAIWSCaptureConn{events: [][]byte{[]byte(`{"type":"response.created","response":{"id":"resp_interrupted"}}`)}}
	svc, account, c, rec, dialer := astraHTTPBridgeFixture(t, conn)
	svc.cfg.Gateway.OpenAIWS.CtxPoolHTTPBridgeEnabled = true
	svc.cfg.Gateway.OpenAIWS.AstraHTTPBridgeEnabled = false
	delete(account.Extra, "openai_astra_http_ws_bridge_enabled")
	account.Extra["openai_oauth_responses_websockets_v2_mode"] = "ctx_pool"
	account.Credentials["model_mapping"] = map[string]any{"client-alias": "gpt-6.1-sol"}
	_, err := svc.Forward(context.Background(), c, account, []byte(`{"model":"client-alias","stream":true,"input":"synthetic"}`))
	require.Error(t, err)
	require.Equal(t, "gpt-6.1-sol", conn.lastWrite["model"])
	require.Equal(t, 1, dialer.DialCount())
	httpClient, ok := svc.httpUpstream.(*httpUpstreamRecorder)
	require.True(t, ok)
	require.Nil(t, httpClient.lastReq, "accepted generation must never be invisibly replayed over HTTP")
	require.Equal(t, http.StatusBadGateway, rec.Code)
	require.True(t, gjson.Valid(rec.Body.String()))
	require.Equal(t, "upstream_error", gjson.Get(rec.Body.String(), "error.type").String())
	require.NotContains(t, rec.Body.String(), `"type":"response.completed"`)
	require.True(t, conn.closed)
}

func TestCtxPoolHTTPBridge_PreservesSemanticServerErrorWithoutReplay(t *testing.T) {
	conn := &openAIWSCaptureConn{events: [][]byte{[]byte(`{"type":"error","error":{"code":"server_error","type":"server_error","message":"synthetic upstream processing failure"}}`)}}
	svc, account, c, rec, dialer := astraHTTPBridgeFixture(t, conn)
	svc.cfg.Gateway.OpenAIWS.CtxPoolHTTPBridgeEnabled = true
	svc.cfg.Gateway.OpenAIWS.AstraHTTPBridgeEnabled = false
	account.Extra["openai_oauth_responses_websockets_v2_mode"] = "ctx_pool"
	_, err := svc.Forward(context.Background(), c, account, []byte(`{"model":"gpt-6.1-sol","stream":true,"input":"synthetic"}`))
	require.Error(t, err)
	require.Equal(t, 1, dialer.DialCount())
	httpClient, ok := svc.httpUpstream.(*httpUpstreamRecorder)
	require.True(t, ok)
	require.Nil(t, httpClient.lastReq)
	require.Equal(t, http.StatusBadGateway, rec.Code)
	require.True(t, gjson.Valid(rec.Body.String()))
	require.Contains(t, rec.Body.String(), `"code":"server_error"`)
	require.Contains(t, rec.Body.String(), "synthetic upstream processing failure")
	require.NotContains(t, rec.Body.String(), "upstream_stream_interrupted")
	require.Equal(t, "server_error", gjson.Get(rec.Body.String(), "error.type").String())
	require.True(t, conn.closed)
}

func TestChatCompletionsContextErrorPreservesBareAndTerminalErrorCodes(t *testing.T) {
	for _, source := range []string{
		`{"type":"error","error":{"code":"context_length_exceeded","type":"invalid_request_error","param":"input","message":"Your input exceeds the context window of this model."}}`,
		`{"type":"response.failed","response":{"error":{"code":"context_length_exceeded","type":"invalid_request_error","message":"Your input exceeds the context window of this model."}}}`,
	} {
		payload := []byte(source)
		message := "Your input exceeds the context window of this model."
		require.Equal(t, http.StatusBadRequest, openAIStreamFailureStatus(payload, message))
		status, errType := openAIChatSemanticErrorDefaults(payload, message)
		require.Equal(t, http.StatusBadRequest, status)
		fields := openAIChatErrorFields(errType, message, payload)
		require.Equal(t, "invalid_request_error", fields["type"])
		require.Equal(t, "context_length_exceeded", fields["code"])
		require.Equal(t, "input", fields["param"])
		require.False(t, openAIStreamErrorEventShouldFailover(payload, message))
	}
}
