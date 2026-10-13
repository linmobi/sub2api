package service

import (
	"context"
	"encoding/json"
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

type openAIWSIngressDocumentTurn struct {
	result *OpenAIForwardResult
	err    error
}

type openAIWSIngressDocumentFixture struct {
	client    *coderws.Conn
	upstream  *openAIWSCaptureConn
	dialer    *openAIWSCaptureDialer
	serverErr chan error
	turns     chan openAIWSIngressDocumentTurn
}

func newOpenAIWSIngressDocumentFixture(t *testing.T, frames ...string) *openAIWSIngressDocumentFixture {
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
	upstream := &openAIWSCaptureConn{}
	for _, frame := range frames {
		upstream.events = append(upstream.events, []byte(frame))
	}
	dialer := &openAIWSCaptureDialer{conn: upstream}
	pool := newOpenAIWSConnPool(cfg)
	pool.setClientDialerForTest(dialer)
	t.Cleanup(pool.Close)
	svc := &OpenAIGatewayService{cfg: cfg, cache: &stubGatewayCache{}, httpUpstream: &httpUpstreamRecorder{},
		openaiWSResolver: NewOpenAIWSProtocolResolver(cfg), openaiWSPool: pool, toolCorrector: NewCodexToolCorrector()}
	account := &Account{ID: 192, Name: "synthetic-ingress-documents", Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
		Status: StatusActive, Schedulable: true, Concurrency: 1,
		Credentials: map[string]any{"api_key": "sk-test"}, Extra: map[string]any{"responses_websockets_v2_enabled": true}}
	fixture := &openAIWSIngressDocumentFixture{upstream: upstream, dialer: dialer,
		serverErr: make(chan error, 1), turns: make(chan openAIWSIngressDocumentTurn, 2)}
	hooks := &OpenAIWSIngressHooks{AfterTurn: func(_ int, result *OpenAIForwardResult, err error) {
		fixture.turns <- openAIWSIngressDocumentTurn{result: result, err: err}
	}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := coderws.Accept(w, r, nil)
		if err != nil {
			fixture.serverErr <- err
			return
		}
		defer func() { _ = conn.CloseNow() }()
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = r
		readCtx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		_, firstMessage, err := conn.Read(readCtx)
		cancel()
		if err != nil {
			fixture.serverErr <- err
			return
		}
		fixture.serverErr <- svc.ProxyResponsesWebSocketFromClient(r.Context(), c, conn, account, "sk-test", firstMessage, hooks)
	}))
	t.Cleanup(server.Close)
	dialCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	client, _, err := coderws.Dial(dialCtx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
	cancel()
	require.NoError(t, err)
	fixture.client = client
	t.Cleanup(func() { _ = client.CloseNow() })
	writeCtx, cancelWrite := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancelWrite()
	require.NoError(t, client.Write(writeCtx, coderws.MessageText,
		[]byte(`{"type":"response.create","model":"gpt-6-astra","stream":true,"input":"synthetic input"}`)))
	return fixture
}

func (f *openAIWSIngressDocumentFixture) read(t *testing.T) ([]byte, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, message, err := f.client.Read(ctx)
	return message, err
}

func (f *openAIWSIngressDocumentFixture) completed(t *testing.T) *OpenAIForwardResult {
	t.Helper()
	select {
	case turn := <-f.turns:
		require.NoError(t, turn.err)
		require.NotNil(t, turn.result)
		return turn.result
	case <-time.After(3 * time.Second):
		t.Fatal("ingress did not observe terminal")
		return nil
	}
}

func (f *openAIWSIngressDocumentFixture) stop(t *testing.T, wantError bool) {
	t.Helper()
	_ = f.client.CloseNow()
	select {
	case err := <-f.serverErr:
		if wantError {
			require.ErrorContains(t, err, "malformed Responses event JSON")
		} else {
			require.NoError(t, err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("ingress session did not stop")
	}
	require.Empty(t, f.turns, "terminal/error must be accounted once")
	require.Equal(t, 1, f.dialer.DialCount(), "malformed transport data must not replay generation")
	f.upstream.mu.Lock()
	defer f.upstream.mu.Unlock()
	require.Len(t, f.upstream.writes, 1, "send only the original generation")
}

const openAIWSIngressAddedDocument = `{"type":"response.output_item.added","sequence_number":1,"item":{"id":"msg_synthetic","type":"message","role":"assistant","status":"in_progress","content":[]}}`
const openAIWSIngressDoneDocument = `{"type":"response.output_item.done","sequence_number":2,"item":{"id":"msg_synthetic","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"synthetic answer","annotations":[]}]}}`
const openAIWSIngressCompletedDocument = `{"type":"response.completed","sequence_number":3,"response":{"id":"resp_synthetic_documents","object":"response","model":"gpt-6-astra","status":"completed","output":[],"usage":{"input_tokens":11,"output_tokens":7}}}`

func TestOpenAIWSIngressJSONDocuments_ConcatenatedFramePreservesOrderedEventsAndUsage(t *testing.T) {
	f := newOpenAIWSIngressDocumentFixture(t, openAIWSIngressAddedDocument+"\n"+openAIWSIngressDoneDocument+openAIWSIngressCompletedDocument)
	for i, eventType := range []string{"response.output_item.added", "response.output_item.done", "response.completed"} {
		message, err := f.read(t)
		require.NoError(t, err)
		require.True(t, json.Valid(message), "each downstream message must be one valid JSON event")
		require.Equal(t, eventType, gjson.GetBytes(message, "type").String())
		require.Equal(t, int64(i+1), gjson.GetBytes(message, "sequence_number").Int())
	}
	result := f.completed(t)
	require.Equal(t, "response.completed", result.UpstreamTerminalEvent)
	require.Equal(t, "resp_synthetic_documents", result.RequestID)
	require.Equal(t, 11, result.Usage.InputTokens)
	require.Equal(t, 7, result.Usage.OutputTokens)
	f.stop(t, false)
}

func TestOpenAIWSIngressJSONDocuments_RejectEntireMalformedFrameWithoutReplay(t *testing.T) {
	for _, tc := range []struct{ name, frame string }{
		{"junk_after_terminal", openAIWSIngressAddedDocument + openAIWSIngressCompletedDocument + "unexpected-tail"},
		{"truncated_second_document", openAIWSIngressAddedDocument + `{"type":"response.completed"`},
		{"document_count_limit", strings.Repeat(`{"type":"response.in_progress"}`, maxOpenAIConcatenatedJSONDocuments) + openAIWSIngressCompletedDocument},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newOpenAIWSIngressDocumentFixture(t, tc.frame)
			message, err := f.read(t)
			require.Error(t, err, "do not forward any prefix of a malformed frame")
			require.Empty(t, message)
			select {
			case turn := <-f.turns:
				require.Nil(t, turn.result)
				require.ErrorContains(t, turn.err, "malformed Responses event JSON")
			case <-time.After(3 * time.Second):
				t.Fatal("malformed frame did not finish the turn")
			}
			f.stop(t, true)
		})
	}
}

func TestOpenAIWSIngressJSONDocuments_TerminalTailPoisonsConnection(t *testing.T) {
	f := newOpenAIWSIngressDocumentFixture(t, openAIWSIngressCompletedDocument+`{"type":"error","error":{"code":"server_error","message":"synthetic tail"}}`)
	message, err := f.read(t)
	require.NoError(t, err)
	require.True(t, json.Valid(message))
	require.Equal(t, "response.completed", gjson.GetBytes(message, "type").String())
	f.completed(t)
	f.upstream.mu.Lock()
	closed := f.upstream.closed
	f.upstream.mu.Unlock()
	require.True(t, closed, "data after terminal makes the upstream connection unsafe to reuse")
	f.stop(t, false)
}

func TestOpenAIWSIngressJSONDocuments_MalformedFrameAfterOutputDoesNotReplay(t *testing.T) {
	f := newOpenAIWSIngressDocumentFixture(t, openAIWSIngressAddedDocument, openAIWSIngressDoneDocument+"trailing-junk")
	message, err := f.read(t)
	require.NoError(t, err)
	require.Equal(t, "response.output_item.added", gjson.GetBytes(message, "type").String())
	message, err = f.read(t)
	require.Error(t, err)
	require.Empty(t, message, "do not forward corrupt output after a valid event")
	select {
	case turn := <-f.turns:
		require.Nil(t, turn.result)
		require.ErrorContains(t, turn.err, "malformed Responses event JSON")
	case <-time.After(3 * time.Second):
		t.Fatal("malformed post-output frame did not finish the turn")
	}
	f.stop(t, true)
}
