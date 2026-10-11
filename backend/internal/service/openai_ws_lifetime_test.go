//go:build unit

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

func TestOpenAIWSLifetimeRotation_ProtectsExternalAndIncompleteHistory(t *testing.T) {
	for _, tc := range []struct {
		name, payload, input   string
		complete, stored, safe bool
	}{
		{"external_response", `{"previous_response_id":"resp_external"}`, `[{"role":"user","content":"delta"}]`, false, false, false},
		{"orphan_tool_result", `{"previous_response_id":"resp_last"}`, `[{"type":"custom_tool_call_output","call_id":"unknown","output":"ok"}]`, true, false, false},
		{"item_reference", `{"previous_response_id":"resp_last"}`, `[{"type":"item_reference","id":"item_external"}]`, true, false, false},
		{"conversation_reference", `{"conversation":"conv_external"}`, `[]`, true, false, false},
		{"persisted_response", `{"previous_response_id":"resp_stored","input":[]}`, `[]`, false, true, true},
		{"fresh_root", `{"input":[]}`, `[]`, true, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var input []json.RawMessage
			require.NoError(t, json.Unmarshal([]byte(tc.input), &input))
			body, safe, err := prepareOpenAIWSLifetimeRotationPayload([]byte(tc.payload), input, true, tc.complete, !tc.stored)
			require.NoError(t, err)
			require.Equal(t, tc.safe, safe)
			if !safe || tc.stored {
				require.JSONEq(t, tc.payload, string(body))
			}
		})
	}
}

func TestOpenAIWSLifetimeRotation_KeepsAssistantAndToolContextOnFreshConnection(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{}
	cfg.Security.URLAllowlist.AllowInsecureHTTP = true
	cfg.Gateway.OpenAIWS.Enabled = true
	cfg.Gateway.OpenAIWS.APIKeyEnabled = true
	cfg.Gateway.OpenAIWS.ResponsesWebsocketsV2 = true
	cfg.Gateway.OpenAIWS.MaxConnsPerAccount = 1
	cfg.Gateway.OpenAIWS.MaxIdlePerAccount = 1
	cfg.Gateway.OpenAIWS.ReadTimeoutSeconds = 3
	cfg.Gateway.OpenAIWS.WriteTimeoutSeconds = 3
	cfg.Gateway.OpenAIWS.DialTimeoutSeconds = 3
	first := &openAIWSCaptureConn{events: [][]byte{[]byte(`{"type":"response.completed","response":{"id":"resp_first","model":"gpt-6.1-sol","output":[{"type":"message","id":"msg_first","role":"assistant","content":[{"type":"output_text","text":"Remember the original context."}]},{"type":"function_call","id":"fc_first","call_id":"call_first","name":"lookup","arguments":"{}"}]}}`)}}
	second := &openAIWSCaptureConn{events: [][]byte{[]byte(`{"type":"response.completed","response":{"id":"resp_second","model":"gpt-6.1-sol","output":[]}}`)}}
	dialer := &openAIWSQueueDialer{conns: []openAIWSClientConn{first, second}}
	pool := newOpenAIWSConnPool(cfg)
	defer pool.Close()
	pool.setClientDialerForTest(dialer)
	svc := &OpenAIGatewayService{cfg: cfg, httpUpstream: &httpUpstreamRecorder{}, cache: &stubGatewayCache{}, openaiWSResolver: NewOpenAIWSProtocolResolver(cfg), toolCorrector: NewCodexToolCorrector(), openaiWSPool: pool}
	account := &Account{ID: 777, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 1, Credentials: map[string]any{"api_key": "sk-test"}, Extra: map[string]any{"responses_websockets_v2_enabled": true}}
	serverErr := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := coderws.Accept(w, r, nil)
		if err != nil {
			serverErr <- err
			return
		}
		defer conn.CloseNow()
		_, body, err := conn.Read(r.Context())
		if err != nil {
			serverErr <- err
			return
		}
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = r
		hooks := &OpenAIWSIngressHooks{BeforeTurn: func(turn int) error {
			if turn == 2 {
				v, ok := pool.accounts.Load(account.ID)
				if ok {
					ap := v.(*openAIWSAccountPool)
					ap.mu.Lock()
					for _, c := range ap.conns {
						c.createdAtNano.Store(time.Now().Add(-31 * time.Minute).UnixNano())
					}
					ap.mu.Unlock()
				}
			}
			return nil
		}}
		serverErr <- svc.ProxyResponsesWebSocketFromClient(r.Context(), c, conn, account, "sk-test", body, hooks)
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client, _, err := coderws.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
	require.NoError(t, err)
	defer client.CloseNow()
	require.NoError(t, client.Write(ctx, coderws.MessageText, []byte(`{"type":"response.create","model":"gpt-6.1-sol","store":false,"input":[{"role":"user","content":"Original request"}],"tools":[{"type":"function","name":"lookup","parameters":{"type":"object","properties":{}}}]}`)))
	_, event, err := client.Read(ctx)
	require.NoError(t, err)
	require.Equal(t, "resp_first", gjson.GetBytes(event, "response.id").String())
	require.NoError(t, client.Write(ctx, coderws.MessageText, []byte(`{"type":"response.create","model":"gpt-6.1-sol","store":false,"previous_response_id":"resp_first","input":[{"type":"function_call_output","call_id":"call_first","output":"tool result"}]}`)))
	_, event, err = client.Read(ctx)
	require.NoError(t, err)
	require.Equal(t, "resp_second", gjson.GetBytes(event, "response.id").String())
	client.Close(coderws.StatusNormalClosure, "done")
	select {
	case err := <-serverErr:
		require.NoError(t, err)
	case <-ctx.Done():
		t.Fatal("session did not finish")
	}
	require.Equal(t, 2, dialer.DialCount())
	first.mu.Lock()
	require.Len(t, first.writes, 1)
	first.mu.Unlock()
	second.mu.Lock()
	defer second.mu.Unlock()
	require.Len(t, second.writes, 1)
	require.NotContains(t, second.lastWrite, "previous_response_id")
	b, err := json.Marshal(second.lastWrite)
	require.NoError(t, err)
	items := gjson.GetBytes(b, "input").Array()
	require.Len(t, items, 4)
	require.Equal(t, "Original request", items[0].Get("content").String())
	require.Equal(t, "Remember the original context.", items[1].Get("content.0.text").String())
	require.Equal(t, "function_call", items[2].Get("type").String())
	require.Equal(t, "call_first", items[3].Get("call_id").String())
}
