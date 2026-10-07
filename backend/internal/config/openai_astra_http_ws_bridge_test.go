package config

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAstraHTTPBridge_DefaultDisabledKeepsLegacyDeadline(t *testing.T) {
	resetViperWithJWTSecret(t)
	cfg, err := Load()
	require.NoError(t, err)
	require.False(t, cfg.Gateway.OpenAIWS.AstraHTTPBridgeEnabled)
	require.True(t, cfg.Gateway.OpenAIWS.CtxPoolHTTPBridgeEnabled)
	require.Equal(t, 900, cfg.Gateway.OpenAIWS.ReadTimeoutSeconds)
	require.Equal(t, 3600, cfg.Gateway.OpenAIWS.AstraHTTPBridgeReadTimeoutSeconds)
	require.Equal(t, 15, cfg.Gateway.OpenAIWS.AstraHTTPBridgeHeartbeatSeconds)
}

func TestCtxPoolHTTPBridge_ExplicitDisable(t *testing.T) {
	resetViperWithJWTSecret(t)
	t.Setenv("GATEWAY_OPENAI_WS_CTX_POOL_HTTP_BRIDGE_ENABLED", "false")
	cfg, err := Load()
	require.NoError(t, err)
	require.False(t, cfg.Gateway.OpenAIWS.CtxPoolHTTPBridgeEnabled)
}

func TestAstraHTTPBridge_ExplicitEnvAndInvalidDeadline(t *testing.T) {
	t.Run("explicit_opt_in", func(t *testing.T) {
		resetViperWithJWTSecret(t)
		t.Setenv("GATEWAY_OPENAI_WS_ASTRA_HTTP_BRIDGE_ENABLED", "true")
		t.Setenv("GATEWAY_OPENAI_WS_ASTRA_HTTP_BRIDGE_API_KEY_IDS", "11,12")
		t.Setenv("GATEWAY_OPENAI_WS_ASTRA_HTTP_BRIDGE_READ_TIMEOUT_SECONDS", "1800")
		t.Setenv("GATEWAY_OPENAI_WS_ASTRA_HTTP_BRIDGE_HEARTBEAT_SECONDS", "10")
		cfg, err := Load()
		require.NoError(t, err)
		require.True(t, cfg.Gateway.OpenAIWS.AstraHTTPBridgeEnabled)
		require.Equal(t, []int64{11, 12}, cfg.Gateway.OpenAIWS.AstraHTTPBridgeAPIKeyIDs)
		require.Equal(t, 1800, cfg.Gateway.OpenAIWS.AstraHTTPBridgeReadTimeoutSeconds)
		require.Equal(t, 10, cfg.Gateway.OpenAIWS.AstraHTTPBridgeHeartbeatSeconds)
	})
	t.Run("invalid_deadline", func(t *testing.T) {
		resetViperWithJWTSecret(t)
		t.Setenv("GATEWAY_OPENAI_WS_ASTRA_HTTP_BRIDGE_ENABLED", "true")
		t.Setenv("GATEWAY_OPENAI_WS_ASTRA_HTTP_BRIDGE_READ_TIMEOUT_SECONDS", "0")
		_, err := Load()
		require.ErrorContains(t, err, "positive")
	})
}
