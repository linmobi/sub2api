# lin.mobi 实例公网部署配置

本目录记录已部署实例的公网代理配置。它不修改 Sub2API 业务源码。

## 域名与路径

| 域名 | 允许访问 | 被阻止的内容 |
| --- | --- | --- |
| `ai.lin.mobi` | 面板、静态资源、登录和 `/api/v1` 后台接口 | 模型调用接口返回 JSON 404 |
| `api.ai.lin.mobi` | OpenAI、Anthropic、Gemini、Antigravity 等模型网关接口及已有兼容路径 | 面板、静态资源、登录和 `/api/v1` 后台接口返回 JSON 404 |
| 其他 `*.ai.lin.mobi` | 无 | Nginx 直接关闭连接，不返回内容或跳转 |

客户端 OpenAI 兼容地址为 `https://api.ai.lin.mobi/v1`。Anthropic 客户端使用基础地址 `https://api.ai.lin.mobi`，Gemini 原生接口使用 `/v1beta`。各接口保留 Sub2API 原有 API 密钥认证。

两个正式域名各自将允许访问的 HTTP 路径跳转到同域名 HTTPS；被禁止的路径不会通过跳转绕过限制。其他子域名在 HTTP 和 HTTPS 下均关闭连接。浏览器或客户端可能显示“空响应”或“连接关闭”，这是预期行为。

## 实例结构与文件

公网请求到达 AWS 上的 Nginx，再通过现有 WireGuard 隧道代理到虚拟机 `10.77.77.2:8080`。配置中的域名、内网地址和证书路径属于此实例；用于其他实例前请调整。

- `nginx.conf`：AWS 实际启用的完整站点配置，安装位置 `/etc/nginx/sites-available/sub2api-public`。
- `renew-nginx.sh`：证书更新后检查并平滑加载 Nginx，安装位置 `/etc/letsencrypt/renewal-hooks/deploy/sub2api-nginx-reload`。
- `verify-domains.py`：不读取密钥、不调用模型的公网路径检查，需在 AWS 上使用 Python 3 运行；报告保存到 `/var/lib/sub2api-proxy/domain-separation-verification.json`。

Nginx 对部分根路径兼容接口转为已有 `/v1` 路径，避免嵌入式前端对这些路径返回面板 HTML。后台 `/api/v1` 与模型网关 `/v1` 是不同接口，保留后台接口是面板正常运行的必要条件。

## 安装和恢复

先确认 WireGuard 通道可访问应用，且证书已签发。备份现有站点文件，然后安装本目录配置并检查语法，通过后再平滑加载。例如，在 AWS 上的仓库根目录执行：

```bash
sudo install -d -m 700 /var/backups/sub2api-proxy
backup_path="/var/backups/sub2api-proxy/sub2api-public-$(date -u +%Y%m%dT%H%M%SZ).conf"
sudo install -m 600 /etc/nginx/sites-available/sub2api-public "$backup_path"
sudo install -m 644 deploy/linmobi/nginx.conf /etc/nginx/sites-available/sub2api-public
sudo ln -sfn /etc/nginx/sites-available/sub2api-public /etc/nginx/sites-enabled/sub2api-public
sudo nginx -t
# 只有 nginx -t 成功后才执行 reload；失败时先从 backup_path 恢复。
sudo systemctl reload nginx
sudo install -d -m 700 /var/lib/sub2api-proxy
sudo python3 deploy/linmobi/verify-domains.py
```

原实例首次隔离前的配置备份位于 `/var/backups/sub2api-proxy/20261004T093004Z-domain-separation/sub2api-public-before.conf`。恢复时将该文件复制回站点文件，语法检查成功后再平滑加载；恢复会重新允许全部子域名访问同一个站点。

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
