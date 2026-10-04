package service

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func astraHTTPBridgeFixture(t *testing.T, conn *openAIWSCaptureConn) (*OpenAIGatewayService, *Account, *gin.Context, *httptest.ResponseRecorder, *openAIWSCaptureDialer) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	cfg := newOpenAIWSV2TestConfig()
	cfg.Gateway.OpenAIWS.AstraHTTPBridgeEnabled = true
	cfg.Gateway.OpenAIWS.AstraHTTPBridgeReadTimeoutSeconds = 4
	cfg.Gateway.OpenAIWS.AstraHTTPBridgeHeartbeatSeconds = 1
	cfg.Gateway.OpenAIWS.ReadTimeoutSeconds = 1
	cfg.Gateway.OpenAIWS.MaxConnsPerAccount = 2
	cfg.Gateway.OpenAIWS.MaxIdlePerAccount = 1
	cfg.Gateway.OpenAIWS.QueueLimitPerConn = 8
	cfg.Gateway.OpenAIWS.DialTimeoutSeconds = 3
	cfg.Gateway.OpenAIWS.WriteTimeoutSeconds = 3
	cfg.Gateway.OpenAIWS.StoreDisabledConnMode = "off" // bridge must override unsafe legacy policy
	dialer := &openAIWSCaptureDialer{conn: conn}
	pool := newOpenAIWSConnPool(cfg)
	t.Cleanup(pool.Close)
	pool.setClientDialerForTest(dialer)
	svc := &OpenAIGatewayService{cfg: cfg, httpUpstream: &httpUpstreamRecorder{}, cache: &stubGatewayCache{}, openaiWSResolver: NewOpenAIWSProtocolResolver(cfg), toolCorrector: NewCodexToolCorrector(), openaiWSPool: pool}
	account := &Account{ID: 91, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true, Concurrency: 2,
		Credentials: map[string]any{"access_token": "synthetic-test-token"},
		Extra:       map[string]any{"openai_oauth_responses_websockets_v2_enabled": true, "openai_astra_http_ws_bridge_enabled": true}}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	groupID := int64(7)
	c.Set("api_key", &APIKey{ID: 11, GroupID: &groupID})
	SetOpenAIHTTPResponseOwner(c, 22, 11)
	SetOpenAIClientTransport(c, OpenAIClientTransportHTTP)
	return svc, account, c, rec, dialer
}

func TestAstraHTTPBridge_GatesRetainHTTP(t *testing.T) {
	tests := []struct {
		name   string
		change func(*OpenAIGatewayService, *Account, *gin.Context)
		model  string
	}{
		{"default_off", func(s *OpenAIGatewayService, _ *Account, _ *gin.Context) {
			s.cfg.Gateway.OpenAIWS.AstraHTTPBridgeEnabled = false
		}, "gpt-6-astra"},
		{"other_model", func(*OpenAIGatewayService, *Account, *gin.Context) {}, "gpt-6-sol"},
		{"account_opt_out", func(_ *OpenAIGatewayService, a *Account, _ *gin.Context) {
			delete(a.Extra, "openai_astra_http_ws_bridge_enabled")
		}, "gpt-6-astra"},
		{"ws_opt_out", func(_ *OpenAIGatewayService, a *Account, _ *gin.Context) {
			a.Extra["openai_oauth_responses_websockets_v2_enabled"] = false
		}, "gpt-6-astra"},
		{"api_key_account", func(_ *OpenAIGatewayService, a *Account, _ *gin.Context) {
			a.Type = AccountTypeAPIKey
			a.Credentials = map[string]any{"api_key": "synthetic-test-key"}
		}, "gpt-6-astra"},
		{"passthrough", func(_ *OpenAIGatewayService, a *Account, _ *gin.Context) { a.Extra["openai_passthrough"] = true }, "gpt-6-astra"},
		{"force_http", func(s *OpenAIGatewayService, _ *Account, _ *gin.Context) { s.cfg.Gateway.OpenAIWS.ForceHTTP = true }, "gpt-6-astra"},
		{"noncanary_key", func(s *OpenAIGatewayService, _ *Account, _ *gin.Context) {
			s.cfg.Gateway.OpenAIWS.AstraHTTPBridgeAPIKeyIDs = []int64{12}
		}, "gpt-6-astra"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			svc, account, c, _, dialer := astraHTTPBridgeFixture(t, &openAIWSCaptureConn{})
			tc.change(svc, account, c)
			httpClient := svc.httpUpstream.(*httpUpstreamRecorder)
			httpClient.resp = &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_http\",\"model\":\"" + tc.model + "\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n"))}
			result, err := svc.Forward(context.Background(), c, account, []byte(`{"model":"`+tc.model+`","stream":true,"input":"hello"}`))
			require.NoError(t, err)
			require.NotNil(t, result)
			require.False(t, result.OpenAIWSMode)
			require.NotNil(t, httpClient.lastReq)
			require.Zero(t, dialer.DialCount(), "ineligible requests must not dial WS")
		})
	}
}

func TestAstraHTTPBridge_KeepaliveBeforeTokensCompletesAndBindsOwner(t *testing.T) {
	conn := &openAIWSCaptureConn{events: [][]byte{[]byte(`{"type":"response.completed","response":{"id":"resp_astra_bridge","model":"gpt-6-astra","status":"completed","usage":{"input_tokens":3,"output_tokens":2}}}`)}, readDelays: []time.Duration{2200 * time.Millisecond}}
	svc, account, c, rec, dialer := astraHTTPBridgeFixture(t, conn)
	svc.cfg.Gateway.OpenAIWS.AllowStoreRecovery = true // bridge must still send false
	result, err := svc.Forward(context.Background(), c, account, []byte(`{"model":"gpt-6-astra","stream":true,"store":true,"input":[{"role":"user","content":"synthetic full input"}]}`))
	require.NoError(t, err, "experimental read timeout must exceed the legacy 1-second deadline")
	require.True(t, result.OpenAIWSMode)
	require.Nil(t, result.FirstTokenMs, "keepalive comments are not model tokens")
	require.GreaterOrEqual(t, strings.Count(rec.Body.String(), ": ping\n\n"), 2)
	require.Equal(t, 1, strings.Count(rec.Body.String(), `"type":"response.completed"`))
	require.False(t, conn.lastWrite["store"].(bool))
	require.Contains(t, requestToJSONString(conn.lastWrite), "synthetic full input")
	require.Equal(t, dialer.lastHeaders.Get("session_id"), dialer.lastHeaders.Get("conversation_id"))
	require.True(t, conn.closed, "request-private upstream must be discarded even on success")
	owned, err := svc.ValidateOpenAIHTTPResponseOwner(context.Background(), 7, "resp_astra_bridge", 22, 11)
	require.NoError(t, err)
	require.True(t, owned)
	owned, err = svc.ValidateOpenAIHTTPResponseOwner(context.Background(), 7, "resp_astra_bridge", 23, 12)
	require.NoError(t, err)
	require.False(t, owned)
	bridge, ok := openAIAstraHTTPBridgeFromContext(c)
	require.True(t, ok)
	_, sessionBound := svc.getOpenAIWSStateStore().GetSessionConn(7, bridge.executionScope)
	require.False(t, sessionBound, "private connection must not become resumable shared state")
}

func TestAstraHTTPBridge_EOFWithOnlyKeepaliveEmitsSSEError(t *testing.T) {
	conn := &openAIWSCaptureConn{events: [][]byte{[]byte(`{"type":"response.created","response":{"id":"resp_eof"}}`)}, readDelays: []time.Duration{1100 * time.Millisecond}}
	svc, account, c, rec, dialer := astraHTTPBridgeFixture(t, conn)
	result, err := svc.Forward(context.Background(), c, account, []byte(`{"model":"gpt-6-astra","stream":true,"input":"synthetic input"}`))
	require.Error(t, err)
	require.NotNil(t, result)
	require.Equal(t, "resp_eof", result.ResponseID)
	require.Zero(t, result.Usage.InputTokens, "unknown usage must not be invented")
	require.Contains(t, rec.Body.String(), ": ping\n\n")
	require.Equal(t, 1, strings.Count(rec.Body.String(), `"code":"upstream_stream_interrupted"`))
	require.Equal(t, 1, strings.Count(rec.Body.String(), `"type":"response.failed"`))
	forEachOpenAISSEFrame(rec.Body.String(), func(eventType string, payload []byte) {
		if eventType != "response.failed" {
			return
		}
		require.True(t, gjson.GetBytes(payload, "sequence_number").Exists())
		require.True(t, gjson.GetBytes(payload, "response.created_at").Exists())
		require.Equal(t, "resp_eof", gjson.GetBytes(payload, "response.id").String())
		require.False(t, gjson.GetBytes(payload, "response.usage").Exists())
	})
	require.True(t, IsResponseCommitted(c), "handler must not append a second terminal failure")
	require.NotContains(t, rec.Body.String(), `"type":"response.completed"`)
	require.Nil(t, svc.httpUpstream.(*httpUpstreamRecorder).lastReq, "committed keepalive must not cause an invisible HTTP replay")
	require.Equal(t, 1, dialer.DialCount())
}

func TestAstraHTTPBridge_RejectsContinuationBeforeDial(t *testing.T) {
	svc, account, c, rec, dialer := astraHTTPBridgeFixture(t, &openAIWSCaptureConn{})
	_, err := svc.Forward(context.Background(), c, account, []byte(`{"model":"gpt-6-astra","stream":true,"previous_response_id":"resp_other_user","input":"synthetic"}`))
	require.Error(t, err)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.True(t, IsResponseCommitted(c))
	require.Contains(t, rec.Body.String(), "full input")
	require.Zero(t, dialer.DialCount())
	require.Nil(t, svc.httpUpstream.(*httpUpstreamRecorder).lastReq)
}

func TestAstraHTTPBridge_RequestScopeCannotBeShared(t *testing.T) {
	svc, account, c, _, _ := astraHTTPBridgeFixture(t, &openAIWSCaptureConn{})
	var scopes []string
	for _, pair := range []struct{ keyID, accountID int64 }{{11, 91}, {11, 91}, {12, 91}, {11, 92}} {
		c.Set("api_key", &APIKey{ID: pair.keyID})
		account.ID = pair.accountID
		scope, err := prepareOpenAIAstraHTTPBridge(c, account, []byte(`{"input":"same text"}`), "same-client-thread")
		require.NoError(t, err)
		for _, other := range scopes {
			require.NotEqual(t, other, scope)
		}
		scopes = append(scopes, scope)
	}
	_ = svc
}

func TestAstraHTTPBridge_TimeoutDefaultIndependentFromWSIngress(t *testing.T) {
	svc := &OpenAIGatewayService{cfg: &config.Config{}}
	require.Equal(t, time.Hour, svc.astraHTTPBridgeReadTimeout())
	require.Equal(t, 15*time.Minute, svc.openAIWSReadTimeout())
}

func TestAstraHTTPBridge_PartialUsageSurvivesEOF(t *testing.T) {
	conn := &openAIWSCaptureConn{events: [][]byte{
		[]byte(`{"type":"response.in_progress","response":{"id":"resp_partial","usage":{"input_tokens":5,"output_tokens":2}}}`),
		[]byte(`{"type":"response.output_text.delta","delta":"synthetic partial output"}`),
	}}
	svc, account, c, rec, _ := astraHTTPBridgeFixture(t, conn)
	result, err := svc.Forward(context.Background(), c, account, []byte(`{"model":"gpt-6-astra","stream":true,"input":"synthetic full input"}`))
	require.Error(t, err)
	require.NotNil(t, result)
	require.Equal(t, 5, result.Usage.InputTokens)
	require.Equal(t, 2, result.Usage.OutputTokens)
	require.Equal(t, 1, strings.Count(rec.Body.String(), `"code":"upstream_stream_interrupted"`))
	require.True(t, IsResponseCommitted(c))
	_, failed := GetOpsStreamError(c)
	require.True(t, failed)
}

func TestAstraHTTPBridge_UpstreamErrorDoesNotDuplicateOrMixProtocols(t *testing.T) {
	for _, stream := range []bool{true, false} {
		conn := &openAIWSCaptureConn{events: [][]byte{[]byte(`{"type":"error","error":{"type":"invalid_request_error","code":"invalid_request_error","message":"synthetic rejected input"}}`)}}
		svc, account, c, rec, _ := astraHTTPBridgeFixture(t, conn)
		body := `{"model":"gpt-6-astra","stream":false,"input":"synthetic"}`
		if stream {
			body = `{"model":"gpt-6-astra","stream":true,"input":"synthetic"}`
		}
		_, err := svc.Forward(context.Background(), c, account, []byte(body))
		require.Error(t, err)
		require.True(t, IsResponseCommitted(c))
		require.Equal(t, 1, strings.Count(rec.Body.String(), "synthetic rejected input"))
		if !stream {
			require.NotContains(t, rec.Body.String(), "data:")
			require.Contains(t, rec.Header().Get("Content-Type"), "application/json")
		} else {
			require.NotContains(t, rec.Body.String(), "upstream_stream_interrupted")
			require.Equal(t, 1, strings.Count(rec.Body.String(), `"type":"response.failed"`))
		}
	}
}

func TestAstraHTTPBridge_ClientCancellationDrainsUsageAndClosesPrivateConn(t *testing.T) {
	conn := &openAIWSCaptureConn{events: [][]byte{
		[]byte(`{"type":"response.created","response":{"id":"resp_cancel_bridge"}}`),
		[]byte(`{"type":"response.output_text.delta","delta":"partial"}`),
		[]byte(`{"type":"response.completed","response":{"id":"resp_cancel_bridge","model":"gpt-6-astra","usage":{"input_tokens":3,"output_tokens":5}}}`),
	}, readDelays: []time.Duration{0, 0, 25 * time.Millisecond}}
	svc, account, _, _, _ := astraHTTPBridgeFixture(t, conn)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	writer := &cancelOnFirstWriteResponseWriter{cancel: cancel}
	c, _ := gin.CreateTestContext(writer)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil).WithContext(ctx)
	groupID := int64(7)
	c.Set("api_key", &APIKey{ID: 11, GroupID: &groupID})
	SetOpenAIHTTPResponseOwner(c, 22, 11)
	SetOpenAIClientTransport(c, OpenAIClientTransportHTTP)
	result, err := svc.Forward(ctx, c, account, []byte(`{"model":"gpt-6-astra","stream":true,"input":"synthetic"}`))
	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, result.ClientDisconnect)
	require.Equal(t, 3, result.Usage.InputTokens)
	require.Equal(t, 5, result.Usage.OutputTokens)
	require.True(t, conn.closed)
	require.NotContains(t, writer.body.String(), "upstream_stream_interrupted")
}

func TestAstraHTTPBridge_CancellationDuringKeepaliveStopsReadWorker(t *testing.T) {
	conn := &openAIWSCaptureConn{events: [][]byte{
		[]byte(`{"type":"response.created","response":{"id":"resp_cancel_wait"}}`),
		[]byte(`{"type":"response.in_progress","response":{"id":"resp_cancel_wait"}}`),
	}, readDelays: []time.Duration{0, 5 * time.Second}}
	svc, account, _, _, _ := astraHTTPBridgeFixture(t, conn)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	writer := &cancelOnFirstWriteResponseWriter{cancel: cancel}
	c, _ := gin.CreateTestContext(writer)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil).WithContext(ctx)
	groupID := int64(7)
	c.Set("api_key", &APIKey{ID: 11, GroupID: &groupID})
	SetOpenAIHTTPResponseOwner(c, 22, 11)
	SetOpenAIClientTransport(c, OpenAIClientTransportHTTP)
	started := time.Now()
	result, err := svc.Forward(ctx, c, account, []byte(`{"model":"gpt-6-astra","stream":true,"input":"synthetic"}`))
	require.ErrorIs(t, err, context.Canceled)
	require.NotNil(t, result)
	require.True(t, result.ClientDisconnect)
	require.Less(t, time.Since(started), 2500*time.Millisecond, "read worker must stop when downstream is canceled")
	require.Zero(t, result.Usage.InputTokens)
	require.Zero(t, result.Usage.OutputTokens)
	require.True(t, conn.closed)
	require.NotContains(t, writer.body.String(), "upstream_stream_interrupted")
}

func TestAstraHTTPBridge_FrequentHiddenEventsDoNotPostponeKeepalive(t *testing.T) {
	conn := &openAIWSCaptureConn{}
	for i := 0; i < 4; i++ {
		conn.events = append(conn.events, []byte(`{"type":"response.in_progress","response":{"id":"resp_progress"}}`))
		conn.readDelays = append(conn.readDelays, 600*time.Millisecond)
	}
	conn.events = append(conn.events, []byte(`{"type":"response.completed","response":{"id":"resp_progress","model":"gpt-6-astra","usage":{"input_tokens":1,"output_tokens":1}}}`))
	conn.readDelays = append(conn.readDelays, 600*time.Millisecond)
	svc, account, c, rec, _ := astraHTTPBridgeFixture(t, conn)
	result, err := svc.Forward(context.Background(), c, account, []byte(`{"model":"gpt-6-astra","stream":true,"input":"synthetic"}`))
	require.NoError(t, err)
	require.Nil(t, result.FirstTokenMs)
	require.GreaterOrEqual(t, strings.Count(rec.Body.String(), ": ping\n\n"), 2)
}

func TestAstraHTTPBridge_FailedHeartbeatCancelsInFlightRead(t *testing.T) {
	ticks := make(chan time.Time, 1)
	ticks <- time.Now()
	readStopped := make(chan struct{})
	_, err := readOpenAIWSWithHeartbeat(context.Background(), time.Hour, ticks, func(ctx context.Context, _ time.Duration) ([]byte, error) {
		<-ctx.Done()
		close(readStopped)
		return nil, ctx.Err()
	}, func() bool { return false })
	require.ErrorIs(t, err, context.Canceled)
	select {
	case <-readStopped:
	default:
		t.Fatal("in-flight reader must stop before helper returns")
	}
}

func TestAstraHTTPBridge_RejectsOtherServerHistoryReferences(t *testing.T) {
	for _, tc := range []struct{ name, extra, param string }{
		{"conversation_string", `"conversation":"conv_other"`, "conversation"},
		{"conversation_object", `"conversation":{"id":"conv_other"}`, "conversation"},
		{"item_reference", `"input":[{"type":"item_reference","id":"msg_other"}]`, "input.item_reference"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, account, c, rec, dialer := astraHTTPBridgeFixture(t, &openAIWSCaptureConn{})
			body := `{"model":"gpt-6-astra","stream":true,` + tc.extra + `}`
			_, err := svc.Forward(context.Background(), c, account, []byte(body))
			require.Error(t, err)
			require.Equal(t, 400, rec.Code)
			require.Contains(t, rec.Body.String(), tc.param)
			require.True(t, IsResponseCommitted(c))
			require.Zero(t, dialer.DialCount())
			require.Nil(t, svc.httpUpstream.(*httpUpstreamRecorder).lastReq)
		})
	}
}

func TestAstraHTTPBridge_DoesNotPrewarmPrivateRequestScope(t *testing.T) {
	conn := &openAIWSCaptureConn{events: [][]byte{[]byte(`{"type":"response.completed","response":{"id":"resp_no_prewarm","model":"gpt-6-astra","usage":{"input_tokens":1,"output_tokens":1}}}`)}}
	svc, account, c, _, dialer := astraHTTPBridgeFixture(t, conn)
	svc.cfg.Gateway.OpenAIWS.MaxConnsPerAccount = 8
	svc.cfg.Gateway.OpenAIWS.MinIdlePerAccount = 4
	svc.cfg.Gateway.OpenAIWS.MaxIdlePerAccount = 4
	_, err := svc.Forward(context.Background(), c, account, []byte(`{"model":"gpt-6-astra","stream":true,"input":"synthetic"}`))
	require.NoError(t, err)
	require.Equal(t, 1, dialer.DialCount(), "private scope must not create idle prewarm connections")
	ap, ok := svc.openaiWSPool.getAccountPool(account.ID)
	require.True(t, ok)
	ap.mu.Lock()
	count := len(ap.conns)
	cached := ap.lastAcquire
	active := ap.prewarmActive
	ap.mu.Unlock()
	require.Zero(t, count)
	require.Nil(t, cached)
	require.False(t, active)
}

func TestAstraHTTPBridge_AcknowledgedUsageDoesNotReplayBeforeFirstHeartbeat(t *testing.T) {
	conn := &openAIWSCaptureConn{events: [][]byte{[]byte(`{"type":"response.in_progress","response":{"id":"resp_acknowledged","usage":{"input_tokens":5,"output_tokens":2}}}`)}}
	svc, account, c, rec, dialer := astraHTTPBridgeFixture(t, conn)
	result, err := svc.Forward(context.Background(), c, account, []byte(`{"model":"gpt-6-astra","stream":true,"input":"synthetic"}`))
	require.Error(t, err)
	require.NotNil(t, result)
	require.Equal(t, 5, result.Usage.InputTokens)
	require.Equal(t, 2, result.Usage.OutputTokens)
	require.Equal(t, 1, dialer.DialCount())
	require.Nil(t, svc.httpUpstream.(*httpUpstreamRecorder).lastReq)
	require.NotContains(t, rec.Body.String(), ": ping")
	require.Nil(t, result.FirstTokenMs)
	require.Contains(t, rec.Header().Get("Content-Type"), "application/json")
}

func TestAstraHTTPBridge_CanaryKeyIsEligible(t *testing.T) {
	conn := &openAIWSCaptureConn{events: [][]byte{[]byte(`{"type":"response.completed","response":{"id":"resp_canary","model":"gpt-6-astra","usage":{"input_tokens":1,"output_tokens":1}}}`)}}
	svc, account, c, _, dialer := astraHTTPBridgeFixture(t, conn)
	svc.cfg.Gateway.OpenAIWS.AstraHTTPBridgeAPIKeyIDs = []int64{11}
	result, err := svc.Forward(context.Background(), c, account, []byte(`{"model":"gpt-6-astra","stream":true,"input":"synthetic"}`))
	require.NoError(t, err)
	require.True(t, result.OpenAIWSMode)
	require.Equal(t, 1, dialer.DialCount())
}

func TestAstraHTTPBridge_NestedFailedCodeAndKnownUsageArePreserved(t *testing.T) {
	source := []byte(`{"type":"response.failed","sequence_number":7,"response":{"id":"resp_upstream_failed","object":"response","created_at":100,"status":"failed","model":"gpt-6-astra","output":[],"error":{"type":"invalid_request_error","code":"context_length_exceeded","message":"synthetic rejection"},"usage":{"input_tokens":5,"output_tokens":2}}}`)
	payload := buildAstraHTTPBridgeFailedEvent("resp_upstream_failed", "gpt-6-astra", source, 8)
	require.Equal(t, "context_length_exceeded", gjson.GetBytes(payload, "response.error.code").String())
	require.Equal(t, "invalid_request_error", gjson.GetBytes(payload, "response.error.type").String())
	conn := &openAIWSCaptureConn{events: [][]byte{source}}
	svc, account, c, rec, _ := astraHTTPBridgeFixture(t, conn)
	result, err := svc.Forward(context.Background(), c, account, []byte(`{"model":"gpt-6-astra","stream":true,"input":"synthetic"}`))
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, 5, result.Usage.InputTokens)
	require.Equal(t, 2, result.Usage.OutputTokens)
	require.Equal(t, 1, strings.Count(rec.Body.String(), `"type":"response.failed"`))
	require.Contains(t, rec.Body.String(), "context_length_exceeded")
	require.True(t, IsResponseCommitted(c))
}

func TestAstraHTTPBridge_CancelBeforeFirstHeartbeatDoesNotWriteFailure(t *testing.T) {
	conn := &openAIWSCaptureConn{events: [][]byte{[]byte(`{"type":"response.created","response":{"id":"resp_canceled_before_commit"}}`)}, readDelays: []time.Duration{5 * time.Second}}
	svc, account, c, rec, _ := astraHTTPBridgeFixture(t, conn)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.Request = c.Request.WithContext(ctx)
	timer := time.AfterFunc(100*time.Millisecond, cancel)
	defer timer.Stop()
	result, err := svc.Forward(ctx, c, account, []byte(`{"model":"gpt-6-astra","stream":true,"input":"synthetic"}`))
	require.ErrorIs(t, err, context.Canceled)
	require.NotNil(t, result)
	require.True(t, result.ClientDisconnect)
	require.Zero(t, result.Usage.InputTokens)
	require.Zero(t, result.Usage.OutputTokens)
	require.Empty(t, rec.Body.String())
	require.False(t, c.Writer.Written())
	require.True(t, conn.closed)
}
