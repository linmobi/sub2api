package service

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOpenAIWSErrorHTTPStatus_SemanticEvents(t *testing.T) {
	tests := []struct {
		name      string
		errorJSON string
		topFields string
		wantHTTP  int
		wantCool  int
	}{
		{"explicit_server_500", `"code":"server_error","type":"server_error","status":500`, "", 500, 500},
		{"explicit_server_503", `"code":"server_error","status_code":503`, "", 503, 503},
		{"explicit_429_over_server_code", `"code":"server_error","status":429`, "", 429, 0},
		{"explicit_top_status", `"code":"server_error"`, `,"status":503`, 503, 503},
		{"explicit_top_status_code", `"code":"server_error"`, `,"status_code":500`, 500, 500},
		{"nested_status_wins_top_level", `"code":"server_error","status_code":429`, `,"status":503`, 429, 0},
		{"invalid_nested_uses_valid_top_level", `"code":"server_error","status":200`, `,"status":503`, 503, 503},
		{"implicit_overload", `"code":"server_is_overloaded","type":"service_unavailable_error"`, "", 503, 503},
		{"implicit_unavailable_type", `"type":"service_unavailable_error"`, "", 503, 503},
		{"implicit_server_keeps_gateway_status", `"code":"server_error","type":"server_error"`, "", 502, 500},
		{"invalid_string_uses_overload_inference", `"code":"server_is_overloaded","status":"500"`, "", 503, 503},
		{"invalid_fraction_uses_server_inference", `"code":"server_error","status":503.5`, "", 502, 500},
		{"invalid_success_uses_server_inference", `"code":"server_error","status":200`, "", 502, 500},
		{"invalid_range_uses_server_inference", `"code":"server_error","status_code":700`, "", 502, 500},
		{"explicit_529_keeps_capacity_path", `"code":"server_error","status_code":529`, "", 529, 0},
	}
	for _, tc := range tests {
		for _, eventType := range []string{"error", "response.failed"} {
			t.Run(tc.name+"/"+eventType, func(t *testing.T) {
				errorJSON := `{"message":"synthetic upstream rejection",` + tc.errorJSON + `}`
				payload := fmt.Sprintf(`{"type":"error","error":%s%s}`, errorJSON, tc.topFields)
				if eventType == "response.failed" {
					payload = fmt.Sprintf(`{"type":"response.failed","response":{"status":"failed","error":%s}%s}`, errorJSON, tc.topFields)
				}
				require.Equal(t, tc.wantHTTP, openAIWSErrorHTTPStatus([]byte(payload)))
				require.Equal(t, tc.wantCool, openAIWSPayloadTransientStatus([]byte(payload)))
			})
		}
	}
}
