# 配置与部署参考

> 快速开始见 [README](README.md)，接口说明见 [API.md](API.md)。

## 获取 SSO Token

SSO Token 是你的 Grok 账号凭证，用于调用上游 Grok API。

**方法一：浏览器 DevTools（推荐）**

1. 打开 [grok.com](https://grok.com) 并登录你的账号
2. 按 `F12` 打开浏览器开发者工具
3. 切换到 **Application**（应用程序）标签页
4. 左侧找到 **Cookies** → `https://grok.com`
5. 找到名为 `sso` 的 Cookie，复制它的 **Value** 值
6. 这个值就是你的 SSO Token（通常是一串很长的字符）

**方法二：Network 面板抓包**

1. 打开 [grok.com](https://grok.com) 并登录
2. 按 `F12` → **Network**（网络）标签页
3. 在 Grok 页面随便发一条消息
4. 找到 `conversations/new` 请求 → **Headers** → **Cookie**
5. 从 Cookie 字符串中提取 `sso=` 后面的值

> **注意**：每个 SSO Token 对应一个 Grok 账号。Token 过期后需要重新获取。免费账号（basic pool）和付费账号（super/heavy pool）的配额不同。

## 配置反爬绕过（重要）

Grok 有 Cloudflare + 自研反爬机制。要正常调用，需要从浏览器抓取两组凭证：

### 第一步：抓取 Cloudflare 凭证

1. 打开 [grok.com](https://grok.com)（已登录），F12 → **Network**
2. 在 Grok 页面随便发一条消息
3. 找到 `conversations/new` 请求 → **Headers** → **Cookie**
4. 复制 `cf_clearance` 的值

### 第二步：抓取 grok 会话令牌

`x-anonuserid`、`x-challenge`、`x-signature`、`x-userid` 是 grok 服务器通过 `Set-Cookie` 响应头下发的会话令牌，**不是客户端生成的**。它们与 `cf_clearance` 必须来自同一个浏览器会话。

**抓取方法：**

1. 浏览器打开 [grok.com](https://grok.com) 并登录
2. F12 → **Application**（应用程序）标签页
3. 左侧 **Cookies** → `https://grok.com`
4. 依次找到并复制以下 4 个 Cookie 的值：
   - `x-anonuserid` — 匿名用户 UUID（首次访问时服务器自动下发）
   - `x-userid` — 登录用户 UUID
   - `x-challenge` — grok proof-of-work 挑战令牌（较长的 base64 字符串）
   - `x-signature` — 对 challenge 的签名（较短的 base64 字符串）

> **注意**：这 4 个令牌是**服务器绑定的会话凭证**，过期后无法客户端续期，必须重新登录获取。

### 第三步：填写配置

将抓取的值填入 `data/config.toml`：

```toml
[proxy.clearance]
# 整段 Cookie 也可以直接粘贴（程序会自动提取所需部分）
cf_cookies = "cf_clearance=..."
user_agent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/147.0.0.0 Safari/537.36"
# 从浏览器 Cookies 中逐个复制
x_anonuserid = "00000000-0000-0000-0000-000000000000"
x_userid = "00000000-0000-0000-0000-000000000000"
x_challenge = "<从浏览器 Cookie 复制的 x-challenge>"
x_signature = "<从浏览器 Cookie 复制的 x-signature>"
```

> **提示**
> - `cf_clearance` 有效期通常几小时到一天，过期后需重新抓取，否则会返回 403。
> - `x-challenge` / `x-signature` / `x-anonuserid` / `x-userid` 是 grok 内部匿名会话的 anti-bot 令牌，**必须与 `cf_clearance` 来自同一浏览器会话**。如果你更新了其中一个，建议全部重新导出。
> - `x-statsig-id` 反 bot 头由纯 Go 实时自动生成（启动时自动创建随机 seed + 内置 HEX 算法），**无需手动配置**。如果被 grok 拒绝（code:7），可调用 `RotatePair()` 刷新。

### 抓取 statsig 指纹

程序内置纯 Go 自动生成 `x-statsig-id`（启动时创建随机 seed，通过 CSS 动画指纹算法计算 HEX），**大多数场景无需额外配置**。

如果 grok 频繁返回 `code:7`（anti-bot 拒绝），说明当前 seed/HEX 对可能被标记。程序会自动尝试 `RotatePair()` 刷新。如仍失败，可手动抓取浏览器真实值：

**步骤：**

1. 打开 [grok.com](https://grok.com) 并登录
2. 按 `F12` → **Console**（控制台）
3. 粘贴并执行以下脚本：

```javascript
(()=>{const o=crypto.subtle.digest.bind(crypto.subtle);crypto.subtle.digest=function(a,d){const s=new TextDecoder().decode(new Uint8Array(d.buffer||d)),i=s.indexOf('obfiowerehiring');if(i>=0)console.log('SEED=',document.querySelector('meta[name="grok-site―verification"]').content,'\nHEX=',s.slice(i+15));return o(a,d);};})();
```

4. 在 Grok 页面**发送一条消息**
5. 控制台会输出 `SEED=...` 和 `HEX=...`
6. 将 `SEED` 和 `HEX` 填入 `data/config.toml`：

```toml
[proxy.clearance]
statsig_seed = "abc123def456"
statsig_hex  = "0123456789abcdef"
```

## 配置说明

配置文件采用 TOML 格式，加载优先级：

1. `config.defaults.toml`（内置默认值）
2. `data/config.toml`（用户自定义，覆盖默认值）
3. `GROK_*` 环境变量（最高优先级）

### 主要配置项

```toml
[app]
app_key = "grok2api"           # 管理后台密码
api_key = ""                    # API 密钥（留空不鉴权，逗号分隔多个）
public_origin = ""              # 对外 origin（如 https://grok.example.com），留空自动推断

[logging]
file_level = "INFO"             # 文件日志级别
max_files = 7                   # 日志文件最大保留数

[features]
stream = true                   # 默认流式响应
thinking = true                 # 输出思考过程
temporary = true                # 临时对话（不保存历史）
memory = false                  # 会话记忆
auto_chat_mode_fallback = true  # AUTO 模型自动降级到 fast/expert
custom_instruction = ""         # 全局附加指令（系统提示）
enable_nsfw = true              # 图片生成/编辑请求启用 NSFW

[cache.local]
image_max_mb = 0                # 图片缓存上限（MB），0 = 不限制
video_max_mb = 0                # 视频缓存上限（MB），0 = 不限制

[proxy.egress]
proxy_url = ""                  # 出站代理（留空直连），HTTP/HTTPS/SOCKS4/5

[proxy.clearance]
cf_cookies = ""                 # 手动模式：浏览器 Cookie 串（含 cf_clearance）
user_agent = "..."              # 需与抓取 Cookie 时的 UA 一致
statsig_seed = ""               # 可选：手动覆盖自动 statsig 种子
statsig_hex  = ""               # 可选：手动覆盖自动 statsig HEX 指纹
statsig_from_html = false       # 已弃用：程序现已内置纯 Go 自动生成功能

[retry]
max_retries = 1                 # 换账号重试最大次数（0 = 不重试）
on_codes = "429,401,503"        # 触发重试的 HTTP 状态码
reset_session_status_codes = [403]  # 触发重建代理 Session 的状态码

[account.refresh]
enabled = true                  # true=配额模式；false=随机模式
basic_interval_sec = 86400      # basic 池刷新间隔（秒）
super_interval_sec = 7200       # super 池刷新间隔（秒）
heavy_interval_sec = 7200       # heavy 池刷新间隔（秒）

[account.selection]
max_inflight = 8                # 单号并发上限

[asset]
upload_timeout = 60             # 资源上传超时（秒）
list_timeout = 60               # 资源列表超时（秒）
delete_timeout = 60             # 资源删除超时（秒）

[nsfw]
timeout = 60                    # NSFW 设置超时（秒）
```

> `config.defaults.toml` 内置全部默认值，`data/config.toml` 只需覆盖你想修改的项即可。

### 环境变量

| 变量 | 说明 | 默认值 |
|---|---|---|
| `SERVER_HOST` | 监听地址 | `0.0.0.0` |
| `SERVER_PORT` | 监听端口 | `8000` |
| `LOG_LEVEL` | 日志级别 | `INFO` |
| `LOG_FILE_ENABLED` | 启用文件日志 | `true` |
| `DATA_DIR` | 数据目录 | `./data` |
| `ACCOUNT_LOCAL_PATH` | 账号存储路径 | `./data/accounts.jsonl` |
| `PROXY_HTTP` | 代理地址（覆盖配置文件） | _(空)_ |
| `GROK_<配置路径>` | 配置覆盖，路径中的 `.` 换成 `_`，如 `GROK_ACCOUNT_REFRESH_ENABLED` → `account.refresh.enabled` | _(空)_ |

> `GROK_*` 环境变量可用于覆盖任意配置项（包括三级配置）。例如 `GROK_FEATURES_STREAM=false` 等同于 `features.stream = false`，`GROK_PROXY_EGRESS_PROXY_URL=http://127.0.0.1:7890` 等同于 `proxy.egress.proxy_url`。变量名按 `config.defaults.toml` 中已知的配置项做最长匹配，未知项回退为「首个下划线之前为 section」。环境变量优先级最高：被其覆盖的配置项在管理后台修改后会写入 `data/config.toml`，但在移除该环境变量前不会生效。

## 账号池与模型

### 账号池

| 池 | 说明 | 配额周期 |
|---|---|---|
| basic | 免费账号 | 24 小时 |
| super | 付费账号 | 2 小时 |
| heavy | 高级账号 | 2 小时 |

### 可用模型

**grok.com 聊天**：`grok-4.20-0309`、`grok-4.20-0309-reasoning`、`grok-4.20-heavy`、`grok-4.20-multi-agent-0309` 等 16 个模型

**Console**：`grok-4.3-console`、`grok-4.3-high`、`grok-4.20-multi-agent-xhigh`、`grok-4.20-0309-non-reasoning-console`、`grok-build-console` 等对话模型（通过 console.x.ai，自动使用 DPoP）

**媒体**：Web 路径提供 `grok-imagine-image-lite`、`grok-imagine-image`、`grok-imagine-image-pro`（WebSocket 实时生成）、`grok-imagine-image-edit`、`grok-imagine-video`；Console DPoP 路径另提供 `grok-imagine-image-console`、`grok-imagine-image-quality-console`、`grok-imagine-video-console`

完整模型列表见 [API.md](API.md)。

## Docker 部署

```bash
docker pull ghcr.io/gentpan/grok2api:latest
docker run -d --name grok2api -p 8000:8000 -v $(pwd)/data:/app/data ghcr.io/gentpan/grok2api:latest
```

自行构建：`docker build -t grok2api .`

`docker compose` 示例：

```yaml
services:
  grok2api:
    image: ghcr.io/gentpan/grok2api:latest
    container_name: grok2api
    restart: unless-stopped
    ports:
      - "8000:8000"
    volumes:
      - ./data:/app/data
    environment:
      - TZ=Asia/Shanghai
      # - PROXY_HTTP=http://127.0.0.1:7890
```

> 镜像由 GitHub Actions 构建并发布到 `ghcr.io/gentpan/grok2api`（`latest` 与提交短哈希标签），可匿名拉取。
>
> 容器以非 root 用户运行；挂载的 `data`、`logs` 目录会在启动时自动修正属主。

### 多实例

账号池保存在单个 `accounts.jsonl` 文件里，由进程内的目录缓存维护，**多个进程共享同一个数据目录并不安全**（没有跨进程文件锁，彼此看不到对方的内存状态，后写入者会覆盖前者）。需要多实例时，请让每个实例使用各自独立的 `DATA_DIR`（以及各自的账号），在前面用反向代理做负载均衡。

## 常见问题

**Q: 如何获取 SSO Token？**
A: 登录 [grok.com](https://grok.com)，按 F12 打开开发者工具 → Application → Cookies → 找到 `sso` 字段复制其值。详见上方「获取 SSO Token」章节。

**Q: 为什么调用返回 403 "Request rejected by anti-bot rules"？**
A: 这是 Grok 的反爬机制，通常是 `cf_clearance` 缺失或过期。解决方法：

1. 按上方「配置反爬绕过」重新抓取浏览器 Cookie，填入 `proxy.clearance.cf_cookies`
2. 确保 `proxy.clearance.user_agent` 与抓取时浏览器的 UA 完全一致
3. 若使用代理，确认代理出口 IP 与浏览器 IP 一致（cf_clearance 绑定 IP）

> `x-statsig-id` 等反 bot 头由程序自动生成，无需手动处理。如遇频繁拦截，可手动抓取浏览器里的真实 seed / HEX 并填入 `proxy.clearance.statsig_seed` / `statsig_hex`（填写后以配置为准），详见[抓取 statsig 指纹](#抓取-statsig-指纹)。

**Q: 如何获取 cf_clearance？**
A: 登录 grok.com，F12 → Network → 刷新页面 → 找到任意 grok.com 请求 → Request Headers → Cookie → 复制 `cf_clearance=...` 的值。有效期通常几小时到一天。最简单的做法是把整段 Cookie 头都粘进 `cf_cookies`。

**Q: 如何使用代理？**
A: 在 `config.toml` 的 `[proxy.egress]` 中设置 `proxy_url`，兼容 HTTP/HTTPS/SOCKS4/SOCKS5 协议；或用环境变量 `PROXY_HTTP` 覆盖。示例：`proxy_url = "socks5://127.0.0.1:1080"`。

**Q: 支持图片输入吗？**
A: 支持。在 messages 的 content 中使用 `image_url` 类型，支持 URL 和 base64 data URI。

**Q: 多实例怎么部署？**
A: 每个实例使用独立的 `DATA_DIR` 和账号，由反向代理分流。不要让多个进程共享同一个 `accounts.jsonl`，详见上方「多实例」。
