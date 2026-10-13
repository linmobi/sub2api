package service

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestCtxPoolHTTPBridge_ExplicitStatusWithoutReplay(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, status := range []int{429, 500, 503} {
			t.Run(fmt.Sprintf("stream%t/status%d", stream, status), func(t *testing.T) {
				conn := &openAIWSCaptureConn{events: [][]byte{[]byte(fmt.Sprintf(`{"type":"error","error":{"code":"server_error","type":"server_error","status":%d,"message":"synthetic rejection"}}`, status))}}
				svc, account, c, rec, dialer := astraHTTPBridgeFixture(t, conn)
				svc.cfg.Gateway.OpenAIWS.CtxPoolHTTPBridgeEnabled = true
				svc.cfg.Gateway.OpenAIWS.AstraHTTPBridgeEnabled = false
				account.Extra["openai_oauth_responses_websockets_v2_mode"] = "ctx_pool"
				body := fmt.Sprintf(`{"model":"gpt-6.1-sol","stream":%t,"reasoning":{"effort":"max"},"input":"%s"}`, stream, strings.Repeat("synthetic history ", 4096))
				_, err := svc.Forward(context.Background(), c, account, []byte(body))
				require.Error(t, err)
				require.Equal(t, status, rec.Code)
				require.Contains(t, rec.Header().Get("Content-Type"), "application/json")
				require.True(t, gjson.Valid(rec.Body.String()))
				require.Equal(t, "server_error", gjson.Get(rec.Body.String(), "error.code").String())
				require.Equal(t, "server_error", gjson.Get(rec.Body.String(), "error.type").String())
				require.NotContains(t, rec.Body.String(), "upstream_stream_interrupted")
				require.Equal(t, 1, dialer.DialCount())
				require.Nil(t, svc.httpUpstream.(*httpUpstreamRecorder).lastReq)
			})
		}
	}
}
