package handler

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// Service mock tests cover real bridge output and its committed flag. This
// exercises the actual HTTP handler's fallback contract for those outputs.
func TestAstraHTTPBridge_HandlerDoesNotAppendFailureAfterCommittedResponse(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name   string
		status int
		body   string
		stream bool
	}{
		{"keepalive_then_error", 200, ": ping\n\ndata: {\"type\":\"error\",\"error\":{\"code\":\"upstream_stream_interrupted\"}}\n\n", true},
		{"upstream_stream_error", 200, "data: {\"type\":\"error\",\"error\":{\"code\":\"invalid_request_error\"}}\n\n", true},
		{"upstream_json_error", 400, "{\"error\":{\"message\":\"synthetic upstream error\"}}", false},
		{"continuation_rejection", 400, "{\"error\":{\"param\":\"previous_response_id\"}}", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			c.Writer.WriteHeader(tc.status)
			_, err := c.Writer.Write([]byte(tc.body))
			require.NoError(t, err)
			service.MarkResponseCommitted(c)
			h := &OpenAIGatewayHandler{}
			require.False(t, h.ensureForwardErrorResponse(c, tc.stream))
			require.Equal(t, tc.body, rec.Body.String(), "no extra SSE may be appended to an already delivered error")
			require.Equal(t, tc.status, rec.Code)
		})
	}
}
