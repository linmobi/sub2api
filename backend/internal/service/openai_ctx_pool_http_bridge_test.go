package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestCtxPoolHTTPBridge_AllModelsKeysAndStreamModes(t *testing.T) {
	for _, model := range []string{"gpt-6.1-sol", "gpt-6-luna", "future-model"} {
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

func TestCtxPoolHTTPBridge_AstraRequiresExplicitCanary(t *testing.T) {
	for _, tc := range []struct {
		name, model, mapped string
	}{
		{"astra", "gpt-6-astra", "gpt-6-astra"},
		{"dated_astra", "gpt-6-astra-2026-10-01", "gpt-6-astra-2026-10-01"},
		{"mapped_alias", "client-alias", "gpt-6-astra"},
	} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream%t", tc.name, stream), func(t *testing.T) {
				svc, account, c, _, dialer := astraHTTPBridgeFixture(t, &openAIWSCaptureConn{})
				svc.cfg.Gateway.OpenAIWS.CtxPoolHTTPBridgeEnabled = true
				svc.cfg.Gateway.OpenAIWS.AstraHTTPBridgeEnabled = false
				account.Extra["openai_oauth_responses_websockets_v2_mode"] = "ctx_pool"
				account.Credentials["model_mapping"] = map[string]any{tc.model: tc.mapped}
				httpClient, ok := svc.httpUpstream.(*httpUpstreamRecorder)
				require.True(t, ok)
				httpClient.resp = &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(`data: {"type":"response.completed","response":{"id":"resp_native_astra","object":"response","status":"completed","model":"` + tc.mapped + `","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"synthetic answer"}]}],"usage":{"input_tokens":3,"output_tokens":2}}}` + "\n\n"))}
				body := []byte(fmt.Sprintf(`{"model":%q,"stream":%t,"reasoning":{"effort":"xhigh","mode":"pro"},"service_tier":"default","input":[{"role":"user","content":"synthetic history"}]}`, tc.model, stream))
				result, err := svc.Forward(context.Background(), c, account, body)
				require.NoError(t, err)
				require.NotNil(t, result)
				require.False(t, result.OpenAIWSMode)
				require.Zero(t, dialer.DialCount(), "automatic ctx_pool bridging must not override Astra's disabled canary")
				require.Len(t, httpClient.requests, 1)
				require.Equal(t, "https://chatgpt.com/backend-api/codex/responses", httpClient.lastReq.URL.String())
				require.Equal(t, tc.mapped, gjson.GetBytes(httpClient.lastBody, "model").String())
				require.Equal(t, "xhigh", gjson.GetBytes(httpClient.lastBody, "reasoning.effort").String())
				require.Equal(t, "pro", gjson.GetBytes(httpClient.lastBody, "reasoning.mode").String())
				require.Equal(t, "default", gjson.GetBytes(httpClient.lastBody, "service_tier").String())
				require.Contains(t, string(httpClient.lastBody), "synthetic history")
			})
		}
	}
}

func TestCtxPoolHTTPBridge_AstraCanaryAndNativeWSRouting(t *testing.T) {
	for _, tc := range []struct {
		name          string
		canary, optIn bool
		allowedKey    int64
		wantBridge    bool
	}{
		{"explicit_canary", true, true, 11, true},
		{"noncanary_key", true, true, 12, false},
		{"account_opt_out", true, false, 11, false},
		{"global_disabled", false, true, 11, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, account, c, _, _ := astraHTTPBridgeFixture(t, &openAIWSCaptureConn{})
			svc.cfg.Gateway.OpenAIWS.CtxPoolHTTPBridgeEnabled = true
			svc.cfg.Gateway.OpenAIWS.AstraHTTPBridgeEnabled = tc.canary
			svc.cfg.Gateway.OpenAIWS.AstraHTTPBridgeAPIKeyIDs = []int64{tc.allowedKey}
			account.Extra["openai_oauth_responses_websockets_v2_mode"] = "ctx_pool"
			account.Extra["openai_astra_http_ws_bridge_enabled"] = tc.optIn
			decision := svc.getOpenAIWSProtocolResolver().Resolve(account)
			require.Equal(t, OpenAIUpstreamTransportResponsesWebsocketV2, decision.Transport)
			body := []byte(`{"model":"gpt-6-astra","stream":true,"input":"synthetic"}`)
			require.Equal(t, tc.wantBridge, svc.shouldBridgeAstraHTTP(c, account, body, decision))
			SetOpenAIClientTransport(c, OpenAIClientTransportWS)
			require.False(t, svc.shouldBridgeAstraHTTP(c, account, body, decision))
			require.Equal(t, decision, resolveOpenAIWSDecisionByClientTransport(decision, GetOpenAIClientTransport(c)), "native WS ingress must retain its selected transport")
			require.Equal(t, OpenAIUpstreamTransportHTTPSSE, resolveOpenAIWSDecisionByClientTransport(decision, OpenAIClientTransportHTTP).Transport)
		})
	}
}

func TestAstraNativeHTTP_AcceptedGenerationNeverProposesReplay(t *testing.T) {
	for _, tc := range []struct{ name, events string }{
		{"created_then_eof", `data: {"type":"response.created","sequence_number":0,"response":{"id":"resp_accepted","object":"response","status":"in_progress"}}` + "\n\n"},
		{"tool_added_then_eof", `data: {"type":"response.output_item.added","sequence_number":0,"item":{"id":"ctc_synthetic","type":"custom_tool_call","status":"in_progress","name":"synthetic_custom","call_id":"ctc_synthetic","input":""}}` + "\n\n"},
		{"tool_delta_then_eof", `data: {"type":"response.custom_tool_call_input.delta","sequence_number":0,"item_id":"ctc_synthetic","output_index":0,"delta":"synthetic tool input"}` + "\n\n"},
		{"reasoning_only_then_eof", `data: {"type":"response.output_item.done","sequence_number":0,"item":{"id":"rs_synthetic","type":"reasoning","status":"completed","encrypted_content":"synthetic-opaque-checkpoint","summary":[]}}` + "\n\n"},
		{"empty_message_then_eof", `data: {"type":"response.output_item.added","sequence_number":0,"item":{"id":"msg_synthetic","type":"message","status":"in_progress","role":"assistant","content":[]}}` + "\n\n"},
		{"reasoning_delta_then_eof", `data: {"type":"response.reasoning_text.delta","sequence_number":0,"delta":"synthetic reasoning text"}` + "\n\n"},
		{"created_then_server_error", `data: {"type":"response.created","sequence_number":0,"response":{"id":"resp_accepted","object":"response","status":"in_progress"}}` + "\n\n" + `data: {"type":"response.failed","sequence_number":1,"response":{"id":"resp_accepted","status":"failed","error":{"code":"server_error","type":"server_error","status":503,"message":"synthetic rejection"}}}` + "\n\n"},
		{"known_zero_usage_then_error", `data: {"type":"response.in_progress","sequence_number":0,"response":{"usage":{"input_tokens":0,"output_tokens":0}}}` + "\n\n" + `data: {"type":"error","sequence_number":1,"error":{"code":"server_error","status":503,"message":"synthetic rejection"}}` + "\n\n"},
		{"known_usage_then_error", `data: {"type":"response.in_progress","sequence_number":0,"response":{"id":"resp_known_usage","usage":{"input_tokens":3,"output_tokens":2}}}` + "\n\n" + `data: {"type":"error","sequence_number":1,"error":{"code":"server_error","status":503,"message":"synthetic rejection"}}` + "\n\n"},
		{"empty_usage_then_error", `data: {"type":"response.in_progress","sequence_number":0,"response":{"usage":{}}}` + "\n\n" + `data: {"type":"error","sequence_number":1,"error":{"code":"server_error","status":503,"message":"synthetic rejection"}}` + "\n\n"},
		{"partial_usage_then_error", `data: {"type":"response.in_progress","sequence_number":0,"response":{"usage":{"input_tokens":3}}}` + "\n\n" + `data: {"type":"error","sequence_number":1,"error":{"code":"server_error","status":503,"message":"synthetic rejection"}}` + "\n\n"},
		{"string_usage_then_error", `data: {"type":"response.in_progress","sequence_number":0,"response":{"usage":{"input_tokens":"3","output_tokens":2}}}` + "\n\n" + `data: {"type":"error","sequence_number":1,"error":{"code":"server_error","status":503,"message":"synthetic rejection"}}` + "\n\n"},
		{"fractional_usage_then_error", `data: {"type":"response.in_progress","sequence_number":0,"response":{"usage":{"input_tokens":1.5,"output_tokens":2}}}` + "\n\n" + `data: {"type":"error","sequence_number":1,"error":{"code":"server_error","status":503,"message":"synthetic rejection"}}` + "\n\n"},
		{"negative_usage_then_error", `data: {"type":"response.in_progress","sequence_number":0,"response":{"usage":{"input_tokens":-1,"output_tokens":2}}}` + "\n\n" + `data: {"type":"error","sequence_number":1,"error":{"code":"server_error","status":503,"message":"synthetic rejection"}}` + "\n\n"},
	} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream%t", tc.name, stream), func(t *testing.T) {
				svc, account, c, rec, dialer := astraHTTPBridgeFixture(t, &openAIWSCaptureConn{})
				svc.cfg.Gateway.OpenAIWS.CtxPoolHTTPBridgeEnabled = false
				svc.cfg.Gateway.OpenAIWS.AstraHTTPBridgeEnabled = false
				account.Extra["openai_oauth_responses_websockets_v2_mode"] = "ctx_pool"
				httpClient, ok := svc.httpUpstream.(*httpUpstreamRecorder)
				require.True(t, ok)
				httpClient.resp = &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(tc.events))}
				result, err := svc.Forward(context.Background(), c, account, []byte(fmt.Sprintf(`{"model":"gpt-6-astra","stream":%t,"reasoning":{"effort":"xhigh"},"input":"synthetic"}`, stream)))
				require.Error(t, err)
				var failoverErr *UpstreamFailoverError
				require.False(t, errors.As(err, &failoverErr), "an accepted generation must not enter outer failover")
				require.Len(t, httpClient.requests, 1)
				require.Zero(t, dialer.DialCount())
				require.True(t, IsResponseCommitted(c), "the client must receive an explicit final failure")
				require.NotContains(t, rec.Body.String(), `"type":"response.completed"`)
				require.NotContains(t, rec.Body.String(), `"type":"response.done"`)
				if !strings.HasPrefix(tc.name, "known_") {
					require.Nil(t, result, "unknown usage must not be converted to a synthetic zero usage result")
					if stream && rec.Code == http.StatusOK {
						forEachOpenAISSEFrame(rec.Body.String(), func(eventType string, payload []byte) {
							if eventType == "response.failed" {
								require.False(t, gjson.GetBytes(payload, "response.usage").Exists())
							}
						})
					} else {
						require.NotContains(t, rec.Body.String(), `"usage"`)
					}
				} else {
					require.NotNil(t, result)
					require.Equal(t, stream, result.Stream)
					require.False(t, result.OpenAIWSMode)
					require.Equal(t, "gpt-6-astra", result.UpstreamModel)
					require.NotNil(t, result.ReasoningEffort)
					require.Equal(t, "xhigh", *result.ReasoningEffort)
					if tc.name == "known_zero_usage_then_error" {
						require.Zero(t, result.Usage.InputTokens)
						require.Zero(t, result.Usage.OutputTokens)
					} else {
						require.Equal(t, 3, result.Usage.InputTokens)
						require.Equal(t, 2, result.Usage.OutputTokens)
					}
				}
				if stream && rec.Code == http.StatusOK {
					require.Equal(t, 1, strings.Count(rec.Body.String(), `"type":"response.failed"`))
				} else {
					require.GreaterOrEqual(t, rec.Code, 400)
					require.True(t, gjson.Valid(rec.Body.String()))
					require.NotEmpty(t, gjson.Get(rec.Body.String(), "error.code").String())
				}
			})
		}
	}
}

func TestAstraNativeHTTP_ToolThenBareErrorHasOneSequencedFailure(t *testing.T) {
	for _, tc := range []struct {
		name, code string
		wantStatus int
	}{
		{"overload", "server_is_overloaded", http.StatusServiceUnavailable},
		{"context", "context_length_exceeded", http.StatusBadRequest},
	} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream%t", tc.name, stream), func(t *testing.T) {
				svc, account, c, rec, dialer := astraHTTPBridgeFixture(t, &openAIWSCaptureConn{})
				svc.cfg.Gateway.OpenAIWS.CtxPoolHTTPBridgeEnabled = true
				svc.cfg.Gateway.OpenAIWS.AstraHTTPBridgeEnabled = false
				account.Extra["openai_oauth_responses_websockets_v2_mode"] = "ctx_pool"
				httpClient, ok := svc.httpUpstream.(*httpUpstreamRecorder)
				require.True(t, ok)
				events := `data: {"type":"response.custom_tool_call_input.delta","sequence_number":41,"item_id":"ctc_synthetic","output_index":0,"delta":"synthetic tool input"}` + "\n\n" + fmt.Sprintf(`data: {"type":"error","sequence_number":42,"code":%q,"message":"synthetic rejection"}`, tc.code) + "\n\n"
				httpClient.resp = &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(events))}
				result, err := svc.Forward(context.Background(), c, account, []byte(fmt.Sprintf(`{"model":"gpt-6-astra","stream":%t,"input":"synthetic"}`, stream)))
				require.Error(t, err)
				require.Nil(t, result)
				var failoverErr *UpstreamFailoverError
				require.False(t, errors.As(err, &failoverErr))
				require.Len(t, httpClient.requests, 1)
				require.Zero(t, dialer.DialCount())
				require.True(t, IsResponseCommitted(c))
				if stream {
					count := 0
					forEachOpenAISSEFrame(rec.Body.String(), func(eventType string, payload []byte) {
						if eventType != "response.failed" {
							return
						}
						count++
						require.Equal(t, int64(43), gjson.GetBytes(payload, "sequence_number").Int())
						require.Equal(t, tc.code, gjson.GetBytes(payload, "response.error.code").String())
						require.Equal(t, "response", gjson.GetBytes(payload, "response.object").String())
						require.True(t, gjson.GetBytes(payload, "response.created_at").Exists())
						require.False(t, gjson.GetBytes(payload, "response.usage").Exists())
					})
					require.Equal(t, 1, count)
				} else {
					require.Equal(t, tc.wantStatus, rec.Code)
					require.Equal(t, tc.code, gjson.Get(rec.Body.String(), "error.code").String())
				}
			})
		}
	}
}

func TestAstraNativeHTTP_TerminalUsageReplacesProgressUsage(t *testing.T) {
	_, account, c, _, _ := astraHTTPBridgeFixture(t, &openAIWSCaptureConn{})
	prepareOpenAIAstraNativeHTTP(c, account, []byte(`{"model":"gpt-6-astra","stream":true}`), openAIWSHTTPDecision("client_protocol_http"))
	observeOpenAIAstraNativeHTTP(c, []byte(`{"type":"response.in_progress","response":{"usage":{"input_tokens":3,"output_tokens":2,"input_tokens_details":{"cached_tokens":1}}}}`), "response.in_progress")
	observeOpenAIAstraNativeHTTP(c, []byte(`{"type":"response.failed","response":{"id":"resp_terminal_usage","usage":{"input_tokens":5,"output_tokens":1},"error":{"code":"server_error","status":503}}}`), "response.failed")
	state := openAIAstraNativeHTTPStateFromContext(c)
	require.NotNil(t, state)
	require.True(t, state.usageKnown)
	require.Equal(t, 5, state.usage.InputTokens)
	require.Equal(t, 1, state.usage.OutputTokens)
	require.Zero(t, state.usage.CacheReadInputTokens, "terminal usage must replace earlier cache counts")
}

func TestAstraNativeHTTP_PreAcceptance503RetainsFailover(t *testing.T) {
	svc, account, c, _, dialer := astraHTTPBridgeFixture(t, &openAIWSCaptureConn{})
	svc.cfg.Gateway.OpenAIWS.CtxPoolHTTPBridgeEnabled = true
	svc.cfg.Gateway.OpenAIWS.AstraHTTPBridgeEnabled = false
	account.Extra["openai_oauth_responses_websockets_v2_mode"] = "ctx_pool"
	httpClient, ok := svc.httpUpstream.(*httpUpstreamRecorder)
	require.True(t, ok)
	httpClient.resp = &http.Response{StatusCode: http.StatusServiceUnavailable, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"error":{"type":"service_unavailable_error","code":"server_is_overloaded","message":"synthetic overload before generation"}}`))}
	_, err := svc.Forward(context.Background(), c, account, []byte(`{"model":"gpt-6-astra","stream":true,"input":"synthetic"}`))
	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)
	require.Equal(t, http.StatusServiceUnavailable, failoverErr.StatusCode)
	require.False(t, openAIAstraNativeHTTPAccepted(c))
	require.Zero(t, dialer.DialCount())
	require.Len(t, httpClient.requests, 1)
}

func TestAstraNativeHTTP_PreservesCustomToolHistoryAndIncompleteTerminal(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream%t", stream), func(t *testing.T) {
			svc, account, c, rec, dialer := astraHTTPBridgeFixture(t, &openAIWSCaptureConn{})
			svc.cfg.Gateway.OpenAIWS.CtxPoolHTTPBridgeEnabled = true
			svc.cfg.Gateway.OpenAIWS.AstraHTTPBridgeEnabled = false
			account.Extra["openai_oauth_responses_websockets_v2_mode"] = "ctx_pool"
			httpClient, ok := svc.httpUpstream.(*httpUpstreamRecorder)
			require.True(t, ok)
			httpClient.resp = &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(`data: {"type":"response.incomplete","sequence_number":0,"response":{"id":"resp_incomplete","object":"response","model":"gpt-6-astra","status":"incomplete","output":[],"incomplete_details":{"reason":"max_output_tokens"},"usage":{"input_tokens":3,"output_tokens":2}}}` + "\n\n"))}
			body := fmt.Sprintf(`{"model":"gpt-6-astra","stream":%t,"reasoning":{"effort":"max","mode":"pro"},"service_tier":"default","tools":[{"type":"namespace","name":"synthetic_tools","tools":[{"type":"custom","name":"inspect","description":"synthetic harmless tool","format":{"type":"text"}}]}],"input":[{"role":"user","content":"synthetic original history"},{"type":"custom_tool_call","id":"ctc_synthetic","call_id":"ctc_synthetic","namespace":"synthetic_tools","name":"inspect","input":"synthetic call input"},{"type":"custom_tool_call_output","call_id":"ctc_synthetic","output":"synthetic call result"}]}`, stream)
			result, err := svc.Forward(context.Background(), c, account, []byte(body))
			require.NoError(t, err, "legitimate incomplete responses must never be retried or treated as transport failures")
			require.NotNil(t, result)
			require.Len(t, httpClient.requests, 1)
			require.Zero(t, dialer.DialCount())
			require.Equal(t, "max", gjson.GetBytes(httpClient.lastBody, "reasoning.effort").String())
			require.Equal(t, "pro", gjson.GetBytes(httpClient.lastBody, "reasoning.mode").String())
			require.Equal(t, "default", gjson.GetBytes(httpClient.lastBody, "service_tier").String())
			require.Equal(t, "namespace", gjson.GetBytes(httpClient.lastBody, "tools.0.type").String())
			require.Equal(t, "synthetic_tools", gjson.GetBytes(httpClient.lastBody, "tools.0.name").String())
			require.Equal(t, "custom", gjson.GetBytes(httpClient.lastBody, "tools.0.tools.0.type").String())
			require.Equal(t, "synthetic original history", gjson.GetBytes(httpClient.lastBody, "input.0.content").String())
			require.Equal(t, "synthetic_tools", gjson.GetBytes(httpClient.lastBody, "input.1.namespace").String())
			require.Equal(t, "synthetic call input", gjson.GetBytes(httpClient.lastBody, "input.1.input").String())
			require.Equal(t, "synthetic call result", gjson.GetBytes(httpClient.lastBody, "input.2.output").String())
			require.Equal(t, gjson.GetBytes(httpClient.lastBody, "input.1.call_id").String(), gjson.GetBytes(httpClient.lastBody, "input.2.call_id").String())
			if stream {
				require.Contains(t, rec.Body.String(), `"type":"response.incomplete"`)
			} else {
				require.True(t, gjson.Valid(rec.Body.String()))
				require.Equal(t, "incomplete", gjson.Get(rec.Body.String(), "status").String())
				require.Equal(t, "max_output_tokens", gjson.Get(rec.Body.String(), "incomplete_details.reason").String())
			}
		})
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
