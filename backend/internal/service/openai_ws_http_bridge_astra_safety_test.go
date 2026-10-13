package service

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func runAstraWSHTTPBridgeFixture(t *testing.T, ctx context.Context, response *http.Response, payload []byte, write func([]byte) error, configure ...func(*Account)) (*OpenAIForwardResult, *httpUpstreamRecorder, [][]byte, error) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	upstream := &httpUpstreamRecorder{resp: response}
	svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
	account := &Account{ID: 91, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Concurrency: 1}
	for _, set := range configure {
		set(account)
	}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil).WithContext(ctx)
	var writes [][]byte
	result, err := svc.proxyOpenAIWSHTTPBridgeTurn(ctx, c, account, "synthetic-token", payload, len(payload),
		"gpt-6-astra", "", "", "", "", 1, func(message []byte) error {
			if write != nil {
				if writeErr := write(message); writeErr != nil {
					return writeErr
				}
			}
			writes = append(writes, append([]byte(nil), message...))
			return nil
		})
	return result, upstream, writes, err
}

func astraWSHTTPFixtureResponse(sse string, readError error) *http.Response {
	var reader io.Reader = strings.NewReader(sse)
	if readError != nil {
		reader = io.MultiReader(reader, iotest.ErrReader(readError))
	}
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(reader)}
}

func astraWSHTTPFixturePayload() []byte {
	return []byte(`{"type":"response.create","model":"gpt-6-astra","store":false,"instructions":"Synthetic protocol fixture","reasoning":{"effort":"high"},"service_tier":"default","input":[{"role":"user","content":"Synthetic finite task"}]}`)
}

func requireAstraWSHTTPFailedTerminal(t *testing.T, writes [][]byte, sequence int64, code string) {
	t.Helper()
	var failures [][]byte
	for _, message := range writes {
		require.True(t, gjson.ValidBytes(message), "each client message must contain one complete JSON document")
		if gjson.GetBytes(message, "type").String() == "response.failed" {
			failures = append(failures, message)
		}
	}
	require.Len(t, failures, 1)
	terminal := failures[0]
	require.Equal(t, sequence, gjson.GetBytes(terminal, "sequence_number").Int())
	require.NotEmpty(t, gjson.GetBytes(terminal, "response.id").String())
	require.Equal(t, "response", gjson.GetBytes(terminal, "response.object").String())
	require.Positive(t, gjson.GetBytes(terminal, "response.created_at").Int())
	require.Equal(t, "failed", gjson.GetBytes(terminal, "response.status").String())
	require.Equal(t, code, gjson.GetBytes(terminal, "response.error.code").String())
	require.False(t, gjson.GetBytes(terminal, "response.usage").Exists())
}

func TestAstraWSHTTPBridgeAcceptedProgressNeverFailsOver(t *testing.T) {
	progress := []struct{ name, event string }{
		{"created", `{"type":"response.created","sequence_number":4,"response":{"id":"resp_accepted"}}`},
		{"in_progress", `{"type":"response.in_progress","sequence_number":4,"response":{"id":"resp_accepted"}}`},
		{"opaque_reasoning", `{"type":"response.output_item.done","sequence_number":4,"item":{"type":"reasoning","id":"rs_synthetic","encrypted_content":"synthetic-opaque"}}`},
		{"empty_message", `{"type":"response.output_item.added","sequence_number":4,"item":{"type":"message","role":"assistant","content":[]}}`},
		{"custom_tool_start", `{"type":"response.output_item.added","sequence_number":4,"item":{"type":"custom_tool_call","call_id":"call_fixture","name":"shell","input":""}}`},
		{"custom_tool_input", `{"type":"response.custom_tool_call_input.delta","sequence_number":4,"delta":"synthetic argument"}`},
		{"reasoning_delta", `{"type":"response.reasoning_summary_text.delta","sequence_number":4,"delta":"synthetic"}`},
	}
	for _, tt := range progress {
		for _, readError := range []error{nil, io.ErrUnexpectedEOF} {
			t.Run(fmt.Sprintf("%s/read_error_%v", tt.name, readError != nil), func(t *testing.T) {
				result, upstream, writes, err := runAstraWSHTTPBridgeFixture(t, context.Background(),
					astraWSHTTPFixtureResponse("data: "+tt.event+"\n\n", readError), astraWSHTTPFixturePayload(), nil)
				require.Error(t, err)
				var failover *UpstreamFailoverError
				require.False(t, errors.As(err, &failover), "accepted generation must not be replayed by outer account failover")
				require.Nil(t, result, "failure without valid paired token evidence must remain unknown")
				require.Len(t, upstream.requests, 1)
				requireAstraWSHTTPFailedTerminal(t, writes, 5, "upstream_stream_interrupted")
			})
		}
	}
}

func TestAstraWSHTTPBridgeFailureUsageEvidence(t *testing.T) {
	for _, tt := range []struct {
		name, usage   string
		known         bool
		input, output int
	}{
		{"empty", `{}`, false, 0, 0},
		{"missing_output", `{"input_tokens":8}`, false, 0, 0},
		{"string", `{"input_tokens":"8","output_tokens":2}`, false, 0, 0},
		{"fraction", `{"input_tokens":8,"output_tokens":2.5}`, false, 0, 0},
		{"negative", `{"input_tokens":-1,"output_tokens":2}`, false, 0, 0},
		{"genuine_zero", `{"input_tokens":0,"output_tokens":0}`, true, 0, 0},
		{"known_partial", `{"input_tokens":8,"output_tokens":2}`, true, 8, 2},
	} {
		t.Run(tt.name, func(t *testing.T) {
			sse := `data: {"type":"response.in_progress","sequence_number":6,"response":{"id":"resp_usage","usage":` + tt.usage + `}}` + "\n\n"
			result, upstream, writes, err := runAstraWSHTTPBridgeFixture(t, context.Background(), astraWSHTTPFixtureResponse(sse, nil), astraWSHTTPFixturePayload(), nil)
			require.Error(t, err)
			var failover *UpstreamFailoverError
			require.False(t, errors.As(err, &failover))
			require.Len(t, upstream.requests, 1)
			if tt.known {
				state := &openAIAstraNativeHTTPState{lastSequence: -1}
				observeOpenAIAstraHTTPProtocol(state, []byte(`{"type":"response.in_progress","response":{"usage":`+tt.usage+`}}`), "response.in_progress")
				require.True(t, state.usageKnown, "genuine numeric 0/0 remains distinguishable diagnostic evidence")
				require.Equal(t, tt.input, state.usage.InputTokens)
				require.Equal(t, tt.output, state.usage.OutputTokens)
			}
			require.Nil(t, result, "failed WS attempt must not be turned into a success or ledger result")
			requireAstraWSHTTPFailedTerminal(t, writes, 7, "upstream_stream_interrupted")
		})
	}
}

func TestAstraWSHTTPBridgeAcceptedSemanticFailure(t *testing.T) {
	for _, tt := range []struct{ name, event, code string }{
		{"server_error", `{"type":"response.failed","sequence_number":9,"response":{"id":"resp_semantic","object":"response","created_at":100,"status":"failed","error":{"code":"server_error","message":"Synthetic upstream failure"}}}`, "server_error"},
		{"bare_context", `{"type":"error","sequence_number":9,"code":"context_length_exceeded","message":"maximum context length exceeded"}`, "context_length_exceeded"},
		{"bare_overload", `{"type":"error","sequence_number":9,"code":"server_is_overloaded","message":"Synthetic upstream overload"}`, "server_error"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			sse := "data: " + `{"type":"response.created","sequence_number":8,"response":{"id":"resp_semantic"}}` + "\n\ndata: " + tt.event + "\n\n"
			result, upstream, writes, err := runAstraWSHTTPBridgeFixture(t, context.Background(), astraWSHTTPFixtureResponse(sse, nil), astraWSHTTPFixturePayload(), nil)
			require.Error(t, err, "response.failed is not a successful zero-usage generation")
			var failover *UpstreamFailoverError
			require.False(t, errors.As(err, &failover))
			require.Nil(t, result)
			require.Len(t, upstream.requests, 1)
			sequence := int64(10)
			if tt.name == "server_error" {
				sequence = 9
			}
			requireAstraWSHTTPFailedTerminal(t, writes, sequence, tt.code)
		})
	}
}

func TestAstraWSHTTPBridgePreacceptanceHTTP503CanFailOver(t *testing.T) {
	result, upstream, writes, err := runAstraWSHTTPBridgeFixture(t, context.Background(), &http.Response{
		StatusCode: http.StatusServiceUnavailable, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"error":{"code":"server_is_overloaded","message":"Synthetic unavailable"}}`)),
	}, astraWSHTTPFixturePayload(), nil)
	var failover *UpstreamFailoverError
	require.ErrorAs(t, err, &failover)
	require.Equal(t, http.StatusServiceUnavailable, failover.StatusCode)
	require.Nil(t, result)
	require.Empty(t, writes)
	require.Len(t, upstream.requests, 1)
}

func TestAstraWSHTTPBridgeSuccessStatusWithoutFrameNeverReplays(t *testing.T) {
	for _, tt := range []struct {
		name, sse string
		readError error
	}{
		{"empty_200", "", nil},
		{"empty_200_read_error", "", io.ErrUnexpectedEOF},
		{"truncated_first_frame", `data: {"type":"response.created","response":{"id":"resp_partial"`, nil},
		{"uncommitted_first_frame_read_error", `data: {"type":"response.created","response":{"id":"resp_partial"}}` + "\n", io.ErrUnexpectedEOF},
	} {
		t.Run(tt.name, func(t *testing.T) {
			result, upstream, writes, err := runAstraWSHTTPBridgeFixture(t, context.Background(), astraWSHTTPFixtureResponse(tt.sse, tt.readError), astraWSHTTPFixturePayload(), nil)
			require.Error(t, err)
			var failover *UpstreamFailoverError
			require.False(t, errors.As(err, &failover), "HTTP200 with missing protocol evidence cannot safely prove generation did not start")
			require.Nil(t, result)
			require.Len(t, upstream.requests, 1)
			requireAstraWSHTTPFailedTerminal(t, writes, 0, "upstream_stream_interrupted")
		})
	}
}

func TestAstraWSHTTPBridgeSSEWholeFrameValidation(t *testing.T) {
	created := `{"type":"response.created","sequence_number":0,"response":{"id":"resp_framing"}}`
	tool := `{"type":"response.custom_tool_call_input.delta","sequence_number":1,"delta":"raw\\nargument\\\"value"}`
	completed := `{"type":"response.completed","sequence_number":2,"response":{"id":"resp_framing","status":"completed","usage":{"input_tokens":1,"output_tokens":1}}}`
	for _, tt := range []struct{ name, sse string }{
		{"multiline_with_event_type", "event: response.created\ndata: {\"sequence_number\":0,\ndata: \"response\":{\"id\":\"resp_framing\"}}\n\n" + "data: " + tool + "\n\ndata: " + completed + "\n\n"},
		{"concatenated_documents", "data: " + created + tool + "\n\ndata: " + completed + "\n\n"},
		{"multiple_data_documents", "data: " + created + "\ndata: " + tool + "\n\ndata: " + completed + "\n\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			result, _, writes, err := runAstraWSHTTPBridgeFixture(t, context.Background(), astraWSHTTPFixtureResponse(tt.sse, nil), astraWSHTTPFixturePayload(), nil)
			require.NoError(t, err)
			require.NotNil(t, result)
			require.Len(t, writes, 3)
			for _, message := range writes {
				require.True(t, gjson.ValidBytes(message))
			}
			require.Equal(t, tool, string(writes[1]), "tool argument payload must remain byte-for-byte intact")
		})
	}
	for _, tail := range []string{`{"type":"response.completed"`, `{"type":"response.completed"}garbage`, strings.Repeat(tool, maxOpenAIConcatenatedJSONDocuments)} {
		t.Run(fmt.Sprintf("malformed_or_over_limit_%d", len(tail)), func(t *testing.T) {
			sse := "data: " + created + "\n\ndata: " + tool + tail + "\n\n"
			result, _, writes, err := runAstraWSHTTPBridgeFixture(t, context.Background(), astraWSHTTPFixtureResponse(sse, nil), astraWSHTTPFixturePayload(), nil)
			require.Error(t, err)
			require.Nil(t, result)
			for _, message := range writes {
				require.NotEqual(t, "response.custom_tool_call_input.delta", gjson.GetBytes(message, "type").String(), "invalid whole frame must not leak its valid prefix")
			}
			requireAstraWSHTTPFailedTerminal(t, writes, 1, "upstream_stream_interrupted")
		})
	}
}

func TestAstraWSHTTPBridgePreservesHistoryToolsModelEffortTier(t *testing.T) {
	for _, effort := range []string{"high", "xhigh", "max"} {
		t.Run(effort, func(t *testing.T) {
			payload := []byte(`{"type":"response.create","model":"gpt-6-astra","store":false,"reasoning":{"effort":"` + effort + `","mode":"thinking"},"service_tier":"default","instructions":"Synthetic","tools":[{"type":"namespace","name":"functions","tools":[{"type":"custom","name":"shell","format":{"type":"text"}}]}],"input":[{"role":"user","content":"Synthetic history"},{"type":"custom_tool_call","call_id":"call_keep","name":"functions.shell","input":"keep exact argument"},{"type":"custom_tool_call_output","call_id":"call_keep","output":"keep exact output"},{"role":"user","content":"Synthetic followup"}]}`)
			completed := `data: {"type":"response.completed","sequence_number":0,"response":{"id":"resp_preserve","status":"completed","usage":{"input_tokens":1,"output_tokens":1}}}` + "\n\n"
			result, upstream, _, err := runAstraWSHTTPBridgeFixture(t, context.Background(), astraWSHTTPFixtureResponse(completed, nil), payload, nil)
			require.NoError(t, err)
			require.NotNil(t, result)
			require.Len(t, upstream.bodies, 1)
			body := upstream.bodies[0]
			for _, path := range []string{"model", "reasoning", "service_tier", "instructions", "tools", "input"} {
				require.JSONEq(t, gjson.GetBytes(payload, path).Raw, gjson.GetBytes(body, path).Raw, path)
			}
			require.False(t, gjson.GetBytes(body, "type").Exists())
			require.True(t, gjson.GetBytes(body, "stream").Bool())
		})
	}
}

func TestAstraWSHTTPBridgeCanceledClientSkipsSyntheticFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	sse := "data: " + `{"type":"response.output_text.delta","sequence_number":0,"delta":"synthetic visible"}` + "\n\n"
	result, upstream, writes, err := runAstraWSHTTPBridgeFixture(t, ctx, astraWSHTTPFixtureResponse(sse, nil), astraWSHTTPFixturePayload(), func([]byte) error { cancel(); return context.Canceled })
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, result)
	require.Len(t, upstream.requests, 1)
	require.Empty(t, writes)
}

func TestAstraNativeHTTP_SuccessWithoutProtocolEvidenceDoesNotReplay(t *testing.T) {
	for _, tt := range []struct {
		name, sse string
		readError error
	}{
		{"empty_200", "", nil},
		{"empty_read_failure", "", io.ErrUnexpectedEOF},
		{"comments_only", ": synthetic heartbeat\n\n", nil},
		{"truncated_first_frame", `data: {"type":"response.created","response":{"id":"resp_partial"`, nil},
		{"truncated_read_failure", `data: {"type":"response.created","response":{"id":"resp_partial"`, io.ErrUnexpectedEOF},
	} {
		for _, stream := range []bool{true, false} {
			for _, model := range []string{"gpt-6-astra", "astra-alias"} {
				t.Run(fmt.Sprintf("%s/%s/stream_%v", tt.name, model, stream), func(t *testing.T) {
					svc, account, c, rec, dialer := astraHTTPBridgeFixture(t, &openAIWSCaptureConn{})
					svc.cfg.Gateway.OpenAIWS.CtxPoolHTTPBridgeEnabled = false
					svc.cfg.Gateway.OpenAIWS.AstraHTTPBridgeEnabled = false
					account.Credentials["model_mapping"] = map[string]any{"astra-alias": "gpt-6-astra"}
					upstream, ok := svc.httpUpstream.(*httpUpstreamRecorder)
					require.True(t, ok)
					upstream.resp = astraWSHTTPFixtureResponse(tt.sse, tt.readError)
					result, err := svc.Forward(context.Background(), c, account, []byte(fmt.Sprintf(`{"model":%q,"stream":%v,"input":"Synthetic finite request"}`, model, stream)))
					require.Error(t, err)
					var failover *UpstreamFailoverError
					require.False(t, errors.As(err, &failover), "successful HTTP status alone cannot authorize another generation")
					require.False(t, openAIAstraNativeHTTPAccepted(c), "HTTP success is not proof of a complete protocol event")
					require.Nil(t, result)
					require.Len(t, upstream.requests, 1)
					require.Zero(t, dialer.DialCount())
					require.True(t, IsResponseCommitted(c))
					if rec.Code == http.StatusOK {
						count := 0
						forEachOpenAISSEFrame(rec.Body.String(), func(eventType string, message []byte) {
							if eventType == "response.failed" {
								count++
								require.False(t, gjson.GetBytes(message, "response.usage").Exists())
							}
						})
						require.Equal(t, 1, count)
					} else {
						require.Equal(t, http.StatusBadGateway, rec.Code)
						require.Equal(t, "upstream_stream_interrupted", gjson.Get(rec.Body.String(), "error.code").String())
					}
				})
			}
		}
	}
}

func TestAstraWSHTTPBridgeLegalIncompleteIsTerminal(t *testing.T) {
	sse := "data: " + `{"type":"response.created","sequence_number":0,"response":{"id":"resp_incomplete"}}` + "\n\n" +
		"data: " + `{"type":"error","sequence_number":1,"code":"server_error","message":"Synthetic recoverable warning"}` + "\n\n" +
		"data: " + `{"type":"response.incomplete","sequence_number":2,"response":{"id":"resp_incomplete","status":"incomplete","output":[],"incomplete_details":{"reason":"max_output_tokens"},"usage":{"input_tokens":1,"output_tokens":2}}}` + "\n\n"
	result, upstream, writes, err := runAstraWSHTTPBridgeFixture(t, context.Background(), astraWSHTTPFixtureResponse(sse, nil), astraWSHTTPFixturePayload(), nil)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Len(t, upstream.requests, 1)
	require.Equal(t, "response.incomplete", gjson.GetBytes(writes[len(writes)-1], "type").String())
	for _, message := range writes {
		require.NotEqual(t, "response.failed", gjson.GetBytes(message, "type").String())
	}
}

func TestAstraWSHTTPBridgeEventScannerBoundsWholeFrame(t *testing.T) {
	physical := bufio.NewScanner(strings.NewReader("data: {\"type\":\"response.output_text.delta\",\ndata: \"delta\":\"" + strings.Repeat("x", 100) + "\"}\n\n"))
	physical.Buffer(make([]byte, 128), 1024)
	scanner := &openAIWSHTTPBridgeEventScanner{scanner: physical, maxBytes: 64}
	require.False(t, scanner.Scan())
	require.ErrorIs(t, scanner.Err(), errOpenAIWSHTTPBridgeSSEFrameLimit)
	require.Empty(t, scanner.pending)
	require.Equal(t, "frame_limit", openAIWSHTTPBridgeReadErrorKind(scanner.Err()))
}

func TestAstraWSHTTPBridgeTypedMetadataContainsNoPayload(t *testing.T) {
	for _, tt := range []struct {
		name, events            string
		readError               error
		cancel                  bool
		wantAccepted, wantKnown bool
		kind, readKind          string
	}{
		{"empty_200", "", nil, false, false, false, "missing_terminal", "eof"},
		{"accepted_read", "data: " + `{"type":"response.created","sequence_number":0,"response":{"id":"resp_metadata"}}` + "\n\n", errors.New("sensitive arbitrary upstream text"), false, true, false, "stream_read", "read_error"},
		{"known_zero", "data: " + `{"type":"response.in_progress","sequence_number":0,"response":{"usage":{"input_tokens":0,"output_tokens":0}}}` + "\n\n", nil, false, true, true, "missing_terminal", "eof"},
		{"empty_usage", "data: " + `{"type":"response.in_progress","sequence_number":0,"response":{"usage":{}}}` + "\n\n", nil, false, true, false, "missing_terminal", "eof"},
		{"cancel", "data: " + `{"type":"response.output_text.delta","sequence_number":0,"delta":"sensitive synthetic answer"}` + "\n\n", nil, true, true, false, "", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			sink, release := captureStructuredLog(t)
			defer release()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var write func([]byte) error
			if tt.cancel {
				write = func([]byte) error { cancel(); return context.Canceled }
			}
			result, _, _, err := runAstraWSHTTPBridgeFixture(t, ctx, astraWSHTTPFixtureResponse(tt.events, tt.readError), astraWSHTTPFixturePayload(), write)
			require.Error(t, err)
			require.Nil(t, result)
			sink.mu.Lock()
			defer sink.mu.Unlock()
			failed, entered := 0, 0
			for _, event := range sink.events {
				if event.Message == "openai.ws_http_bridge_enter" {
					entered++
					require.Equal(t, "http_sse", event.Fields["upstream_transport"])
				}
				if event.Message != "openai.ws_http_bridge_failed" {
					continue
				}
				failed++
				require.Equal(t, "http_sse", event.Fields["upstream_transport"])
				require.Equal(t, true, event.Fields["upstream_success_status"])
				require.Equal(t, tt.wantAccepted, event.Fields["accepted"])
				require.Equal(t, tt.wantKnown, event.Fields["usage_known"])
				require.Equal(t, tt.kind, event.Fields["failure_kind"])
				require.Equal(t, tt.readKind, event.Fields["read_error_kind"])
				for _, field := range []string{"error", "read_error", "body", "message", "text", "cipher", "close_reason"} {
					require.NotContains(t, event.Fields, field)
				}
				for _, value := range event.Fields {
					require.NotContains(t, fmt.Sprint(value), "sensitive")
				}
			}
			require.Equal(t, 1, entered)
			if tt.cancel {
				require.Zero(t, failed)
			} else {
				require.Equal(t, 1, failed)
			}
		})
	}
}

func TestAstraWSHTTPBridgeCheckedFailureSequence(t *testing.T) {
	for _, tt := range []struct {
		name, sequence string
		wantFailed     bool
	}{
		{"max_minus_one", "9223372036854775806", true},
		{"max", "9223372036854775807", false},
		{"overflow", "9223372036854775808", false},
		{"fraction", "1.5", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			sse := `data: {"type":"response.custom_tool_call_input.delta","sequence_number":` + tt.sequence + `,"delta":"Synthetic tool argument"}` + "\n\n"
			result, upstream, writes, err := runAstraWSHTTPBridgeFixture(t, context.Background(), astraWSHTTPFixtureResponse(sse, nil), astraWSHTTPFixturePayload(), nil)
			require.Error(t, err)
			require.Nil(t, result)
			require.Len(t, upstream.requests, 1)
			var failover *UpstreamFailoverError
			require.False(t, errors.As(err, &failover))
			if tt.wantFailed {
				requireAstraWSHTTPFailedTerminal(t, writes, math.MaxInt64, "upstream_stream_interrupted")
			} else {
				last := writes[len(writes)-1]
				require.Equal(t, "error", gjson.GetBytes(last, "type").String())
				require.False(t, gjson.GetBytes(last, "sequence_number").Exists())
				require.Equal(t, "upstream_protocol_error", gjson.GetBytes(last, "error.code").String())
				require.NotContains(t, string(last), tt.sequence)
				for _, message := range writes {
					require.NotEqual(t, "response.failed", gjson.GetBytes(message, "type").String())
				}
			}
		})
	}
}

func TestAstraNativeHTTP_CheckedFailureSequence(t *testing.T) {
	for _, tt := range []struct {
		name, sequence string
		wantFailed     bool
	}{
		{"max_minus_one", "9223372036854775806", true},
		{"max", "9223372036854775807", false},
		{"overflow", "9223372036854775808", false},
		{"fraction", "1.5", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			svc, account, c, rec, dialer := astraHTTPBridgeFixture(t, &openAIWSCaptureConn{})
			svc.cfg.Gateway.OpenAIWS.CtxPoolHTTPBridgeEnabled = false
			svc.cfg.Gateway.OpenAIWS.AstraHTTPBridgeEnabled = false
			upstream, ok := svc.httpUpstream.(*httpUpstreamRecorder)
			require.True(t, ok)
			upstream.resp = astraWSHTTPFixtureResponse(`data: {"type":"response.custom_tool_call_input.delta","sequence_number":`+tt.sequence+`,"delta":"Synthetic tool argument"}`+"\n\n", nil)
			result, err := svc.Forward(context.Background(), c, account, []byte(`{"model":"gpt-6-astra","stream":true,"input":"Synthetic finite request"}`))
			require.Error(t, err)
			require.Nil(t, result)
			require.Len(t, upstream.requests, 1)
			require.Zero(t, dialer.DialCount())
			var failover *UpstreamFailoverError
			require.False(t, errors.As(err, &failover))
			if rec.Code != http.StatusOK {
				require.Equal(t, http.StatusBadGateway, rec.Code)
				return // Invalid initial sequence was buffered; the client gets JSON failure.
			}
			var failures [][]byte
			forEachOpenAISSEFrame(rec.Body.String(), func(eventType string, payload []byte) {
				if eventType == "response.failed" || eventType == "error" {
					failures = append(failures, payload)
				}
			})
			require.Len(t, failures, 1)
			if tt.wantFailed {
				require.Equal(t, "response.failed", gjson.GetBytes(failures[0], "type").String())
				require.Equal(t, int64(math.MaxInt64), gjson.GetBytes(failures[0], "sequence_number").Int())
			} else {
				require.Equal(t, "error", gjson.GetBytes(failures[0], "type").String())
				require.False(t, gjson.GetBytes(failures[0], "sequence_number").Exists())
				require.Equal(t, "upstream_protocol_error", gjson.GetBytes(failures[0], "error.code").String())
			}
		})
	}
}

func TestAstraNativeHTTP_SequenceExhaustionMetadataMatchesWire(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Set(openAIAstraNativeHTTPContextKey, &openAIAstraNativeHTTPState{accepted: true, stream: true, lastSequence: math.MaxInt64,
		errorPayload: []byte(`{"error":{"code":"server_is_overloaded","type":"upstream_error","message":"Synthetic overload","status":503}}`)})
	_, err := c.Writer.Write([]byte("event: response.created\ndata: {}\n\n"))
	require.NoError(t, err)
	svc := &OpenAIGatewayService{}
	svc.writeOpenAIAstraNativeHTTPFailure(c, nil, "gpt-6-astra")
	var failure []byte
	forEachOpenAISSEFrame(rec.Body.String(), func(eventType string, payload []byte) {
		if eventType == "error" {
			failure = payload
		}
	})
	require.NotEmpty(t, failure)
	require.False(t, gjson.GetBytes(failure, "sequence_number").Exists())
	require.Equal(t, "upstream_protocol_error", gjson.GetBytes(failure, "error.code").String())
	streamError, known := GetOpsStreamError(c)
	require.True(t, known)
	require.Equal(t, "upstream_protocol_error", streamError.Code)
	require.Equal(t, http.StatusBadGateway, streamError.IntendedStatus)
	require.Equal(t, http.StatusBadGateway, streamError.UpstreamStatus)
}

type astraSuccessAfterHeaderDeadlineUpstream struct {
	body  string
	calls int
}

func (u *astraSuccessAfterHeaderDeadlineUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	u.calls++
	<-req.Context().Done()
	return astraWSHTTPFixtureResponse(u.body, nil), nil
}

func (u *astraSuccessAfterHeaderDeadlineUpstream) DoWithTLS(req *http.Request, _ string, _ int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, "", 0, 0)
}

func TestAstraNativeHTTP_HeaderDeadlineWithSuccessNeverReplays(t *testing.T) {
	for _, body := range []string{"", `data: {"type":"response.created","response":{"id":"resp_partial"`} {
		t.Run(fmt.Sprintf("body_bytes_%d", len(body)), func(t *testing.T) {
			svc, account, c, rec, dialer := astraHTTPBridgeFixture(t, &openAIWSCaptureConn{})
			svc.cfg.Gateway.OpenAIWS.CtxPoolHTTPBridgeEnabled = false
			svc.cfg.Gateway.OpenAIWS.AstraHTTPBridgeEnabled = false
			svc.cfg.Gateway.OpenAIFirstOutputTimeoutSeconds = 1
			upstream := &astraSuccessAfterHeaderDeadlineUpstream{body: body}
			svc.httpUpstream = upstream
			result, err := svc.Forward(context.Background(), c, account, []byte(`{"model":"gpt-6-astra","stream":true,"reasoning":{"effort":"low"},"input":"Synthetic finite request"}`))
			require.Error(t, err)
			var failover *UpstreamFailoverError
			require.False(t, errors.As(err, &failover), "a2xx returned after the local header deadline is still ambiguous generation acceptance")
			require.Nil(t, result)
			require.Equal(t, 1, upstream.calls)
			require.Zero(t, dialer.DialCount())
			require.False(t, openAIAstraNativeHTTPAccepted(c))
			require.True(t, openAIAstraNativeHTTPNoReplay(c))
			require.True(t, IsResponseCommitted(c))
			require.Equal(t, http.StatusGatewayTimeout, rec.Code)
			require.Equal(t, "first_output_timeout", gjson.Get(rec.Body.String(), "error.type").String())
			require.Equal(t, "first_output_timeout", gjson.Get(rec.Body.String(), "error.code").String())
			raw, found := c.Get(OpsUpstreamErrorsKey)
			require.True(t, found)
			events, ok := raw.([]*OpsUpstreamErrorEvent)
			require.True(t, ok)
			require.Len(t, events, 1)
			require.Equal(t, "first_output_timeout", events[0].Kind)
			require.Equal(t, http.StatusGatewayTimeout, events[0].UpstreamStatusCode)
			require.Contains(t, events[0].Detail, "phase=response_headers")
			require.NotContains(t, rec.Body.String(), "upstream_stream_interrupted")
		})
	}
}

func TestAstraWSHTTPBridgeGuardUsesFinalNormalizedModel(t *testing.T) {
	for _, body := range []string{"", `data: {"type":"response.created","response":{"id":"resp_partial"`} {
		t.Run(fmt.Sprintf("body_bytes_%d", len(body)), func(t *testing.T) {
			result, upstream, writes, err := runAstraWSHTTPBridgeFixture(t, context.Background(), astraWSHTTPFixtureResponse(body, nil), astraWSHTTPFixturePayload(), nil,
				func(account *Account) {
					account.Credentials = map[string]any{"model_mapping": map[string]any{"astra-alias": "gpt-6-astra", "gpt-6-astra": "gpt-6-sol"}}
				})
			require.Error(t, err)
			var failover *UpstreamFailoverError
			require.False(t, errors.As(err, &failover))
			require.Nil(t, result)
			require.Len(t, upstream.requests, 1)
			require.Equal(t, "gpt-6-astra", gjson.GetBytes(upstream.lastBody, "model").String())
			requireAstraWSHTTPFailedTerminal(t, writes, 0, "upstream_stream_interrupted")
		})
	}
}

func TestAstraHTTPProtocolExtensionCountersDoNotChangeResponseSequence(t *testing.T) {
	for _, counter := range []string{`"extension-a"`, `2.5`, `9223372036854775808`} {
		t.Run(counter, func(t *testing.T) {
			state := &openAIAstraNativeHTTPState{lastSequence: 7}
			observeOpenAIAstraHTTPProtocol(state, []byte(`{"type":"telemetry.extension","sequence_number":`+counter+`}`), "telemetry.extension")
			require.False(t, state.sequenceInvalid)
			require.Equal(t, int64(7), state.lastSequence)
			observeOpenAIAstraHTTPProtocol(state, []byte(`{"type":"response.failed","sequence_number":8,"response":{"id":"resp_extension","error":{"code":"server_error"}}}`), "response.failed")
			failure := buildOpenAIAstraHTTPProtocolFailure(state, state.responseID, "gpt-6-astra", state.errorPayload)
			require.Equal(t, "response.failed", gjson.GetBytes(failure, "type").String())
			require.Equal(t, int64(9), gjson.GetBytes(failure, "sequence_number").Int())
		})
	}
	result, upstream, writes, err := runAstraWSHTTPBridgeFixture(t, context.Background(), astraWSHTTPFixtureResponse(
		`data: {"type":"response.created","sequence_number":0,"response":{"id":"resp_extension"}}`+"\n\n"+
			`data: {"type":"telemetry.extension","sequence_number":"extension-a"}`+"\n\n"+
			`data: {"type":"response.completed","sequence_number":1,"response":{"id":"resp_extension","status":"completed","usage":{"input_tokens":1,"output_tokens":1}}}`+"\n\n", nil), astraWSHTTPFixturePayload(), nil)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Len(t, upstream.requests, 1)
	require.Equal(t, "response.completed", gjson.GetBytes(writes[len(writes)-1], "type").String())
}
