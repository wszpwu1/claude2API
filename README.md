# Claude2api

> 中文文档 | [English README](README_EN.md) | [API 文档](API.md) | [English API](API_EN.md)

`Claude2api` 是一个基于 Go、Gin 和浏览器 TLS 指纹客户端实现的 claude.ai 网页 API 代理服务。它将 claude.ai 网页会话封装为 OpenAI Chat Completions、Anthropic Messages 和 OpenAI Responses 兼容接口，并提供多账号调度、流式响应、持久会话、管理面板、API Key、限流、健康检查、统计监控及 Docker 部署能力。

> [!IMPORTANT]
> 本项目依赖有效的 claude.ai 浏览器会话。请妥善保管 `sessionKey`、完整 Cookie、管理密码和客户端 API Key，不要将任何凭据提交到公开仓库。

## 核心能力

### 多协议 API 兼容

- OpenAI Chat Completions：`POST /v1/chat/completions`
- Anthropic Messages：`POST /v1/messages`
- OpenAI Responses：`POST /v1/responses`
- 模型列表：`GET /v1/models`
- 删除持久会话：`DELETE /v1/conversations/:id`
- 服务健康检查：`GET /health`
- 支持普通 JSON 响应和 SSE 流式响应
- 支持 `conversation_id` 多轮会话连续性
- 提供 OpenAI 风格的统一错误响应

完整字段、事件及响应格式请查看 [API.md](API.md)。

### claude.ai 浏览器请求环境

- 使用独立 `tlsclient` 模块封装 Chrome TLS 指纹、CookieJar 和浏览器基础 Header
- 账号级复用 TLS Client、CookieJar、浏览器身份和组织信息
- 支持 Bearer `sessionKey`、完整浏览器 Cookie 和多账号文件
- Bearer 模式自动生成前端可生成的浏览器环境 Cookie、会话标识和 RUM trace Header
- completion 请求携带 claude.ai Web `tools` 字段
- Referer 根据创建会话和继续对话阶段动态切换
- Datadog/RUM Cookie 与 trace Header 按浏览器 SDK 字段结构生成
- 不伪造 `routingHint`、`cf_clearance`、`__cf_bm`、`_cfuvid` 等服务端签名或 Cloudflare Cookie

### 多账号池与路由

- 支持通过管理面板维护账号
- 支持从 `accounts.txt` 加载多个 sessionKey 或完整 Cookie
- 支持批量导入、启用、停用和删除账号
- 支持账号健康检查、冷却恢复和会话用量限制
- 支持 `least-loaded` 最小负载路由
- 支持 `round-robin` 轮询路由
- 账号与路由配置更新后无需重启服务
- 显式提供 `Authorization` 或 `X-Claude-Cookie` 时固定使用请求指定账号
- 持久会话按账号隔离
- 同一个 `conversation_id` 的并发轮次会串行执行，避免会话串扰

### 管理与可观测性

- 内置 `/admin` Web 管理面板
- 支持中文和英文界面
- 支持浅色、深色及跟随系统主题
- 支持实时请求数、成功数、失败数、活跃请求、平均延迟和成功率指标
- 支持最近请求记录
- 支持最近 7 天按账号聚合的使用统计
- 支持进程级令牌桶限流，可动态设置每分钟请求数和突发容量
- 支持定时账号保活，可动态设置检查间隔和超时时间
- 支持可选上游代理和带 `{sid}` 的账号级代理 URL 模板
- 管理配置、账号和统计信息原子持久化到 JSON 文件
- 管理 API 不会明文返回账号密钥

### 客户端访问控制

- 支持创建多个独立的 `c2a_` API Key
- 支持启用、停用和删除 API Key
- 完整密钥仅在创建时显示一次
- 公共接口支持 `X-API-Key: c2a_...`
- 也支持 `Authorization: Bearer c2a_...`
- 配置 API Key 后，所有 `/v1/*` 请求必须先通过客户端密钥校验

## 支持的模型

`GET /v1/models` 返回以下模型，请求时也只能使用这些模型：

- `claude-fable-5`
- `claude-opus-4-8`
- `claude-haiku-4-5`
- `claude-opus-4-7`
- `claude-opus-4-6`
- `claude-opus-3`
- `claude-sonnet-4-6`
- `claude-sonnet-5`
- `claude-opus-5`

使用其他模型会返回 `invalid_request_error`。

## API 路由

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| `GET` | `/health` | 服务健康检查 |
| `GET` | `/v1/models` | 获取支持的模型列表 |
| `POST` | `/v1/chat/completions` | OpenAI Chat Completions 兼容接口 |
| `POST` | `/v1/messages` | Anthropic Messages 兼容接口 |
| `POST` | `/v1/responses` | OpenAI Responses 兼容接口 |
| `DELETE` | `/v1/conversations/:id` | 删除指定持久会话 |
| `GET` | `/admin` | 管理面板 |

基础 API 地址：

```text
http://127.0.0.1:8080/v1
```

## 环境要求

### 本地构建

- Go `1.26.4` 或更高版本
- 一个有效的 claude.ai 浏览器会话

### 容器部署

- Docker
- 可选 Docker Compose

## 快速开始

### 1. 获取项目并安装依赖

```bash
git clone https://github.com/aurora-develop/claude2api.git
cd claude2api
go mod download
```

### 2. 构建

Windows：

```powershell
go build -o claude2api.exe .
```

Linux 或 macOS：

```bash
go build -o claude2api .
```

### 3. 启动服务

#### Linux 或 macOS

使用 sessionKey：

```bash
CLAUDE_SESSION_KEY='你的-sessionKey' PORT=8080 ./claude2api
```

使用完整浏览器 Cookie：

```bash
CLAUDE_COOKIE='sessionKey=...; sessionKeyLC=...; anthropic-device-id=...; ...' PORT=8080 ./claude2api
```

#### Windows PowerShell

```powershell
$env:CLAUDE_SESSION_KEY = '你的-sessionKey'
$env:PORT = '8080'
.\claude2api.exe
```

启动后可访问：

- API：`http://127.0.0.1:8080/v1`
- 管理面板：`http://127.0.0.1:8080/admin`
- 健康检查：`http://127.0.0.1:8080/health`

### 4. 验证服务

```bash
curl http://127.0.0.1:8080/health
```

预期响应：

```json
{"status":"ok"}
```

发送非流式对话请求：

```bash
curl http://127.0.0.1:8080/v1/chat/completions \
  -H 'Authorization: Bearer 你的-sessionKey' \
  -H 'Content-Type: application/json' \
  -d '{
    "model": "claude-sonnet-5",
    "messages": [{"role": "user", "content": "Reply with exactly: pong"}],
    "stream": false
  }'
```

发送流式请求：

```bash
curl -N http://127.0.0.1:8080/v1/chat/completions \
  -H 'Authorization: Bearer 你的-sessionKey' \
  -H 'Content-Type: application/json' \
  -d '{
    "model": "claude-sonnet-5",
    "messages": [{"role": "user", "content": "Reply with exactly: pong"}],
    "stream": true
  }'
```

## 凭据与认证

所有 `/v1/*` 接口都需要可用的 claude.ai 账号凭据。可通过服务端预配置账号，也可由请求显式携带凭据。

### Bearer sessionKey

```http
Authorization: Bearer <claude.ai sessionKey>
```

如果服务端已设置 `CLAUDE_SESSION_KEY` 或配置了可用账号池，请求端可以不重复传递 sessionKey。

仅提供 sessionKey 时，服务会自动生成或维护以下浏览器环境值：

- `sessionKeyLC`
- `anthropic-device-id`
- `activitySessionId`
- `ajs_anonymous_id`
- `__ssid`
- `_dd_s`
- `traceparent` 和 Datadog RUM 相关 Header
- 部分 UI 与 analytics Cookie

以下服务端签名或 Cloudflare Cookie 不会被伪造：

- `routingHint`
- `cf_clearance`
- `__cf_bm`
- `_cfuvid`

### 完整浏览器 Cookie

```http
X-Claude-Cookie: <从 claude.ai 浏览器请求中复制的完整 Cookie>
```

这种方式最接近真实浏览器行为，可复用 Cookie 中的：

- `sessionKey`
- `sessionKeyLC`
- `anthropic-device-id`
- `lastActiveOrg`
- `routingHint`
- Cloudflare 相关 Cookie

如果服务端已配置 `CLAUDE_COOKIE`，请求端可以省略 `X-Claude-Cookie`。

### 客户端 API Key

在管理面板创建 API Key 后，请求还必须携带：

```http
X-API-Key: c2a_...
```

也可以使用：

```http
Authorization: Bearer c2a_...
```

当 claude.ai 凭据需要由请求显式提供时，建议使用 `X-API-Key` 传递客户端密钥，将 `Authorization` 留给 claude.ai sessionKey：

```bash
curl http://127.0.0.1:8080/v1/models \
  -H 'X-API-Key: c2a_...' \
  -H 'Authorization: Bearer 你的-sessionKey'
```

## 多账号部署

### accounts.txt

在工作目录创建 `accounts.txt`，每行填写一个 sessionKey 或一条完整 Cookie。空行及以 `#` 开头的注释会被忽略。

```text
# 账号一：sessionKey
sk-ant-sid01-...

# 账号二：完整 Cookie
sessionKey=sk-ant-sid02-...; sessionKeyLC=...; anthropic-device-id=...; ...
```

默认读取当前工作目录的 `accounts.txt`，也可以通过 `CLAUDE_ACCOUNTS_FILE` 指定其他位置：

```bash
CLAUDE_ACCOUNTS_FILE='/path/to/accounts.txt' ./claude2api
```

未显式携带 claude.ai 凭据的请求会从账号池中选择账号。默认支持最小负载路由，也可在管理面板切换为轮询路由。

### 账号来源合并

服务启动时会合并以下账号来源：

1. `CLAUDE_ACCOUNTS_FILE` 指定的账号文件
2. `CLAUDE_SESSION_KEY` 或 `CLAUDE_COOKIE`
3. 管理数据文件中已启用的账号

重复账号会在传统账号文件加载阶段去重。管理面板中的账号变更会动态同步到运行时账号池。

## 管理面板

启动服务后访问：

```text
http://127.0.0.1:8080/admin
```

### 首次登录

首次创建管理数据文件时：

- 优先使用 `ADMIN_INITIAL_PASSWORD` 作为管理员初始密码
- 未设置时初始密码为 `admin`
- 请在首次登录后立即修改密码
- 修改后的管理员密码至少需要 10 个字符
- 默认管理数据文件为 `data/admin.json`

### 管理功能

- 添加、编辑、启用、停用和删除账号
- 批量导入 sessionKey 或完整 Cookie
- 单账号或批量健康检查
- 恢复冷却账号
- 删除阻断账号
- 设置账号会话用量上限
- 切换最小负载或轮询路由
- 配置上游代理
- 动态配置全局限流
- 动态配置账号定时保活
- 查看实时指标、最近请求和 7 天使用统计
- 创建、启停和删除客户端 API Key
- 修改语言、主题和路由偏好

> [!WARNING]
> 管理数据文件包含加密哈希之外的账号会话凭据。请限制文件权限、定期备份，并避免将其提交到版本控制系统。

## 持久会话

在 Chat Completions、Anthropic Messages 或 Responses 请求中传入 `conversation_id`，服务会在内存中保存该 ID 与 claude.ai conversation 的映射。

```json
{
  "conversation_id": "my-chat-001",
  "model": "claude-sonnet-5",
  "messages": [
    {"role": "user", "content": "继续刚才的话题"}
  ]
}
```

删除会话：

```bash
curl -X DELETE http://127.0.0.1:8080/v1/conversations/my-chat-001 \
  -H 'Authorization: Bearer 你的-sessionKey'
```

注意事项：

- 未传入 `conversation_id` 时，每次请求创建临时 claude.ai 会话
- 临时会话会在响应完成后异步尝试删除
- 同一个 `conversation_id` 的并发轮次会串行处理
- 会话按账号隔离
- 会话映射保存在内存中，服务重启后会丢失

## Docker 部署

### 使用 GHCR 镜像

项目镜像地址：

```text
ghcr.io/aurora-develop/claude2api
```

拉取镜像：

```bash
docker pull ghcr.io/aurora-develop/claude2api:latest
```

使用 sessionKey 启动：

```bash
docker run -d \
  --name claude2api \
  --restart unless-stopped \
  -p 8080:8080 \
  -e CLAUDE_SESSION_KEY='你的-sessionKey' \
  -e ADMIN_INITIAL_PASSWORD='请设置一个强密码' \
  -v claude2api-data:/app/data \
  ghcr.io/aurora-develop/claude2api:latest
```

使用完整 Cookie 启动：

```bash
docker run -d \
  --name claude2api \
  --restart unless-stopped \
  -p 8080:8080 \
  -e CLAUDE_COOKIE='sessionKey=...; sessionKeyLC=...; anthropic-device-id=...; ...' \
  -e ADMIN_INITIAL_PASSWORD='请设置一个强密码' \
  -v claude2api-data:/app/data \
  ghcr.io/aurora-develop/claude2api:latest
```

使用账号文件：

```bash
docker run -d \
  --name claude2api \
  --restart unless-stopped \
  -p 8080:8080 \
  -e CLAUDE_ACCOUNTS_FILE='/app/accounts.txt' \
  -e ADMIN_INITIAL_PASSWORD='请设置一个强密码' \
  -v "$PWD/accounts.txt:/app/accounts.txt:ro" \
  -v claude2api-data:/app/data \
  ghcr.io/aurora-develop/claude2api:latest
```

### 本地构建镜像

```bash
docker build -t claude2api:latest .
```

```bash
docker run -d \
  --name claude2api \
  --restart unless-stopped \
  -p 8080:8080 \
  -e CLAUDE_SESSION_KEY='你的-sessionKey' \
  -v claude2api-data:/app/data \
  claude2api:latest
```

### Docker Compose

项目已提供 `docker-compose.yml`。由于 Compose 会挂载账号文件，请先创建该文件：

```bash
touch accounts.txt
```

使用 sessionKey：

```bash
CLAUDE_SESSION_KEY='你的-sessionKey' docker compose up -d --build
```

使用完整 Cookie：

```bash
CLAUDE_COOKIE='sessionKey=...; sessionKeyLC=...; anthropic-device-id=...; ...' docker compose up -d --build
```

使用账号池时，将凭据写入 `accounts.txt` 后运行：

```bash
docker compose up -d --build
```

查看日志：

```bash
docker compose logs -f claude2api
```

停止服务：

```bash
docker compose down
```

> [!NOTE]
> 当前仓库中的 Compose 配置挂载了 `accounts.txt`，但未挂载 `/app/data`。如果需要在容器重建后保留管理面板配置、账号和统计数据，请在 `volumes` 中额外挂载宿主机目录或命名卷到 `/app/data`。

## 生产部署建议

1. 通过 Nginx、Caddy 或其他反向代理提供 HTTPS。
2. 不要直接将管理面板暴露在不受信任的公网环境。
3. 首次部署时设置高强度 `ADMIN_INITIAL_PASSWORD`。
4. 为公共 `/v1/*` 接口创建 `c2a_` API Key。
5. 挂载并备份 `/app/data`，确保管理配置持久化。
6. 对 `accounts.txt`、管理数据文件和环境变量设置严格权限。
7. 根据上游账号容量启用全局限流和账号会话限额。
8. 启用定时保活，并通过管理面板观察账号健康状态。
9. 为 SSE 流式接口关闭反向代理缓冲并配置足够的读取超时。
10. 定期轮换管理员密码、客户端 API Key 和失效账号凭据。

Nginx 反向代理需要注意 SSE：

```nginx
location / {
    proxy_pass http://127.0.0.1:8080;
    proxy_http_version 1.1;
    proxy_set_header Host $host;
    proxy_set_header X-Real-IP $remote_addr;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_buffering off;
    proxy_cache off;
    proxy_read_timeout 3600s;
}
```

## 配置项

| 环境变量 | 默认值 | 说明 |
| --- | --- | --- |
| `PORT` | `8080` | HTTP 服务监听端口 |
| `GIN_MODE` | `release` | Gin 运行模式；未设置时程序自动切换为 release |
| `CLAUDE_BASE_URL` | `https://claude.ai` | claude.ai 上游地址 |
| `CLAUDE_SESSION_KEY` | 空 | 默认 claude.ai sessionKey |
| `CLAUDE_COOKIE` | 空 | 默认完整 claude.ai 浏览器 Cookie |
| `CLAUDE_ACCOUNTS_FILE` | `accounts.txt` | 多账号文件路径 |
| `CLAUDE_PROXY_URL` | 空 | 传统账号池使用的代理 URL，支持 `{sid}` 稳定账号标识 |
| `CLAUDE_TIMEZONE` | `Asia/Singapore` | 发送给上游的时区 |
| `CLAUDE_LOCALE` | `en-US` | 发送给上游的语言区域 |
| `DEFAULT_MODEL` | `claude-sonnet-5` | 请求未指定模型时使用的模型 |
| `CLAUDE_EFFORT` | `medium` | 默认推理力度 |
| `CLAUDE_CODE_EFFORT_LEVEL` | 空 | `CLAUDE_EFFORT` 未设置时使用的兼容配置 |
| `CLAUDE_THINKING` | `auto` | 默认 thinking 配置，可使用 `auto`、`none`、`enabled` 或 JSON 对象 |
| `ADMIN_DATA_FILE` | `data/admin.json` | 管理配置、账号和统计数据的持久化文件 |
| `ADMIN_INITIAL_PASSWORD` | `admin` | 仅首次创建管理数据文件时使用的初始密码 |

`CLAUDE_THINKING` JSON 示例：

```bash
export CLAUDE_THINKING='{"type":"enabled","budget_tokens":10000}'
```

## 上游代理

可通过 `CLAUDE_PROXY_URL` 为启动时加载的传统账号池设置代理，也可以在管理面板中动态配置代理。

代理 URL 模板支持 `{sid}`，服务会将其替换为稳定的账号标识：

```text
http://username-{sid}:password@proxy.example.com:8080
```

这适用于需要按账号维持独立代理会话的服务。修改管理面板中的代理配置后，运行时客户端会按新配置更新。

## 项目结构

```text
.
├── admin/                 # 管理 API、认证、持久化、指标、保活与 Web 面板
├── claude/                # claude.ai Web 客户端及工具定义
├── config/                # 环境变量、账号文件和模型配置
├── handlers/              # OpenAI、Anthropic、Responses 及会话处理器
├── middleware/            # 公共接口认证中间件
├── models/                # 请求与响应数据模型
├── tlsclient/             # TLS 指纹、CookieJar 和浏览器 Header 封装
├── utils/                 # SSE、UUID 等通用工具
├── API.md                 # 中文 API 文档
├── API_EN.md              # 英文 API 文档
├── Dockerfile             # 多阶段容器构建
├── docker-compose.yml     # Docker Compose 配置
├── main.go                # 服务入口与路由注册
└── README.md              # 中文项目说明
```

## 常见问题

### 请求返回 401

检查以下项目：

- 请求是否携带有效的 `Authorization: Bearer <sessionKey>`
- 请求是否携带有效的 `X-Claude-Cookie`
- 服务端是否配置了 `CLAUDE_SESSION_KEY`、`CLAUDE_COOKIE` 或可用账号池
- 启用客户端 API Key 后是否同时携带有效的 `c2a_` 密钥

### 请求返回 429

这通常表示以下情况之一：

- claude.ai 账号或会话触发上游速率限制
- 本服务启用了全局令牌桶限流
- 账号达到管理面板设置的会话用量限制

可在管理面板查看账号状态、最近请求及实时指标。

### 浏览器可以访问，但代理请求失败

优先使用完整浏览器 Cookie 模式。仅使用 sessionKey 时，本服务不会伪造 `routingHint` 或 Cloudflare 签名 Cookie，因此某些会话环境可能存在差异。

### 重启后 conversation_id 失效

持久会话映射目前只保存在内存中，服务重启后会丢失。客户端应在重启后创建新的 `conversation_id`。

### Docker 重建后管理配置丢失

请将宿主机目录或 Docker 命名卷挂载到 `/app/data`。默认管理文件位于 `/app/data/admin.json`。

### usage 中的 token 数不准确

当前 token 使用量为近似值，主要根据文本长度估算，不应作为精确计费依据。

## 限制说明

- 本项目只提供列出的文本对话接口
- 不支持 `/v1/images/generations`
- 不支持 `/v1/images/edits`
- 不支持 `/v1/images/variations`
- 部分 OpenAI 或 Anthropic 请求字段仅用于协议兼容，可能不会完整映射到 claude.ai 网页能力
- 附件字段会保留在请求模型中，但文件上游行为有限
- token 统计为近似值
- 持久会话映射不会跨进程或跨重启持久化

## 安全注意事项

- 不要提交 `sessionKey`、完整 Cookie、`accounts.txt`、管理数据文件或抓包文件
- 不要在日志、截图和问题反馈中公开凭据
- 不要使用默认管理员密码运行公网服务
- 使用独立 API Key 控制客户端访问
- 为管理数据目录设置最小必要权限
- 在公开部署前配置 HTTPS、访问控制和请求限流
- 账号凭据失效或泄露时应立即在 claude.ai 侧撤销会话

## API 文档

更完整的请求字段、响应格式和 SSE 事件说明见：

- [中文 API 文档](API.md)
- [English API Documentation](API_EN.md)

## 致谢

感谢 [LINUX DO 社区](https://linux.do)。本项目在此发布，并持续受益于社区用户的反馈与帮助。

## 相关文档

- [English README](README_EN.md)
- [中文 API 文档](API.md)
- [English API](API_EN.md)
