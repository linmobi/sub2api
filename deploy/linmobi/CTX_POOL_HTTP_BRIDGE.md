# OpenAI ctx_pool 自动 HTTP → WebSocket 桥接

开启 OpenAI OAuth 订阅账号的 `ctx_pool` 后，完整输入的 `/v1/responses`
请求自动使用原 Astra 长请求修复通道，适用于所有上游支持的 Responses 模型
和所有已经通过正常鉴权、分组、配额及模型访问检查的客户端 API Key。
不再需要 Astra 账号额外开关、特定模型名称或 API Key 白名单。

账号透传应保持关闭。全局 WS 禁用、强制 HTTP、账号 WS 关闭时不桥接。
Claude、Gemini、Chat Completions、compact 接口及客户端原生 WebSocket 保持原有路径。
此功能改变传输方式，不扩大上游账号的模型许可或支持范围。

每次 HTTP 请求使用独立上游连接、独立会话标识及 `store=false`；结束后销毁连接。
流式响应定期发送 SSE 心跳，非流式响应返回完整 JSON。
上游已接受请求或下游已提交数据后不隐式重放，避免重复调用及计费。
`previous_response_id`、`conversation`、`input.item_reference` 请求保留 HTTP
处理，避免在新建私有连接中丢失上游保存的上下文。

配置 `gateway.openai_ws.ctx_pool_http_bridge_enabled` 默认 `true`，对应环境变量
`GATEWAY_OPENAI_WS_CTX_POOL_HTTP_BRIDGE_ENABLED`。旧 `astra_http_bridge_*`
超时及心跳配置继续使用，默认读取超时 3600 秒、心跳 15 秒。
旧 Astra 实验配置仍兼容；如需完全禁用两种 HTTP 桥接，同时关闭
`ctx_pool_http_bridge_enabled` 和 `astra_http_bridge_enabled`。

连接稳定性修复不能保证上游永不报错，也不会将上游不支持的模型变为可用模型。
