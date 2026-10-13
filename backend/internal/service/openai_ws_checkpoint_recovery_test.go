package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

const checkpointTestItem = `{"type":"response.output_item.done","item":{"id":"rs_private","type":"reasoning","status":"completed","encrypted_content":"opaque-test-only","summary":[]}}`

type checkpointTestDialer struct {
	mu         sync.Mutex
	conns      []*openAIWSCaptureConn
	headers    []http.Header
	failSecond bool
}

func (d *checkpointTestDialer) Dial(ctx context.Context, url string, headers http.Header, proxy string) (openAIWSClientConn, int, http.Header, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.headers = append(d.headers, cloneHeader(headers))
	if len(d.headers) > 1 && d.failSecond {
		return nil, 0, nil, errors.New("synthetic dial failure")
	}
	if len(d.headers) > len(d.conns) {
		return nil, 0, nil, errors.New("unexpected replay")
	}
	return d.conns[len(d.headers)-1], 0, http.Header{}, nil
}

func TestCheckpointContinuationResumesOpaqueReasoningWithoutDuplicateOutput(t *testing.T) {
	for _, fail := range []string{"eof", "server_error", "failed_event"} {
		t.Run(fail, func(t *testing.T) {
			first := &openAIWSCaptureConn{events: [][]byte{[]byte(`{"type":"response.created","response":{"id":"resp_first"}}`), []byte(checkpointTestItem)}, readDelays: []time.Duration{1100 * time.Millisecond}}
			if fail == "server_error" {
				first.events = append(first.events, []byte(`{"type":"error","error":{"code":"server_error","type":"server_error","message":"synthetic failure"}}`))
			}
			if fail == "failed_event" {
				first.events = append(first.events, []byte(`{"type":"response.failed","response":{"id":"resp_first","status":"failed","error":{"code":"server_error","type":"server_error"}}}`))
			}
			second := &openAIWSCaptureConn{events: [][]byte{
				[]byte(`{"type":"response.created","response":{"id":"resp_resumed"}}`),
				[]byte(`{"type":"response.output_item.added","output_index":0,"item":{"id":"msg_resumed","type":"message","status":"in_progress","role":"assistant","content":[]}}`),
				[]byte(`{"type":"response.output_text.delta","item_id":"msg_resumed","output_index":0,"content_index":0,"delta":"synthetic resumed answer"}`),
				[]byte(`{"type":"response.output_item.done","output_index":0,"item":{"id":"msg_resumed","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"synthetic resumed answer","annotations":[]}]}}`),
				[]byte(`{"type":"response.completed","response":{"id":"resp_resumed","status":"completed","model":"gpt-6-astra","output":[{"id":"msg_resumed","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"synthetic resumed answer","annotations":[]}]}],"usage":{"input_tokens":8,"output_tokens":3}}}`),
			}}
			svc, account, c, rec, _ := astraHTTPBridgeFixture(t, first)
			svc.cfg.Gateway.OpenAIWS.CtxPoolCheckpointRecoveryEnabled = true
			dialer := &checkpointTestDialer{conns: []*openAIWSCaptureConn{first, second}}
			svc.openaiWSPool.setClientDialerForTest(dialer)
			result, err := svc.Forward(context.Background(), c, account, []byte(`{"model":"gpt-6-astra","stream":true,"reasoning":{"effort":"max"},"service_tier":"default","input":[{"role":"user","content":"synthetic long history"}],"include":["message.output_text.logprobs"]}`))
			require.NoError(t, err)
			require.Equal(t, "resp_resumed", result.ResponseID)
			require.Equal(t, 8, result.Usage.InputTokens)
			require.Len(t, dialer.headers, 2)
			require.Equal(t, dialer.headers[0].Get("session_id"), dialer.headers[1].Get("session_id"))
			require.Equal(t, dialer.headers[0].Get("conversation_id"), dialer.headers[1].Get("conversation_id"))
			payload, _ := json.Marshal(second.lastWrite)
			require.Equal(t, "synthetic long history", gjson.GetBytes(payload, "input.0.content").String())
			require.Equal(t, "opaque-test-only", gjson.GetBytes(payload, "input.1.encrypted_content").String())
			require.Equal(t, "max", gjson.GetBytes(payload, "reasoning.effort").String())
			require.Equal(t, "default", gjson.GetBytes(payload, "service_tier").String())
			require.Contains(t, requestToJSONString(first.lastWrite), "message.output_text.logprobs")
			require.Contains(t, requestToJSONString(first.lastWrite), "reasoning.encrypted_content")
			require.Equal(t, 1, strings.Count(rec.Body.String(), `"type":"response.completed"`))
			require.Equal(t, 1, strings.Count(rec.Body.String(), `"type":"response.output_text.delta"`))
			require.Contains(t, rec.Body.String(), `"delta":"synthetic resumed answer"`)
			var completed []byte
			forEachOpenAISSEFrame(rec.Body.String(), func(eventType string, data []byte) {
				if gjson.GetBytes(data, "type").String() == "response.completed" {
					completed = append([]byte(nil), data...)
				}
			})
			require.Equal(t, "synthetic resumed answer", gjson.GetBytes(completed, "response.output.0.content.0.text").String())
			require.NotContains(t, rec.Body.String(), "resp_first")
			require.NotContains(t, rec.Body.String(), "opaque-test-only")
			require.NotContains(t, rec.Body.String(), "response.failed")
			require.True(t, first.closed)
			require.True(t, second.closed)
		})
	}
}

func TestCheckpointContinuationSafetyGates(t *testing.T) {
	for _, kind := range []string{"disabled", "no_checkpoint", "visible", "tool_started", "tool_lifecycle", "terminal_tool", "known_zero_usage", "visible_summary", "partial_checkpoint", "rate_limit", "context_limit", "budget", "incomplete"} {
		t.Run(kind, func(t *testing.T) {
			events := [][]byte{[]byte(checkpointTestItem)}
			failure := `{"type":"error","error":{"code":"server_error","type":"server_error"}}`
			switch kind {
			case "no_checkpoint":
				events = nil
			case "visible":
				events = append(events, []byte(`{"type":"response.output_text.delta","delta":"partial"}`))
			case "tool_started":
				events = append(events, []byte(`{"type":"response.output_item.added","item":{"type":"function_call","id":"fc_1"}}`))
			case "tool_lifecycle":
				events = append(events, []byte(`{"type":"response.web_search_call.in_progress","item_id":"ws_started"}`))
			case "terminal_tool":
				failure = `{"type":"response.failed","response":{"status":"failed","error":{"code":"server_error"},"output":[{"type":"function_call","id":"fc_started","status":"completed","arguments":"{}"}]}}`
			case "known_zero_usage":
				failure = `{"type":"response.failed","response":{"status":"failed","error":{"code":"server_error"},"usage":{"input_tokens":0,"output_tokens":0}}}`
			case "visible_summary":
				events = append(events, []byte(`{"type":"response.reasoning_summary_text.done","item_id":"rs_private","text":"visible summary"}`))
			case "partial_checkpoint":
				events = append(events, []byte(`{"type":"response.output_item.done","item":{"id":"rs_partial","type":"reasoning","status":"incomplete","encrypted_content":"opaque-test-only","summary":[]}}`))
			case "rate_limit":
				failure = `{"type":"error","error":{"code":"rate_limit_exceeded","type":"rate_limit_error"}}`
			case "context_limit":
				failure = `{"type":"error","error":{"code":"context_length_exceeded","type":"invalid_request_error"}}`
			case "incomplete":
				failure = `{"type":"response.incomplete","response":{"id":"resp_budget","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"usage":{"input_tokens":1,"output_tokens":8}}}`
			}
			events = append(events, []byte(failure))
			conn := &openAIWSCaptureConn{events: events}
			svc, account, c, _, _ := astraHTTPBridgeFixture(t, conn)
			svc.cfg.Gateway.OpenAIWS.CtxPoolCheckpointRecoveryEnabled = kind != "disabled"
			dialer := &checkpointTestDialer{conns: []*openAIWSCaptureConn{conn}}
			svc.openaiWSPool.setClientDialerForTest(dialer)
			body := `{"model":"gpt-6-astra","stream":true,"input":"test"}`
			if kind == "budget" {
				body = `{"model":"gpt-6-astra","stream":true,"max_output_tokens":100,"input":"test"}`
			}
			_, _ = svc.Forward(context.Background(), c, account, []byte(body))
			require.Len(t, dialer.headers, 1, "must not start another inference request")
		})
	}
}

func TestCheckpointContinuationBoundedAndSecondDialFailureTerminatesSSE(t *testing.T) {
	for _, dialFailure := range []bool{false, true} {
		t.Run(map[bool]string{false: "second_eof", true: "second_dial_failure"}[dialFailure], func(t *testing.T) {
			first := &openAIWSCaptureConn{events: [][]byte{[]byte(checkpointTestItem)}, readDelays: []time.Duration{1100 * time.Millisecond}}
			second := &openAIWSCaptureConn{events: [][]byte{[]byte(checkpointTestItem)}}
			svc, account, c, rec, _ := astraHTTPBridgeFixture(t, first)
			svc.cfg.Gateway.OpenAIWS.CtxPoolCheckpointRecoveryEnabled = true
			dialer := &checkpointTestDialer{conns: []*openAIWSCaptureConn{first, second}, failSecond: dialFailure}
			svc.openaiWSPool.setClientDialerForTest(dialer)
			_, err := svc.Forward(context.Background(), c, account, []byte(`{"model":"gpt-6-astra","stream":true,"input":"test"}`))
			require.Error(t, err)
			require.Len(t, dialer.headers, 2)
			require.Equal(t, 1, strings.Count(rec.Body.String(), `"type":"response.failed"`))
			require.True(t, IsResponseCommitted(c))
		})
	}
}

func TestReasoningCheckpointsCancellationUsageAndDedup(t *testing.T) {
	p := newOpenAIReasoningCheckpoints(true)
	p.observe([]byte(checkpointTestItem), "response.output_item.done")
	p.observe([]byte(checkpointTestItem), "response.output_item.done")
	require.Len(t, p.items, 1)
	require.True(t, p.canContinue(context.Background(), false, &OpenAIUsage{}))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.False(t, p.canContinue(ctx, false, &OpenAIUsage{}))
	require.False(t, p.canContinue(context.Background(), true, &OpenAIUsage{}))
	require.False(t, p.canContinue(context.Background(), false, &OpenAIUsage{OutputTokens: 1}))
	other := newOpenAIReasoningCheckpoints(true)
	require.Empty(t, other.items, "checkpoints are request-local")
	_, safe := openAICheckpointInput(map[string]any{"input": []any{map[string]any{"type": "item_reference", "id": "rs_other"}}})
	require.False(t, safe)
}

func TestCheckpointContinuationBlocksKnownUsageAndToolEvidence(t *testing.T) {
	for _, event := range []string{
		`{"type":"response.in_progress","response":{"usage":{"input_tokens":0,"output_tokens":0}}}`,
		`{"type":"error","error":{"code":"server_error"},"usage":{"input_tokens":0,"output_tokens":0}}`,
		`{"type":"response.failed","response":{"error":{"code":"server_error"},"usage":{}}}`,
		`{"type":"response.web_search_call.in_progress","item_id":"ws_started"}`,
		`{"type":"response.file_search_call.searching","item_id":"fs_started"}`,
		`{"type":"response.code_interpreter_call_code.done","item_id":"ci_started","code":"synthetic"}`,
		`{"type":"response.mcp_call.in_progress","item_id":"mcp_started"}`,
		`{"type":"response.mcp_list_tools.in_progress","item_id":"mcp_tools"}`,
		`{"type":"response.function_call_arguments.done","item_id":"fc_started","arguments":"{}"}`,
		`{"type":"response.custom_tool_call_input.done","item_id":"ct_started","input":"synthetic"}`,
		`{"type":"response.output_text.done","item_id":"msg_generated","text":"already generated"}`,
		`{"type":"response.content_part.added","item_id":"msg_generated","part":{"type":"output_text","text":"already generated"}}`,
		`{"type":"response.failed","response":{"error":{"code":"server_error"},"output":[{"type":"function_call","id":"fc_started","status":"completed","arguments":"{}"}]}}`,
		`{"type":"response.in_progress","response":{"output":[{"type":"message","content":[{"type":"output_text","text":"already generated"}]}]}}`,
	} {
		t.Run(gjson.Get(event, "type").String()+"_"+gjson.Get(event, "item_id").String()+"_"+gjson.Get(event, "response.output.0.type").String(), func(t *testing.T) {
			p := newOpenAIReasoningCheckpoints(true)
			p.observe([]byte(checkpointTestItem), "response.output_item.done")
			p.observe([]byte(event), gjson.Get(event, "type").String())
			require.False(t, p.canContinue(context.Background(), false, &OpenAIUsage{}), "must not discard known usage or replay after tool/output evidence")
		})
	}
	for _, event := range []string{
		`{"type":"response.in_progress","response":{"usage":null,"output":[]}}`,
		`{"type":"response.in_progress","response":{"output":[{"type":"reasoning","status":"completed","summary":[]}]}}`,
	} {
		p := newOpenAIReasoningCheckpoints(true)
		p.observe([]byte(checkpointTestItem), "response.output_item.done")
		p.observe([]byte(event), "response.in_progress")
		require.True(t, p.canContinue(context.Background(), false, &OpenAIUsage{}), "null usage and private completed reasoning are not known usage or visible output")
	}
}

func TestCheckpointContinuationRejectsPartialOrPlaintextReasoning(t *testing.T) {
	for _, item := range []string{
		`{"id":"rs_partial","type":"reasoning","status":"in_progress","encrypted_content":"opaque-test-only","summary":[]}`,
		`{"id":"rs_partial","type":"reasoning","status":"incomplete","encrypted_content":"opaque-test-only","summary":[]}`,
		`{"id":"rs_visible","type":"reasoning","status":"completed","encrypted_content":"opaque-test-only","summary":[{"type":"summary_text","text":"visible summary"}]}`,
		`{"id":"rs_visible","type":"reasoning","status":"completed","encrypted_content":"opaque-test-only","content":[{"type":"reasoning_text","text":"visible reasoning"}]}`,
	} {
		for _, eventType := range []string{"response.output_item.done", "response.failed"} {
			t.Run(eventType+"_"+gjson.Get(item, "status").String()+"_"+gjson.Get(item, "summary.0.type").String()+"_"+gjson.Get(item, "content.0.type").String(), func(t *testing.T) {
				p := newOpenAIReasoningCheckpoints(true)
				p.observe([]byte(checkpointTestItem), "response.output_item.done")
				message := `{"type":"response.output_item.done","item":` + item + `}`
				if eventType == "response.failed" {
					message = `{"type":"response.failed","response":{"error":{"code":"server_error"},"output":[` + item + `]}}`
				}
				p.observe([]byte(message), eventType)
				require.False(t, p.canContinue(context.Background(), false, &OpenAIUsage{}))
				require.Len(t, p.items, 1, "partial or plaintext reasoning must not be retained as another checkpoint")
			})
		}
	}
}
