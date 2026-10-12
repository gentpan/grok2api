# grok2api

把 Grok (x.ai) 变成 OpenAI / Anthropic 兼容的 API。纯 Go，单个二进制，开箱即用。

- OpenAI 兼容：`/v1/chat/completions`、`/v1/responses`、`/v1/images/generations`、`/v1/videos`
- Anthropic 兼容：`/v1/messages`
- 多账号池，按剩余配额选号，失败自动换号重试
- 管理后台：账号增删、配置修改、调用统计

## 快速开始

```bash
go build -o grok2api .
./grok2api            # 监听 0.0.0.0:8000
```

或者用 Docker（支持 amd64 / arm64 / armv7）：

```bash
docker pull ghcr.io/gentpan/grok2api:latest
docker run -d --name grok2api -p 8000:8000 -v $(pwd)/data:/app/data ghcr.io/gentpan/grok2api:latest
```

也可以自己构建：`docker build -t grok2api .`

### 1. 添加账号

先拿到你的 Grok `sso` Cookie（获取方法见 [CONFIG.md](CONFIG.md#获取-sso-token)），然后：

```bash
curl -X POST http://localhost:8000/admin/api/tokens/add \
  -H "Authorization: Bearer grok2api" \
  -H "Content-Type: application/json" \
  -d '{"tokens": ["你的sso-token"], "pool": "basic"}'
```

`pool` 为 `basic`（免费号）、`super` 或 `heavy`（付费号）。

> 管理密码默认是 `grok2api`（配置项 `app.app_key`）。没改过默认值时，管理接口只接受本机访问；对外开放前请先修改。

### 2. 调用

```bash
curl http://localhost:8000/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model": "grok-4.20-0309-non-reasoning", "messages": [{"role": "user", "content": "你好"}], "stream": true}'
```

也可以不入池，直接把 SSO Token 当 Bearer 使用：`-H "Authorization: Bearer 你的sso-token"`。

OpenAI SDK：

```python
from openai import OpenAI

client = OpenAI(base_url="http://localhost:8000/v1", api_key="any")
resp = client.chat.completions.create(
    model="grok-4.20-0309-non-reasoning",
    messages=[{"role": "user", "content": "你好"}],
)
print(resp.choices[0].message.content)
```

Anthropic SDK：

```python
import anthropic

client = anthropic.Anthropic(base_url="http://localhost:8000", api_key="any")
msg = client.messages.create(
    model="grok-4.20-0309-non-reasoning",
    max_tokens=1024,
    messages=[{"role": "user", "content": "你好"}],
)
print(msg.content[0].text)
```

## 常用配置

配置文件是 `data/config.toml`，只需写你想改的项；默认值见 [config.defaults.toml](config.defaults.toml)。

| 配置项 | 说明 | 默认 |
|---|---|---|
| `app.app_key` | 管理后台密码 | `grok2api` |
| `app.api_key` | API 密钥，留空则不鉴权，多个用逗号分隔 | 空 |
| `proxy.egress.proxy_url` | 出站代理（HTTP / SOCKS4 / SOCKS5） | 空 |
| `retry.max_retries` | 换号重试次数 | `1` |
| `account.refresh.enabled` | 配额模式（`false` 为随机选号） | `true` |

常用环境变量：`SERVER_HOST`、`SERVER_PORT`、`DATA_DIR`、`PROXY_HTTP`。

Grok 返回 403 / anti-bot 拒绝时，需要配置浏览器 Cookie，步骤见 [CONFIG.md](CONFIG.md#配置反爬绕过重要)。

## 文档

- [CONFIG.md](CONFIG.md)：获取 Token、反爬配置、完整配置项与环境变量、账号池与模型、Docker、常见问题
- [API.md](API.md)：全部接口与管理 API

## 开发

```bash
go vet ./...
go test -race ./...
```

## 许可

[MIT](LICENSE)
