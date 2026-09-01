# claude2API

> [中文](README.md) | English | [API 文档](API.md) | [English API](API_EN.md)

A proxy service that converts the claude.ai web interface into a standard API, compatible with OpenAI Chat Completions, Anthropic Messages, and OpenAI Responses protocols. Includes a built-in Web admin panel.

## Features

- **Multi-protocol support**
  - `POST /v1/chat/completions` — OpenAI Chat Completions
  - `POST /v1/messages` — Anthropic Messages
  - `POST /v1/responses` — OpenAI Responses
  - `GET /v1/models` — Model list
  - `DELETE /v1/conversations/:id` — Delete persistent conversation
- **Multi-account management** — Configure multiple claude.ai accounts with automatic least-loaded routing
- **Web admin panel** — Account management, API key management, request metrics, model mappings, proxy configuration, and more
- **Streaming** — SSE streaming output support
- **Persistent conversations** — Reuse claude.ai conversations via `conversation_id`
- **Keep-alive** — Periodic health checks; unhealthy accounts are marked and automatically restored after cooldown
- **Rate limiting** — Configurable global token-bucket rate limiter
- **Model mappings** — Map arbitrary model names to supported Claude models
- **Proxy support** — Configure upstream HTTP/HTTPS proxy per account

### Supported Models

| Model ID | Notes |
|---|---|
| `claude-sonnet-5` | Default model |
| `claude-opus-5` | |
| `claude-opus-4-8` | |
| `claude-opus-4-7` | |
| `claude-opus-4-6` | |
| `claude-opus-3` | |
| `claude-sonnet-4-6` | |
| `claude-haiku-4-5` | |
| `claude-fable-5` | |

---

## Deployment

### Option 1: Docker Compose (Recommended)

**1. Clone the repository**

```bash
git clone https://github.com/wszpwu1/claude2API.git
cd claude2API
```

**2. Prepare an accounts file (optional)**

To configure multiple accounts, create `accounts.txt` with one account per line:

```text
# sessionKey mode (recommended)
sk-ant-sid01-xxxxxxxx

# Full Cookie mode (auto-detected when line contains sessionKey=)
sessionKey=sk-ant-sid01-xxxxxxxx; sessionKeyLC=...; anthropic-device-id=...
```

Lines starting with `#` are ignored.

**3. Configure environment variables**

Create a `.env` file:

```env
PORT=8080
CLAUDE_SESSION_KEY=sk-ant-sid01-xxxxxxxx
CLAUDE_COOKIE=
CLAUDE_ACCOUNTS_FILE=./accounts.txt
CLAUDE_BASE_URL=https://claude.ai
CLAUDE_TIMEZONE=Asia/Singapore
CLAUDE_LOCALE=en-US
DEFAULT_MODEL=claude-sonnet-5
ADMIN_INITIAL_PASSWORD=yourpassword
```

> `ADMIN_INITIAL_PASSWORD` only takes effect when the admin data file (`data/admin.json`) does not yet exist — it sets the initial login password.

**4. Add data directory volume (persist admin panel configuration)**

Append to the `volumes` section in `docker-compose.yml`:

```yaml
      - ./data:/app/data
```

**5. Start the service**

```bash
docker compose up -d
```

The service listens at `http://localhost:8080`. Admin panel: `http://localhost:8080/admin`.

---

### Option 2: Build from source

Requires Go 1.21+.

```bash
git clone https://github.com/wszpwu1/claude2API.git
cd claude2API
go build -o claude2api .

export CLAUDE_SESSION_KEY=sk-ant-sid01-xxxxxxxx
export ADMIN_INITIAL_PASSWORD=yourpassword
./claude2api
```

---

## Environment Variables

| Variable | Default | Description |
|---|---|---|
| `PORT` | `8080` | Listen port |
| `CLAUDE_BASE_URL` | `https://claude.ai` | claude.ai base URL |
| `CLAUDE_SESSION_KEY` | — | Single-account sessionKey |
| `CLAUDE_COOKIE` | — | Single-account full browser Cookie |
| `CLAUDE_ACCOUNTS_FILE` | `accounts.txt` | Path to multi-account file |
| `CLAUDE_PROXY_URL` | — | Global upstream proxy URL (supports `{sid}` placeholder) |
| `CLAUDE_TIMEZONE` | `Asia/Singapore` | Timezone used in requests |
| `CLAUDE_LOCALE` | `en-US` | Locale used in requests |
| `DEFAULT_MODEL` | `claude-sonnet-5` | Default model when not specified in request |
| `CLAUDE_EFFORT` | `medium` | Thinking effort level (`low` / `medium` / `high`) |
| `CLAUDE_THINKING` | — | Thinking mode (`auto` / `none` / `enabled` / JSON object) |
| `ADMIN_DATA_FILE` | `data/admin.json` | Admin panel data persistence path |
| `ADMIN_INITIAL_PASSWORD` | `admin` | Admin panel initial password (effective when file doesn't exist) |

---

## Authentication

All `/v1` endpoints require authentication. Use one of:

```http
Authorization: ******
```

Or pass a full browser Cookie:

```http
X-Claude-Cookie: sessionKey=...; sessionKeyLC=...; anthropic-device-id=...
```

If the server is configured with accounts via environment variables or the accounts file, clients may pass any non-empty string as the ******

You can also configure a **Master Key** or **API Keys** in the admin panel for clients to access the shared account pool.

---

## Admin Panel

Visit `http://localhost:8080/admin` and log in with the initial password (default: `admin`).

Panel features:

- **Accounts** — Add/remove/enable/disable claude.ai accounts; view health status and usage statistics
- **API Keys** — Create and manage API keys for client authentication
- **Master Key** — Configure a global shared key for external clients to use the account pool
- **Model Mappings** — Map arbitrary model names to supported Claude models
- **Proxy** — Global proxy URL with `{sid}` per-account routing support
- **Rate Limiting** — Configure global token-bucket rate limiting (RPM + Burst)
- **Keep-alive** — Configure periodic health check interval and timeout
- **Metrics** — Real-time request metrics and recent request history

---

## Quick Test

```bash
# List models
curl http://localhost:8080/v1/models \
  -H 'Authorization: ******'

# Chat Completions
curl http://localhost:8080/v1/chat/completions \
  -H 'Authorization: ******' \
  -H 'Content-Type: application/json' \
  -d '{"model":"claude-sonnet-5","messages":[{"role":"user","content":"hello"}]}'

# Streaming
curl -N http://localhost:8080/v1/chat/completions \
  -H 'Authorization: ******' \
  -H 'Content-Type: application/json' \
  -d '{"model":"claude-sonnet-5","messages":[{"role":"user","content":"hello"}],"stream":true}'
```

For full API reference, see [API_EN.md](API_EN.md).

---

## Health Check

```bash
curl http://localhost:8080/health
# {"status":"ok"}
```
