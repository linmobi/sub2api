package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type openAIWSIngressReadFailureConn struct {
	*openAIWSCaptureConn
	readErr       error
	cancel        context.CancelCauseFunc
	cancelCause   error
	beforeFailure func()
}

func (c *openAIWSIngressReadFailureConn) ReadMessage(ctx context.Context) ([]byte, error) {
	message, err := c.openAIWSCaptureConn.ReadMessage(ctx)
	if !errors.Is(err, io.EOF) {
		return message, err
	}
	if c.beforeFailure != nil {
		c.beforeFailure()
	}
	if c.cancel != nil {
		cause := c.cancelCause
		if cause == nil {
			cause = context.Canceled
		}
		c.cancel(cause)
		return nil, context.Canceled
	}
	return nil, c.readErr
}

type openAIWSIngressReadFailureFixture struct {
	client    *coderws.Conn
	upstream  *openAIWSCaptureConn
	dialer    *openAIWSQueueDialer
	c         *gin.Context
	serverErr chan error
	turns     chan openAIWSIngressDocumentTurn
}

func newOpenAIWSIngressReadFailureFixture(t *testing.T, frames []string, readErr error, cancelOnEOF bool, closeServerOnEOF ...bool) *openAIWSIngressReadFailureFixture {
	t.Helper()
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{}
	cfg.Security.URLAllowlist.Enabled = false
	cfg.Security.URLAllowlist.AllowInsecureHTTP = true
	cfg.Gateway.OpenAIWS.Enabled = true
	cfg.Gateway.OpenAIWS.APIKeyEnabled = true
	cfg.Gateway.OpenAIWS.ResponsesWebsocketsV2 = true
	cfg.Gateway.OpenAIWS.MaxConnsPerAccount = 1
	cfg.Gateway.OpenAIWS.MaxIdlePerAccount = 1
	cfg.Gateway.OpenAIWS.QueueLimitPerConn = 8
	cfg.Gateway.OpenAIWS.DialTimeoutSeconds = 3
	cfg.Gateway.OpenAIWS.ReadTimeoutSeconds = 3
	cfg.Gateway.OpenAIWS.WriteTimeoutSeconds = 3
	controlCtx, cancel := context.WithCancelCause(context.Background())
	var preemptState *openAIWSSessionPreemptState
	if len(closeServerOnEOF) > 1 && closeServerOnEOF[1] {
		preemptState = &openAIWSSessionPreemptState{}
		controlCtx = context.WithValue(controlCtx, openAIWSSessionPreemptContextKey{}, preemptState)
	}
	t.Cleanup(func() { cancel(context.Canceled) })
	capture := &openAIWSCaptureConn{}
	for _, frame := range frames {
		capture.events = append(capture.events, []byte(frame))
	}
	upstream := &openAIWSIngressReadFailureConn{openAIWSCaptureConn: capture, readErr: readErr}
	if cancelOnEOF {
		upstream.cancel = cancel
		if len(closeServerOnEOF) > 2 && closeServerOnEOF[2] {
			upstream.cancelCause = ErrOpenAIWSIngressLeaseLost
		}
	}
	dialer := &openAIWSQueueDialer{conns: []openAIWSClientConn{upstream}}
	pool := newOpenAIWSConnPool(cfg)
	pool.setClientDialerForTest(dialer)
	t.Cleanup(pool.Close)
	svc := &OpenAIGatewayService{cfg: cfg, cache: &stubGatewayCache{}, httpUpstream: &httpUpstreamRecorder{},
		openaiWSResolver: NewOpenAIWSProtocolResolver(cfg), openaiWSPool: pool, toolCorrector: NewCodexToolCorrector()}
	account := &Account{ID: 193, Name: "synthetic-ingress-read-failure", Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
		Status: StatusActive, Schedulable: true, Concurrency: 1,
		Credentials: map[string]any{"api_key": "sk-test"}, Extra: map[string]any{"responses_websockets_v2_enabled": true}}
	f := &openAIWSIngressReadFailureFixture{upstream: capture, dialer: dialer,
		serverErr: make(chan error, 1), turns: make(chan openAIWSIngressDocumentTurn, 2)}
	hooks := &OpenAIWSIngressHooks{ClientLifecycleContext: controlCtx, AfterTurn: func(_ int, result *OpenAIForwardResult, err error) {
		f.turns <- openAIWSIngressDocumentTurn{result: result, err: err}
	}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := coderws.Accept(w, r, nil)
		if err != nil {
			f.serverErr <- err
			return
		}
		defer func() { _ = conn.CloseNow() }()
		if preemptState != nil || (len(closeServerOnEOF) > 0 && closeServerOnEOF[0]) {
			upstream.beforeFailure = func() {
				if preemptState != nil {
					preemptState.preempted.Store(true)
				}
				if len(closeServerOnEOF) > 0 && closeServerOnEOF[0] {
					_ = conn.CloseNow()
				}
			}
		}
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = r.WithContext(controlCtx)
		f.c = c
		readCtx, stopRead := context.WithTimeout(controlCtx, 3*time.Second)
		_, firstMessage, err := conn.Read(readCtx)
		stopRead()
		if err != nil {
			f.serverErr <- err
			return
		}
		f.serverErr <- svc.ProxyResponsesWebSocketFromClient(controlCtx, c, conn, account, "sk-test", firstMessage, hooks)
	}))
	t.Cleanup(server.Close)
	ctx, stopDial := context.WithTimeout(context.Background(), 3*time.Second)
	client, _, err := coderws.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
	stopDial()
	require.NoError(t, err)
	f.client = client
	t.Cleanup(func() { _ = client.CloseNow() })
	writeCtx, stopWrite := context.WithTimeout(context.Background(), 3*time.Second)
	defer stopWrite()
	require.NoError(t, client.Write(writeCtx, coderws.MessageText,
		[]byte(`{"type":"response.create","model":"gpt-6-astra","stream":true,"reasoning":{"effort":"xhigh"},"input":"synthetic input"}`)))
	return f
}

func (f *openAIWSIngressReadFailureFixture) read(t *testing.T) ([]byte, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, message, err := f.client.Read(ctx)
	return message, err
}

func (f *openAIWSIngressReadFailureFixture) finish(t *testing.T, wantError bool) openAIWSIngressDocumentTurn {
	t.Helper()
	var turn openAIWSIngressDocumentTurn
	select {
	case turn = <-f.turns:
		if wantError {
			require.Error(t, turn.err)
			require.Nil(t, turn.result, "unknown failure usage must not create a billing result")
		} else {
			require.NoError(t, turn.err)
			require.NotNil(t, turn.result)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("ingress turn did not finish")
	}
	_ = f.client.CloseNow()
	select {
	case err := <-f.serverErr:
		if wantError {
			require.Error(t, err)
		} else {
			require.NoError(t, err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("ingress session did not stop")
	}
	require.Empty(t, f.turns, "account for the failed turn once")
	require.Equal(t, 1, f.dialer.DialCount(), "accepted generation must never retry")
	f.upstream.mu.Lock()
	writes := len(f.upstream.writes)
	f.upstream.mu.Unlock()
	require.Equal(t, 1, writes, "send the original generation once")
	return turn
}

const openAIWSIngressReadFailureCreated = `{"type":"response.created","sequence_number":10,"response":{"id":"resp_synthetic_interrupted","object":"response","status":"in_progress","model":"gpt-6-astra"}}`

func TestOpenAIWSIngressReadFailure_AcceptedEOFAndCloseHaveOneExplicitFailure(t *testing.T) {
	for _, tc := range []struct {
		name    string
		frames  []string
		readErr error
		lastSeq int64
	}{
		{"created_eof", []string{openAIWSIngressReadFailureCreated}, io.EOF, 10},
		{"reasoning_eof", []string{`{"type":"response.output_item.done","sequence_number":15,"item":{"id":"rs_synthetic","type":"reasoning","status":"completed","encrypted_content":"synthetic-opaque","summary":[]}}`}, io.EOF, 15},
		{"reported_usage_eof", []string{openAIWSIngressReadFailureCreated, `{"type":"response.in_progress","sequence_number":14,"response":{"id":"resp_synthetic_interrupted","usage":{"input_tokens":3,"output_tokens":2}}}`}, io.EOF, 14},
		{"visible_normal_close", []string{openAIWSIngressReadFailureCreated, `{"type":"response.output_text.delta","sequence_number":20,"item_id":"msg_synthetic","output_index":0,"content_index":0,"delta":"synthetic answer prefix"}`}, coderws.CloseError{Code: coderws.StatusNormalClosure, Reason: "synthetic-secret-close-reason"}, 20},
		{"tool_abnormal_close", []string{openAIWSIngressReadFailureCreated, `{"type":"response.output_item.added","sequence_number":30,"item":{"id":"fc_synthetic","type":"function_call","name":"synthetic_inspect","call_id":"call_synthetic","arguments":"","status":"in_progress"}}`}, coderws.CloseError{Code: coderws.StatusAbnormalClosure, Reason: "synthetic-secret-close-reason"}, 30},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newOpenAIWSIngressReadFailureFixture(t, tc.frames, tc.readErr, false)
			for _, frame := range tc.frames {
				message, err := f.read(t)
				require.NoError(t, err)
				require.Equal(t, gjson.Get(frame, "type").String(), gjson.GetBytes(message, "type").String())
			}
			message, err := f.read(t)
			require.NoError(t, err, "deliver an explicit terminal before transport close")
			require.True(t, json.Valid(message))
			require.Equal(t, "response.failed", gjson.GetBytes(message, "type").String())
			require.Equal(t, tc.lastSeq+1, gjson.GetBytes(message, "sequence_number").Int())
			require.Equal(t, "failed", gjson.GetBytes(message, "response.status").String())
			require.Equal(t, "response", gjson.GetBytes(message, "response.object").String())
			require.True(t, gjson.GetBytes(message, "response.created_at").Exists())
			require.Equal(t, "gpt-6-astra", gjson.GetBytes(message, "response.model").String())
			require.Equal(t, "upstream_stream_interrupted", gjson.GetBytes(message, "response.error.code").String())
			require.False(t, gjson.GetBytes(message, "response.usage").Exists(), "a local failure must never invent provider usage")
			_, err = f.read(t)
			require.Error(t, err, "do not deliver a duplicate failure")
			turn := f.finish(t, true)
			require.NotContains(t, turn.err.Error(), "synthetic-secret-close-reason")
			streamError, marked := GetOpsStreamError(f.c)
			require.True(t, marked)
			require.Equal(t, "upstream_stream_interrupted", streamError.Code)
			if code := coderws.CloseStatus(tc.readErr); code != -1 {
				var failure *openAIWSIngressReadFailure
				require.ErrorAs(t, turn.err, &failure)
				require.Equal(t, int(code), failure.closeStatus, "retain typed upstream closure evidence")
				require.Equal(t, coderws.StatusCode(-1), coderws.CloseStatus(turn.err), "upstream normal closure is not a normal client exit")
			}
		})
	}
}

func TestOpenAIWSIngressReadFailure_SafeEvidenceNeverContainsProviderText(t *testing.T) {
	failure := newOpenAIWSIngressReadFailure(coderws.CloseError{Code: coderws.StatusNormalClosure, Reason: "provider-secret-reason"})
	require.Equal(t, "websocket_close", failure.kind)
	require.Equal(t, int(coderws.StatusNormalClosure), failure.closeStatus)
	require.NotContains(t, failure.Error(), "provider-secret")
	require.Equal(t, coderws.StatusCode(-1), coderws.CloseStatus(failure), "handler must not mistake upstream1000 for a client cancellation")
	require.Equal(t, "response.output_text.delta", openAIWSIngressReadEvidenceEventType("response.output_text.delta"))
	require.Equal(t, "other", openAIWSIngressReadEvidenceEventType("provider-secret-event"))
	require.Equal(t, "resp_safe_id-1", openAIWSIngressReadEvidenceResponseID("resp_safe_id-1"))
	require.Empty(t, openAIWSIngressReadEvidenceResponseID("provider-secret-id"))
	require.Empty(t, openAIWSIngressReadEvidenceResponseID("resp_bad\nprovider-secret"))
	require.ErrorIs(t, newOpenAIWSIngressReadFailure(context.Canceled), context.Canceled)
	require.ErrorIs(t, newOpenAIWSIngressReadFailure(context.DeadlineExceeded), context.DeadlineExceeded)
	require.ErrorIs(t, newOpenAIWSIngressReadFailure(io.EOF), io.EOF)
}

func TestOpenAIWSIngressReadFailure_DoesNotDuplicateUpstreamFailureOrIncomplete(t *testing.T) {
	for _, event := range []string{
		`{"type":"error","sequence_number":11,"error":{"code":"server_error","type":"server_error","message":"synthetic provider failure"}}`,
		`{"type":"response.failed","sequence_number":11,"response":{"id":"resp_synthetic_interrupted","status":"failed","error":{"code":"server_error","type":"server_error","message":"synthetic provider failure"}}}`,
		`{"type":"response.incomplete","sequence_number":11,"response":{"id":"resp_synthetic_interrupted","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"}}}`,
	} {
		t.Run(gjson.Get(event, "type").String(), func(t *testing.T) {
			f := newOpenAIWSIngressReadFailureFixture(t, []string{openAIWSIngressReadFailureCreated, event}, io.EOF, false)
			for _, frame := range []string{openAIWSIngressReadFailureCreated, event} {
				message, err := f.read(t)
				require.NoError(t, err)
				require.Equal(t, gjson.Get(frame, "type").String(), gjson.GetBytes(message, "type").String())
			}
			wantError := gjson.Get(event, "type").String() == "error"
			if wantError {
				_, err := f.read(t)
				require.Error(t, err, "existing error must not acquire an extra local failed event")
			}
			f.finish(t, wantError)
		})
	}
}

func TestOpenAIWSIngressReadFailure_ClientCancellationAddsNoFailure(t *testing.T) {
	f := newOpenAIWSIngressReadFailureFixture(t, []string{openAIWSIngressReadFailureCreated}, io.EOF, true)
	message, err := f.read(t)
	require.NoError(t, err)
	require.Equal(t, "response.created", gjson.GetBytes(message, "type").String())
	_, err = f.read(t)
	require.Error(t, err)
	turn := f.finish(t, true)
	require.ErrorIs(t, turn.err, context.Canceled)
	_, marked := GetOpsStreamError(f.c)
	require.False(t, marked, "client cancellation is not an upstream stream fault")
}

func TestOpenAIWSIngressReadFailure_SequenceBoundaries(t *testing.T) {
	t.Run("max_minus_one_has_representable_failed_successor", func(t *testing.T) {
		frame := `{"type":"response.created","sequence_number":9223372036854775806,"response":{"id":"resp_synthetic_sequence","status":"in_progress"}}`
		f := newOpenAIWSIngressReadFailureFixture(t, []string{frame}, io.EOF, false)
		message, err := f.read(t)
		require.NoError(t, err)
		require.Equal(t, "9223372036854775806", gjson.GetBytes(message, "sequence_number").Raw)
		message, err = f.read(t)
		require.NoError(t, err)
		require.Equal(t, "response.failed", gjson.GetBytes(message, "type").String())
		require.Equal(t, "9223372036854775807", gjson.GetBytes(message, "sequence_number").Raw)
		_, err = f.read(t)
		require.Error(t, err)
		f.finish(t, true)
	})
	t.Run("max_has_unsequenced_protocol_error", func(t *testing.T) {
		frame := `{"type":"response.created","sequence_number":9223372036854775807,"response":{"id":"resp_synthetic_sequence","status":"in_progress"}}`
		f := newOpenAIWSIngressReadFailureFixture(t, []string{frame}, io.EOF, false)
		message, err := f.read(t)
		require.NoError(t, err)
		require.Equal(t, "9223372036854775807", gjson.GetBytes(message, "sequence_number").Raw)
		message, err = f.read(t)
		require.NoError(t, err)
		require.Equal(t, "error", gjson.GetBytes(message, "type").String())
		require.Equal(t, "upstream_protocol_error", gjson.GetBytes(message, "error.code").String())
		require.False(t, gjson.GetBytes(message, "sequence_number").Exists(), "never fabricate a lower successor")
		require.False(t, gjson.GetBytes(message, "usage").Exists())
		_, err = f.read(t)
		require.Error(t, err)
		f.finish(t, true)
	})
	for _, sequence := range []string{"9223372036854775808", "9007199254740992.5", "-1", "0.5", `"provider-secret-sequence"`, "null"} {
		t.Run("invalid_"+sequence, func(t *testing.T) {
			sink, releaseLog := captureStructuredLog(t)
			defer releaseLog()
			frame := `{"type":"response.output_text.delta","sequence_number":` + sequence + `,"delta":"provider-secret-body"}`
			f := newOpenAIWSIngressReadFailureFixture(t, []string{openAIWSIngressReadFailureCreated, frame}, io.EOF, false)
			message, err := f.read(t)
			require.NoError(t, err)
			require.Equal(t, "response.created", gjson.GetBytes(message, "type").String())
			message, err = f.read(t)
			require.NoError(t, err)
			require.Equal(t, "error", gjson.GetBytes(message, "type").String(), "invalid event must not be forwarded")
			require.Equal(t, "upstream_protocol_error", gjson.GetBytes(message, "error.code").String())
			require.False(t, gjson.GetBytes(message, "sequence_number").Exists())
			require.NotContains(t, string(message), "provider-secret")
			_, err = f.read(t)
			require.Error(t, err)
			turn := f.finish(t, true)
			require.False(t, isOpenAIWSIngressTurnRetryable(turn.err))
			require.NotContains(t, turn.err.Error(), "provider-secret")
			sink.mu.Lock()
			defer sink.mu.Unlock()
			for _, event := range sink.events {
				serialized, err := json.Marshal(event)
				require.NoError(t, err)
				require.NotContains(t, string(serialized), "provider-secret", "protocol diagnostics must never include provider text")
			}
		})
	}
	for _, terminalType := range []string{"response.completed", "response.incomplete"} {
		t.Run("max_"+terminalType+"_preserved", func(t *testing.T) {
			status := strings.TrimPrefix(terminalType, "response.")
			frame := `{"type":"` + terminalType + `","sequence_number":9223372036854775807,"response":{"id":"resp_synthetic_sequence","status":"` + status + `","usage":{"input_tokens":2,"output_tokens":3}}}`
			f := newOpenAIWSIngressReadFailureFixture(t, []string{frame}, io.EOF, false)
			message, err := f.read(t)
			require.NoError(t, err)
			require.Equal(t, frame, string(message), "authoritative terminal must remain unchanged")
			f.finish(t, false)
		})
	}
}

func TestOpenAIWSIngressReadFailure_PreemptedBeforeCancellationAddsNoFailure(t *testing.T) {
	sink, releaseLog := captureStructuredLog(t)
	defer releaseLog()
	f := newOpenAIWSIngressReadFailureFixture(t, []string{openAIWSIngressReadFailureCreated}, io.EOF, false, false, true)
	message, err := f.read(t)
	require.NoError(t, err)
	require.Equal(t, "response.created", gjson.GetBytes(message, "type").String())
	_, err = f.read(t)
	require.Error(t, err, "preemption must not send an extra failure in its pre-cancel gap")
	select {
	case sessionErr := <-f.serverErr:
		require.True(t, IsOpenAIWSSessionPreemptedError(sessionErr), "outer handler must recognize preemption before cancellation")
	case <-time.After(3 * time.Second):
		t.Fatal("preempted ingress session did not stop")
	}
	require.Empty(t, f.turns, "existing preemption path must not report an upstream-failed turn")
	require.Equal(t, 1, f.dialer.DialCount(), "preemption must never replay")
	f.upstream.mu.Lock()
	writes := len(f.upstream.writes)
	f.upstream.mu.Unlock()
	require.Equal(t, 1, writes)
	_, marked := GetOpsStreamError(f.c)
	require.False(t, marked, "preemption is not an upstream fault")
	sink.mu.Lock()
	defer sink.mu.Unlock()
	count := 0
	for _, event := range sink.events {
		if event.Message == "openai.ws_ingress_read_failed" {
			count++
			require.Equal(t, "info", event.Level)
			require.Equal(t, true, event.Fields["session_preempted"])
			require.Equal(t, false, event.Fields["context_canceled"], "fixture must cover state set before context cancellation")
			require.Equal(t, false, event.Fields["failure_delivered"])
		}
	}
	require.Equal(t, 1, count)
}

func TestOpenAIWSIngressReadFailure_LogReportsFinalDeliveryState(t *testing.T) {
	for _, tc := range []struct {
		name                                                             string
		frames                                                           []string
		cancelOnEOF, closeServerOnEOF, wantDelivered, wantPriorDelivered bool
		wantLevel                                                        string
	}{
		{"local_failure_delivered", []string{openAIWSIngressReadFailureCreated}, false, false, true, false, "warn"},
		{"local_failure_write_failed", []string{openAIWSIngressReadFailureCreated}, false, true, false, false, "warn"},
		{"upstream_error_already_delivered", []string{openAIWSIngressReadFailureCreated, `{"type":"error","sequence_number":11,"error":{"code":"server_error","type":"server_error","message":"synthetic provider failure"}}`}, false, false, true, true, "info"},
		{"client_canceled", []string{openAIWSIngressReadFailureCreated}, true, false, false, false, "info"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sink, releaseLog := captureStructuredLog(t)
			defer releaseLog()
			f := newOpenAIWSIngressReadFailureFixture(t, tc.frames, io.EOF, tc.cancelOnEOF, tc.closeServerOnEOF)
			for range tc.frames {
				_, err := f.read(t)
				require.NoError(t, err)
			}
			if tc.wantDelivered && !tc.wantPriorDelivered {
				message, err := f.read(t)
				require.NoError(t, err)
				require.Equal(t, "response.failed", gjson.GetBytes(message, "type").String())
			}
			_, err := f.read(t)
			require.Error(t, err)
			f.finish(t, true)
			sink.mu.Lock()
			defer sink.mu.Unlock()
			count := 0
			for _, event := range sink.events {
				if event.Message != "openai.ws_ingress_read_failed" {
					continue
				}
				count++
				require.Equal(t, tc.wantLevel, event.Level)
				require.Equal(t, tc.wantDelivered, event.Fields["failure_delivered"], "log delivery after the actual terminal write")
				require.Equal(t, tc.wantPriorDelivered, event.Fields["prior_failure_delivered"])
				require.Equal(t, "read_upstream", event.Fields["stage"])
				require.Equal(t, false, event.Fields["ingress_lease_lost"], "ordinary cancellation is distinct from lease loss")
				for _, field := range []string{"error", "read_error", "close_reason", "body"} {
					require.NotContains(t, event.Fields, field)
				}
			}
			require.Equal(t, 1, count)
		})
	}
}

func TestOpenAIWSIngressReadFailure_LogReportsLeaseLoss(t *testing.T) {
	sink, releaseLog := captureStructuredLog(t)
	defer releaseLog()
	f := newOpenAIWSIngressReadFailureFixture(t, []string{openAIWSIngressReadFailureCreated}, io.EOF, true, false, false, true)
	_, err := f.read(t)
	require.NoError(t, err)
	_, err = f.read(t)
	require.Error(t, err)
	f.finish(t, true)
	sink.mu.Lock()
	defer sink.mu.Unlock()
	count := 0
	for _, event := range sink.events {
		if event.Message != "openai.ws_ingress_read_failed" {
			continue
		}
		count++
		require.Equal(t, true, event.Fields["ingress_lease_lost"])
		require.Equal(t, "info", event.Level)
		require.Equal(t, false, event.Fields["failure_delivered"])
		for _, field := range []string{"error", "read_error", "close_reason", "body"} {
			require.NotContains(t, event.Fields, field)
		}
	}
	require.Equal(t, 1, count)
}
