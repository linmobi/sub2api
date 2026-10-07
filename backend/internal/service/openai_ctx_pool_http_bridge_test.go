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
					require.Nil(t, svc.httpUpstream.(*httpUpstreamRecorder).lastReq)
					require.True(t, conn.closed, "each HTTP turn must discard its private upstream connection")
					require.Equal(t, model, conn.lastWrite["model"])
					require.False(t, conn.lastWrite["store"].(bool))
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
			httpClient := svc.httpUpstream.(*httpUpstreamRecorder)
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
	require.Nil(t, svc.httpUpstream.(*httpUpstreamRecorder).lastReq, "accepted generation must never be invisibly replayed over HTTP")
	require.Equal(t, http.StatusBadGateway, rec.Code)
	require.True(t, gjson.Valid(rec.Body.String()))
	require.Equal(t, "upstream_error", gjson.Get(rec.Body.String(), "error.type").String())
	require.NotContains(t, rec.Body.String(), `"type":"response.completed"`)
	require.True(t, conn.closed)
}
