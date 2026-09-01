# claude2API

> 中文 | [English](README_EN.md) | [API 文档](API.md) | [English API](API_EN.md)

将 claude.ai 网页接口转换为标准 API 服务，兼容 OpenAI Chat Completions、Anthropic Messages 和 OpenAI Responses 三种协议格式，同时提供 Web 管理面板。

## 功能特性

- **多协议兼容**
  - `POST /v1/chat/completions` — OpenAI Chat Completions 格式
  - `POST /v1/messages` — Anthropic Messages 格式
  - `POST /v1/responses` — OpenAI Responses 格式
  - `GET /v1/models` — 模型列表
  - `DELETE /v1/conversations/:id` — 删除持久会话
- **多账号管理** — 支持配置多个 claude.ai 账号，自动按负载分发请求
- **Web 管理面板** — 内置账号管理、API Key 管理、请求统计、模型映射、代理配置等
- **流式响应** — 支持 SSE 流式输出
- **持久会话** — 通过 `conversation_id` 复用 claude.ai 对话
- **账号保活** — 定时健康检查，自动标记不可用账号并在冷却后恢复
- **速率限制** — 可配置全局令牌桶限流
- **模型映射** — 将任意模型名映射到受支持的 Claude 模型
- **代理支持** — 支持为账号配置上游 HTTP/HTTPS 代理

### 支持的模型

| 模型 ID | 说明 |
|---|---|
| `claude-sonnet-5` | 默认模型 |
| `claude-opus-5` | |
| `claude-opus-4-8` | |
| `claude-opus-4-7` | |
| `claude-opus-4-6` | |
| `claude-opus-3` | |
| `claude-sonnet-4-6` | |
| `claude-haiku-4-5` | |
| `claude-fable-5` | |

---

## 部署

### 方式一：Docker Compose（推荐）

**1. 克隆项目**

```bash
git clone <repo-url>
cd claude2API
```

**2. 准备账号文件（可选）**

如果需要配置多个账号，创建 `accounts.txt`，每行一个账号：

```text
# sessionKey 模式（推荐）
sk-ant-sid01-xxxxxxxx

# 完整 Cookie 模式（含 sessionKey= 字段时自动识别）
sessionKey=sk-ant-sid01-xxxxxxxx; sessionKeyLC=...; anthropic-device-id=...
```

以 `#` 开头的行会被忽略。

**3. 配置环境变量**

创建 `.env` 文件（或直接在 `docker-compose.yml` 中修改）：

```env
PORT=8080
CLAUDE_SESSION_KEY=sk-ant-sid01-xxxxxxxx
CLAUDE_COOKIE=
CLAUDE_ACCOUNTS_FILE=./accounts.txt
CLAUDE_BASE_URL=https://claude.ai
CLAUDE_TIMEZONE=Asia/Shanghai
CLAUDE_LOCALE=zh-CN
DEFAULT_MODEL=claude-sonnet-5
ADMIN_INITIAL_PASSWORD=yourpassword
```

**4. 启动服务**

```bash
docker compose up -d
```

服务监听 `http://localhost:8080`。

---

### 方式二：直接编译运行

需要 Go 1.21+。

```bash
git clone <repo-url>
cd claude2API
go build -o claude2api .

export CLAUDE_SESSION_KEY=sk-ant-sid01-xxxxxxxx
./claude2api
```

---

## 环境变量说明

| 变量 | 默认值 | 说明 |
|---|---|---|
| `PORT` | `8080` | 监听端口 |
| `CLAUDE_BASE_URL` | `https://claude.ai` | claude.ai 地址 |
| `CLAUDE_SESSION_KEY` | — | 单账号 sessionKey |
| `CLAUDE_COOKIE` | — | 单账号完整浏览器 Cookie |
| `CLAUDE_ACCOUNTS_FILE` | `accounts.txt` | 多账号文件路径 |
| `CLAUDE_PROXY_URL` | — | 全局上游代理地址（支持 `{sid}` 占位符） |
| `CLAUDE_TIMEZONE` | `Asia/Singapore` | 请求时使用的时区 |
| `CLAUDE_LOCALE` | `en-US` | 请求时使用的语言 |
| `DEFAULT_MODEL` | `claude-sonnet-5` | 请求未指定模型时使用的默认模型 |
| `CLAUDE_EFFORT` | `medium` | 思考努力程度（`low` / `medium` / `high`） |
| `CLAUDE_THINKING` | — | 思考模式（`auto` / `none` / `enabled` / JSON 对象） |
| `ADMIN_DATA_FILE` | `data/admin.json` | 管理面板数据持久化文件路径 |
| `ADMIN_INITIAL_PASSWORD` | `admin` | 管理面板初始密码（文件不存在时生效） |

---

## 认证

所有 `/v1` 接口均需要认证，支持以下方式之一：

```http
Authorization: ******
```

或传入完整浏览器 Cookie：

```http
X-Claude-Cookie: sessionKey=...; sessionKeyLC=...; anthropic-device-id=...
```

如果服务端已通过环境变量或账号文件配置了账号，客户端可以传任意非空字符串作为 ******

也可以通过管理面板配置 **Master Key** 或 **API Key**，客户端使用对应 Key 访问账号池。

---

## 管理面板

访问 `http://localhost:8080/admin`，使用初始密码（默认 `admin`）登录。

面板功能包括：

- **账号管理** — 添加/删除/启用/禁用 claude.ai 账号，查看健康状态和使用统计
- **API Key 管理** — 创建和管理 API Key，供客户端认证使用
- **Master Key** — 配置全局共享 Key，允许外部客户端使用账号池
- **模型映射** — 将任意模型名映射到支持的 Claude 模型
- **代理配置** — 全局代理地址，支持 `{sid}` 按账号路由
- **速率限制** — 配置全局令牌桶限流（RPM + Burst）
- **账号保活** — 配置定时健康检查间隔和超时
- **请求统计** — 实时指标和最近请求记录

---

## 快速测试

```bash
# 获取模型列表
curl http://localhost:8080/v1/models \
  -H 'Authorization: ******'

# Chat Completions
curl http://localhost:8080/v1/chat/completions \
  -H 'Authorization: ******' \
  -H 'Content-Type: application/json' \
  -d '{"model":"claude-sonnet-5","messages":[{"role":"user","content":"hello"}]}'

# 流式输出
curl -N http://localhost:8080/v1/chat/completions \
  -H 'Authorization: ******' \
  -H 'Content-Type: application/json' \
  -d '{"model":"claude-sonnet-5","messages":[{"role":"user","content":"hello"}],"stream":true}'
```

更多接口说明请参阅 [API 文档](API.md)。

---

## 健康检查

```bash
curl http://localhost:8080/health
# {"status":"ok"}
```
