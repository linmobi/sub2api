# Astra HTTP/SSE to WSv2 experiment

This is a default-off experiment for the observed OpenAI HTTP stream closure
near 901 seconds. It is not evidence that WebSocket generation will outlast
that limit. Validate in isolation before an explicitly limited canary, then
complete a real long request before describing the interruption as fixed.

The bridge applies only to HTTP `/v1/responses` (including compatible prefixed
Responses routes), exact `gpt-6-astra` with no model remapping, and explicitly
opted-in OpenAI OAuth accounts. Existing HTTP paths, Chat Completions,
passthrough, compact requests, other models and other credentials retain their
current transport.

Required experimental global setting (default false):

```yaml
gateway:
  openai_ws:
    astra_http_bridge_enabled: true
    astra_http_bridge_api_key_ids: [1]
    astra_http_bridge_read_timeout_seconds: 3600
    astra_http_bridge_heartbeat_seconds: 15
```

Environment equivalents use `GATEWAY_OPENAI_WS_ASTRA_HTTP_BRIDGE_ENABLED`,
`GATEWAY_OPENAI_WS_ASTRA_HTTP_BRIDGE_READ_TIMEOUT_SECONDS` and
`GATEWAY_OPENAI_WS_ASTRA_HTTP_BRIDGE_HEARTBEAT_SECONDS`.
The optional canary allowlist uses `GATEWAY_OPENAI_WS_ASTRA_HTTP_BRIDGE_API_KEY_IDS`
(comma-separated IDs). Empty allows all keys on explicitly opted-in accounts;
the shown `[1]` restricts an experiment to API key 1. Other keys keep HTTP.

The account must separately contain both extra flags:

```json
{
  "openai_astra_http_ws_bridge_enabled": true,
  "openai_oauth_responses_websockets_v2_enabled": true
}
```

Existing global WSv2 and OAuth gates still apply; global/account force-HTTP
continues to win. Changing the experiment flag does not authorize changing
account credentials or inference settings.

Each experimental request carries its full input, forces `store=false`, uses
a new upstream WebSocket, and closes it after success or failure. Its private
scope includes the authenticated API key, upstream account, any original
execution scope, and a random request ID. No cached turn state, connection or
continuation anchor is reused. `previous_response_id`, non-null `conversation`,
and input `item_reference` items are explicitly rejected with pure HTTP 400 JSON.
Completed response IDs retain the HTTP owner binding,
but they are not resumable inside this prototype.

The SSE writer stays on the forwarding goroutine. Only the blocking upstream
read runs in a worker. A request-wide timer emits comments every 15 seconds,
including before the first token and during frequent hidden progress events.
Comments never count as semantic output, tokens or TTFT. Once comments have
committed HTTP 200, the prototype conservatively stops automatic request
replay; a later failure is delivered as a single strict Responses `response.failed`
terminal event, with monotonic sequence number, required response fields, committed and
operations failure markers. Confirmed partial usage is returned to the caller;
unknown usage is left unknown. Existing 900-second WS ingress deadlines remain
unchanged; only opted-in bridge reads use the experiment's 3600-second limit.
The total drain budget after downstream cancellation retains the existing WS
read-timeout setting rather than extending to one hour.
Acknowledged response IDs or known usage also stop implicit retry before any
heartbeat, avoiding duplicate generations and discarded first-attempt usage.
Pool idle prewarming is suppressed for private bridge acquires, without
changing the shared pool's global minimum idle configuration.
Fresh sockets and request-private cache keys intentionally trade away some
cache/connection reuse during the experiment. Measure that cost before any
permanent rollout.

Mock validation:

```sh
cd backend
GOMAXPROCS=1 GOGC=20 GOMEMLIMIT=1400MiB go test -p 1 -gcflags='all=-l' \
  ./internal/config ./internal/service ./internal/handler \
  -run 'TestAstraHTTPBridge|TestOpenAIWS|Test.*WS[vV]2|Test.*OpenAIClientTransport|TestResolveOpenAIWSDecisionByClientTransport|TestOpenAIGatewayService_ProxyResponsesWebSocketFromClient|TestOpenAIGatewayService_PrewarmReadHonorsParentContext|TestLoadDefaultOpenAIWSConfig|TestLoadOpenAIWSForceHTTPFromEnv' \
  -count=1 -timeout=300s
```

Tests exercise ineligible HTTP requests, delayed first output, sustained
keepalive, terminal delivery, HTTP owner isolation, continuation rejection,
fresh connection closure, confirmed partial usage, cancellation both between
reads and during keepalive, and the HTTP handler's terminal-error contract.
These tests use synthetic input and mock upstream connections. They do not
call real accounts or prove an upstream long-request deadline is removed.

On 2026-10-04, the selected configuration, gateway, handler and existing
WebSocket regressions passed: 282 top-level tests, 424 pass events including
subtests, with zero failures. The resource-limited test container exited 0
without OOM. An additional temporary harness exercised this exact candidate
against the real ChatGPT OAuth WebSocket upstream using synthetic `17 + 23`,
`max` effort and `standard` mode. It returned one completed event and answer
`40` in 3.064 seconds, with HTTP response owner binding and no surviving
private pooled connection. Its credentials were read only inside the server
from a protected temporary file, then that file and harness were removed.
The live test did not modify the production account or application settings.
This verifies short-request compatibility only; a real long request remains
required to establish whether the original 901-second interruption is avoided.

The final selected regression run used Go 1.27.1 in a container capped at one
CPU and 2 GiB memory, with memory-swap also capped at 2 GiB. It exited 0 without
OOM on 2026-10-04: 282 top-level tests passed (424 pass events including
subtests), including 21 top-level bridge tests. Config, service and handler
packages all passed. Earlier ordinary compilation exceeded the container
memory limit before service assertions ran; disabling Go compiler inlining
with `all=-l` made the final run fit. This is a compiler resource setting, not
an upstream model, reasoning or quality setting. Use matching compilation
flags to reuse this experiment's build cache. A deployable binary must also
use the repository's `embed` build tag and the current custom frontend assets.

Roll back the experiment by disabling its global flag. Account extras can also
be removed. Keep the deployment's other environment variables and database
unchanged.
