# lin.mobi 实例公网部署配置

本目录记录已部署实例的公网代理配置。它不修改 Sub2API 业务源码。

## 域名与路径

| 域名 | 允许访问 | 被阻止的内容 |
| --- | --- | --- |
| `ai.lin.mobi` | 面板、静态资源、登录和 `/api/v1` 后台接口 | 模型调用接口返回 JSON 404 |
| `api.ai.lin.mobi` | OpenAI、Anthropic、Gemini、Antigravity 等模型网关接口及已有兼容路径 | 面板、静态资源、登录和 `/api/v1` 后台接口返回 JSON 404 |
| 其他 `*.ai.lin.mobi` | 无 | Nginx 直接关闭连接，不返回内容或跳转 |

客户端 OpenAI 兼容地址为 `https://api.ai.lin.mobi/v1`。Anthropic 客户端使用基础地址 `https://api.ai.lin.mobi`，Gemini 原生接口使用 `/v1beta`。各接口保留 Sub2API 原有 API 密钥认证。

面板启用 Cloudflare 的 Always Use HTTPS，HTTP 请求先返回同域名 HTTPS 的 301 跳转；禁止的模型路径到达 HTTPS 后返回 404。API 域名由 Nginx 将允许的 HTTP 路径以 308 跳转到同域名 HTTPS，禁止路径直接返回 404。其他子域名在 HTTP 和 HTTPS 下均关闭连接。浏览器或客户端可能显示“空响应”或“连接关闭”，这是预期行为。

## Cloudflare CDN

仅 `ai.lin.mobi` 的 A 记录启用代理（橙云）。`api.ai.lin.mobi` 有独立 A 记录，设为仅 DNS（灰云）；`*.ai.lin.mobi` 同样仅 DNS。三条记录均指向 AWS `18.175.224.165`，公开配置记录在 `cloudflare.json`。API 请求直接到达 AWS，不经过 Cloudflare 的浏览器挑战、WAF 或代理读取超时；仍须通过 Sub2API 密钥和配额检查。

Cloudflare SSL/TLS 使用完整（严格）模式，关闭自动加密模式，并保留 Always Use HTTPS。免费 Universal SSL 不覆盖 `api.ai.lin.mobi` 这样的多层子域名，因此 API 暂不启用代理；其 HTTPS 使用源站现有通配符证书。[Cloudflare 官方证书覆盖说明](https://developers.cloudflare.com/ssl/edge-certificates/universal-ssl/limitations/)

启用缓存规则 `Sub2API panel dynamic bypass`，表达式为：

```text
(http.host eq "ai.lin.mobi" and not starts_with(http.request.uri.path, "/assets/"))
```

操作为绕过缓存。只有 `/assets/` 中的版本化静态资源按源站缓存头参与 CDN 缓存；面板 HTML、登录和后台接口使用 `no-store`。不启用 Cache Everything，也不修改现有 WAF 或机器人保护。缓存规则和 SSL 设置通过 Cloudflare 控制台配置，修改 Git 文件不会自动同步云端设置。

Nginx 只信任 Cloudflare 官方代理网段的 `CF-Connecting-IP`，保留真实客户端 IP，用于应用的登录和限流。`cloudflare-realip.conf` 的网段来自官方 `https://api.cloudflare.com/client/v4/ips`；Cloudflare 网段变更时应更新此文件并平滑加载 Nginx。

## 实例结构与文件

公网请求到达 AWS 上的 Nginx，再通过现有 WireGuard 隧道代理到虚拟机 `10.77.77.2:8080`。配置中的域名、内网地址和证书路径属于此实例；用于其他实例前请调整。

- `nginx.conf`：AWS 实际启用的完整站点配置，安装位置 `/etc/nginx/sites-available/sub2api-public`。
- `cloudflare-realip.conf`：可信 Cloudflare 代理网段，安装位置 `/etc/nginx/snippets/sub2api-cloudflare-realip.conf`。
- `cloudflare.json`：本实例 DNS、SSL 和缓存规则的公开配置记录，不包含凭据。
- `renew-nginx.sh`：证书更新后检查并平滑加载 Nginx，安装位置 `/etc/letsencrypt/renewal-hooks/deploy/sub2api-nginx-reload`。
- `verify-domains.py`：不读取密钥、不调用模型的公网路径检查，需在 AWS 上使用 Python 3 运行；报告保存到 `/var/lib/sub2api-proxy/domain-separation-verification.json`。

Nginx 对部分根路径兼容接口转为已有 `/v1` 路径，避免嵌入式前端对这些路径返回面板 HTML。后台 `/api/v1` 与模型网关 `/v1` 是不同接口，保留后台接口是面板正常运行的必要条件。

## 安装和恢复

先确认 WireGuard 通道可访问应用，且证书已签发。备份现有站点文件，然后安装本目录配置并检查语法，通过后再平滑加载。例如，在 AWS 上的仓库根目录执行：

```bash
sudo install -d -m 700 /var/backups/sub2api-proxy
backup_path="/var/backups/sub2api-proxy/sub2api-public-$(date -u +%Y%m%dT%H%M%SZ).conf"
sudo install -m 600 /etc/nginx/sites-available/sub2api-public "$backup_path"
sudo install -d -m 755 /etc/nginx/snippets
sudo install -m 644 deploy/linmobi/cloudflare-realip.conf /etc/nginx/snippets/sub2api-cloudflare-realip.conf
sudo install -m 644 deploy/linmobi/nginx.conf /etc/nginx/sites-available/sub2api-public
sudo ln -sfn /etc/nginx/sites-available/sub2api-public /etc/nginx/sites-enabled/sub2api-public
sudo nginx -t
# 只有 nginx -t 成功后才执行 reload；失败时先从 backup_path 恢复。
sudo systemctl reload nginx
sudo install -d -m 700 /var/lib/sub2api-proxy
sudo python3 deploy/linmobi/verify-domains.py --panel-cdn
```

原实例首次隔离前的配置备份位于 `/var/backups/sub2api-proxy/20261004T093004Z-domain-separation/sub2api-public-before.conf`。恢复时将该文件复制回站点文件，语法检查成功后再平滑加载；恢复会重新允许全部子域名访问同一个站点。

开启 CDN 前的隔离配置备份位于 `/var/backups/sub2api-proxy/20261004T095309Z-cloudflare/sub2api-public-before.conf`，DNS 备份为 `/var/backups/sub2api-proxy/20261004T095509Z-cloudflare-dns.json`。仅回退 CDN 时保留域名隔离，将面板 DNS 改为仅 DNS，再恢复此 Nginx 备份；源站检查命令省略 `--panel-cdn`。

Sub2API 面板系统设置应保持：

```text
frontend_url = https://ai.lin.mobi
api_base_url = https://api.ai.lin.mobi
```

这些是数据库中的站点设置，更新 Git 文件不会自动更新数据库。现有用户、密钥、额度和账号继续由同一个 Sub2API 实例管理。

## SSL 与自动续期

证书包含 `ai.lin.mobi` 和 `*.ai.lin.mobi`，Nginx 使用 `/etc/letsencrypt/live/ai.lin.mobi/` 下的证书文件。通配符通过 Certbot 的 Cloudflare DNS 验证续期，不依赖 HTTP 验证路径，所以关闭其他子域名不会影响续期。

本实例使用 `certbot` 和 `python3-certbot-dns-cloudflare`。Cloudflare Token 仅保存在服务器 `/etc/letsencrypt/cloudflare/lin-mobi.ini`，目录权限 700、文件权限 600，由 root 持有。Token、证书私钥、SSH 私钥、管理员密码和备份文件均不得提交到 Git。

安装续期重载脚本后保持 `certbot.timer` 启用；新增实例需要先签发证书，再验证续期：

```bash
sudo install -m 755 deploy/linmobi/renew-nginx.sh /etc/letsencrypt/renewal-hooks/deploy/sub2api-nginx-reload
sudo systemctl enable --now certbot.timer
sudo certbot renew --dry-run --cert-name ai.lin.mobi --run-deploy-hooks
```

2026-10-04 验证结果：78 项路径检查通过；公网面板登录和后台设置读取通过；使用现有密钥在 API 域名读取 Anthropic、OpenAI 模型目录成功，同一密钥在面板域名调用模型目录返回 404；其他子域名返回零内容。SSL 续期演练在域名隔离前通过，隔离后确认 DNS 验证配置、续期重载脚本和定时器保持正常。未为这些检查发起收费模型生成请求。

同日开启面板 CDN 后，重新通过 78 项路径检查；面板登录和当前用户接口返回 200 且不缓存；版本化 JS 资源出现 `MISS → HIT → HIT`。默认 Python-urllib 客户端携带合法密钥直接读取 Anthropic（13 个）和 OpenAI（10 个）模型成功，响应无 Cloudflare 标识。面板检查客户端设置了明确的 User-Agent；普通脚本访问面板仍可能触发现有 Cloudflare 防护，不影响仅 DNS 的模型 API。

## OpenAI 上游流式连接

2026-10-04，Astra 的两次长请求在约 15–16 分钟后出现 `stream error: ... INTERNAL_ERROR; received from peer`，客户端收到 `Upstream HTTP/2 stream failed`。错误发生在 Sub2API 读取 OpenAI 上游响应时。API 域名为仅 DNS，客户端到 Sub2API 的入站协议不控制这条上游连接。

本实例已采用 `openai-upstream.env.example` 中的配置，让 OpenAI 上游使用 HTTP/1.1，继续向客户端发送流式响应。将该变量合并到应用 `env_file` 指定的 `/opt/sub2api/.env`，不要用示例文件覆盖整个生产环境文件。确认没有活跃请求并备份环境文件后，重新创建仅应用服务以加载新变量：

```bash
cd /opt/sub2api
docker compose -p sub2api-production up -d --no-deps --no-build --pull never sub2api
```

重建应用会短暂中断访问，因此应避开活跃生成请求。仅执行 `restart` 不会更新容器环境变量。回退时恢复原环境文件，或设 `GATEWAY_OPENAI_HTTP2_ENABLED=true`，再重新创建应用服务。

应用加载新配置后健康检查返回 200，一次低推理强度的 Astra 短请求约 4.73 秒完成，收到 `response.completed`。此验证确认流式调用可以完整结束，尚未验证与原请求相同的长任务。配置备份和验证记录保存在虚拟机 `/opt/sub2api/public-deployment/`，不提交生产凭据或真实聊天内容。

### 长请求复查

同日后续复查发现，启用 HTTP/1.1 后仍有五条 Astra 长请求约在 901.2 秒中断，读取错误变为 `unexpected EOF`，客户端显示 `Upstream response stream was interrupted`。因此上述协议调整尚未解决长请求中断，不能仅凭短测试认定问题已经修复。

已核对本实例没有配置普通 OpenAI HTTP 请求的 900 秒总超时，账户没有出站代理，容器没有代理环境变量；公网 Nginx 的读取和发送超时均为 3600 秒，API 域名仍仅 DNS。901 秒的重复时长提示某层存在稳定的连接生命周期，但尚未确认具体原因，不能据此断言 OpenAI 官方设有固定 15 分钟上限。

使用独立的简单算术题、空工具列表及 `reasoning.mode=standard` 对照：`medium` 约 2.54 秒、`max` 约 5.15 秒，均收到 `response.completed`，答案正确。这确认两个档位的短请求可用，不能代表复杂长任务也能完成。现有推理设置和分组限制没有修改。

普通助手可先使用 `medium`、新会话和较小的单次任务缓解，再逐步恢复高推理强度。不要在流已经开始输出后自动重放同一请求，也不要仅调整响应头或空闲连接超时来处理此问题。此 OAuth 路径会移除客户端的 `max_output_tokens`，因此输出 Token 上限不能作为可靠的请求限时办法。诊断记录保存在虚拟机 `/opt/sub2api/public-deployment/openai-long-stream-diagnosis.json`，不提交凭据或真实聊天。

### Nginx、WireGuard 与断开顺序验证

2026-10-04，通过独立测试进程复用现有 WireGuard 链路，按正式 Nginx 的 HTTP/1.1 上游、关闭缓冲及 3600 秒超时配置，分别测试持续心跳和完全静默两条 SSE 流。两条连接均持续 1020 秒（17 分钟），收到完整终止事件；不存在这条转发路径统一在 900 秒断开的现象。测试进程和临时监听端口均已关闭，正式 Nginx、隧道及应用配置未因该测试改变。

同时捕获一次实际 Astra `/v1/responses` 故障的 TCP 元数据：请求持续 901.223 秒，ChatGPT 对端先发送 FIN，虚拟机随后结束连接并记录 `unexpected EOF`。断开前约 8.45 秒仍收到上游数据，因此不符合长时间空闲才触发的超时。上游报文与应用错误使用同一虚拟机时钟；AWS 事件来自另一台主机，跨主机毫秒差只作辅助参考。TCP 元数据不包含请求、凭据或聊天正文。

这些证据将此次故障定位到上游连接结束，不能进一步证明 OpenAI 边缘代理、推理任务或其他上游组件的具体内部原因。重复时长使上游请求生命周期限制成为待验证假设，但没有找到官方公布的 ChatGPT 订阅 HTTP 请求统一 15 分钟限制。[OpenAI 官方仓库的相关 HTTP/SSE 报告](https://github.com/openai/codex/issues/32987)描述的是首事件长期沉默和客户端超时，与本实例的持续数据后对端关闭不同，也没有维护者确认原因或关联修复，不能视为同一故障的证明。

WebSocket 是下一步对照方向。[OpenAI API WebSocket 文档](https://developers.openai.com/api/docs/guides/websocket-mode)描述最长 60 分钟连接；这是 API 文档，不代表 ChatGPT OAuth 通道具有相同保证。原生 WebSocket 的一次独立 Astra 简单算术请求约 3.07 秒完成，只确认基本兼容性。HTTP/SSE 转接的实验设置、上下文限制和验证方法见 [ASTRA_WS_PROTOTYPE.md](ASTRA_WS_PROTOTYPE.md)。尚未通过超过原故障时长的实际长请求前，不应宣称已修复。
