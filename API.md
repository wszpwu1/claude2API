# API 文档

> 中文 API 文档 | [English API](API_EN.md) | [README](README.md) | [English README](README_EN.md)

基础地址：

```text
http://127.0.0.1:8080/v1
```

所有 `/v1` 下的接口都需要认证。可以使用以下任意一种方式：

```http
Authorization: Bearer <claude.ai sessionKey>
```

或：

```http
X-Claude-Cookie: <完整 claude.ai 浏览器 Cookie>
```

如果服务端已经配置 `CLAUDE_SESSION_KEY` 或 `CLAUDE_COOKIE`，请求端可以省略对应 Header。

## 错误格式

错误响应使用类似 OpenAI 的 JSON 格式：

```json
{
  "error": {
    "message": "错误信息",
    "type": "invalid_request_error"
  }
}
```

常见状态码：

| 状态码 | 说明 |
| --- | --- |
| `400` | 请求体无效或模型不支持。 |
| `401` | 缺少 sessionKey 或浏览器 Cookie。 |
| `502` | 上游 claude.ai 请求失败。 |

## 模型列表

### `GET /v1/models`

返回当前支持的模型列表。

```bash
curl http://127.0.0.1:8080/v1/models \
  -H 'Authorization: Bearer <sessionKey>'
```

响应示例：

```json
{
  "object": "list",
  "data": [
    {
      "id": "claude-sonnet-5",
      "object": "model",
      "created": 1783590000,
      "owned_by": "anthropic"
    }
  ]
}
```

支持的模型 ID：

- `claude-fable-5`
- `claude-opus-4-8`
- `claude-haiku-4-5`
- `claude-opus-4-7`
- `claude-opus-4-6`
- `claude-opus-3`
- `claude-sonnet-4-6`
- `claude-sonnet-5`
- `claude-opus-5`

## OpenAI Chat Completions

### `POST /v1/chat/completions`

OpenAI Chat Completions 兼容接口。

### 请求体

```json
{
  "model": "claude-sonnet-5",
  "messages": [
    {"role": "system", "content": "You are concise."},
    {"role": "user", "content": "Say hello"}
  ],
  "stream": false,
  "max_tokens": 1024,
  "temperature": 0.7,
  "top_p": 1
}
```

字段说明：

| 字段 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `model` | string | 否 | 为空时使用 `DEFAULT_MODEL`。必须是支持模型。 |
| `messages` | array | 是 | 至少一条消息。支持 `system`、`user`、`assistant`。 |
| `stream` | boolean | 否 | 为 `true` 时返回 SSE 流。 |
| `conversation_id` | string | 否 | 传入后启用持久会话，同一个 ID 会复用同一个 claude.ai conversation。 |
| `max_tokens` | integer | 否 | 兼容字段。 |
| `temperature` | number | 否 | 兼容字段。 |
| `top_p` | number | 否 | 兼容字段。 |
| `attachments` | array | 否 | 请求模型中保留，文件上游行为有限。 |

内部会把 messages 转换成 claude.ai 网页使用的单 prompt 格式，包含 `[System]`、`[Human]`、`[Assistant]` 段落。

### 非流式示例

```bash
curl http://127.0.0.1:8080/v1/chat/completions \
  -H 'Authorization: Bearer <sessionKey>' \
  -H 'Content-Type: application/json' \
  -d '{
    "model": "claude-sonnet-5",
    "messages": [{"role": "user", "content": "Reply with exactly: pong"}],
    "stream": false
  }'
```

响应示例：

```json
{
  "id": "chatcmpl-e0f87945",
  "object": "chat.completion",
  "created": 1783594773,
  "model": "claude-sonnet-5",
  "choices": [
    {
      "index": 0,
      "message": {
        "role": "assistant",
        "content": "pong"
      },
      "finish_reason": "stop"
    }
  ],
  "usage": {
    "prompt_tokens": 0,
    "completion_tokens": 1,
    "total_tokens": 0
  }
}
```

### 流式示例

```bash
curl -N http://127.0.0.1:8080/v1/chat/completions \
  -H 'Authorization: Bearer <sessionKey>' \
  -H 'Content-Type: application/json' \
  -d '{
    "model": "claude-sonnet-5",
    "messages": [{"role": "user", "content": "Reply with exactly: pong"}],
    "stream": true
  }'
```

响应流：

```text
data: {"id":"chatcmpl-...","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}

data: {"id":"chatcmpl-...","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"pong"},"finish_reason":null}]}

data: {"id":"chatcmpl-...","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}

data: [DONE]
```

## Anthropic Messages

### `POST /v1/messages`

Anthropic Messages 兼容接口。

### 请求体

```json
{
  "model": "claude-sonnet-5",
  "max_tokens": 1024,
  "system": "You are concise.",
  "messages": [
    {"role": "user", "content": "Say hello"}
  ],
  "stream": false
}
```

字段说明：

| 字段 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `model` | string | 否 | 为空时使用 `DEFAULT_MODEL`。必须是支持模型。 |
| `messages` | array | 是 | 至少一条消息。 |
| `system` | string | 否 | 作为 system prompt 前置。 |
| `max_tokens` | integer | 否 | 为空或 0 时默认 `4096`。 |
| `stream` | boolean | 否 | 为 `true` 时返回 Anthropic 风格 SSE。 |
| `conversation_id` | string | 否 | 传入后启用持久会话，同一个 ID 会复用同一个 claude.ai conversation。 |
| `temperature` | number | 否 | 兼容字段。 |
| `top_p` | number | 否 | 兼容字段。 |
| `top_k` | integer | 否 | 兼容字段。 |
| `stop_sequences` | array | 否 | 兼容字段。 |

`content` 可以是字符串，也可以是 text block 数组：

```json
{"role": "user", "content": [{"type": "text", "text": "Hello"}]}
```

### 非流式响应

```json
{
  "id": "msg_...",
  "type": "message",
  "role": "assistant",
  "content": [
    {"type": "text", "text": "Hello!"}
  ],
  "model": "claude-sonnet-5",
  "stop_reason": "end_turn",
  "stop_sequence": null,
  "usage": {
    "input_tokens": 0,
    "output_tokens": 2
  }
}
```

### 流式事件

响应是 `data: <json>\n\n` 格式的 SSE 流，主要事件：

- `message_start`
- `content_block_start`
- `content_block_delta`
- `content_block_stop`
- `message_delta`
- `message_stop`

示例：

```text
data: {"type":"message_start","message":{"role":"assistant"}}

data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}

data: {"type":"content_block_stop","index":0}

data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}

data: {"type":"message_stop"}
```

## OpenAI Responses API

### `POST /v1/responses`

OpenAI Responses 兼容接口。

### 字符串 input

```json
{
  "model": "claude-sonnet-5",
  "instructions": "You are concise.",
  "input": "Say hello",
  "stream": false
}
```

### 结构化 input

```json
{
  "model": "claude-sonnet-5",
  "input": [
    {
      "type": "message",
      "role": "user",
      "content": [
        {"type": "input_text", "text": "Say hello"}
      ]
    }
  ],
  "stream": false
}
```

字段说明：

| 字段 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `model` | string | 否 | 为空时使用 `DEFAULT_MODEL`。必须是支持模型。 |
| `input` | string 或 array | 否 | 输入内容。 |
| `instructions` | string | 否 | 作为 system prompt 前置。 |
| `stream` | boolean | 否 | 为 `true` 时返回 Responses 风格 SSE。 |
| `conversation_id` | string | 否 | 传入后启用持久会话，同一个 ID 会复用同一个 claude.ai conversation。 |
| `max_output_tokens` | integer | 否 | 兼容字段。 |
| `temperature` | number | 否 | 兼容字段。 |
| `top_p` | number | 否 | 兼容字段。 |

### 非流式响应

```json
{
  "id": "resp_...",
  "object": "response",
  "created_at": 1783594773,
  "model": "claude-sonnet-5",
  "status": "completed",
  "output": [
    {
      "type": "message",
      "id": "msg_...",
      "role": "assistant",
      "status": "completed",
      "content": [
        {"type": "output_text", "text": "Hello!"}
      ]
    }
  ],
  "usage": {
    "input_tokens": 0,
    "output_tokens": 2,
    "total_tokens": 2
  }
}
```

### 流式事件

响应是 `data: <json>\n\n` 格式的 SSE 流，主要事件：

- `response.created`
- `response.output_item.added`
- `response.content_part.added`
- `response.output_text.delta`
- `response.content_part.done`
- `response.output_item.done`
- `response.completed`

示例：

```text
data: {"type":"response.created","response":{"id":"resp_...","status":"in_progress"}}

data: {"type":"response.output_item.added","output_index":0,"item":{"type":"message","role":"assistant"}}

data: {"type":"response.content_part.added","output_index":0,"content_index":0,"part":{"type":"output_text","text":""}}

data: {"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"Hello"}

data: {"type":"response.content_part.done","output_index":0,"content_index":0,"part":{"type":"output_text","text":"Hello"}}

data: {"type":"response.output_item.done","output_index":0,"item":{"type":"message","status":"completed"}}

data: {"type":"response.completed","response":{"status":"completed"}}
```

## 对话连续性

默认情况下，不传 `conversation_id` 时，每次请求都会创建临时 claude.ai conversation，请求结束后自动删除。

如果传入 `conversation_id`，服务端会在内存中保存映射，并复用同一个 claude.ai conversation：

```json
{
  "conversation_id": "my-chat-001",
  "model": "claude-sonnet-5",
  "messages": [
    {"role": "user", "content": "继续刚才的话题"}
  ]
}
```

删除持久会话：

```bash
curl -X DELETE http://127.0.0.1:8080/v1/conversations/my-chat-001 \
  -H 'Authorization: Bearer <sessionKey>'
```

响应：

```json
{"id":"my-chat-001","deleted":true}
```

注意：当前会话映射保存在内存中，服务重启后会丢失。

## 完整浏览器 Cookie 用法

如果 Bearer sessionKey 模式和浏览器行为不一致，或遇到浏览器正常但本地请求异常的情况，建议传完整 claude.ai 浏览器 Cookie：

```bash
curl http://127.0.0.1:8080/v1/chat/completions \
  -H 'X-Claude-Cookie: sessionKey=...; sessionKeyLC=...; anthropic-device-id=...; lastActiveOrg=...; ...' \
  -H 'Content-Type: application/json' \
  -d '{
    "model": "claude-sonnet-5",
    "messages": [{"role": "user", "content": "Reply with exactly: pong"}],
    "stream": false
  }'
```

没有 Bearer token 时，代理会尝试从 Cookie 中提取 `sessionKey`。

## sessionKey 模式下自动生成的环境

当只传 `sessionKey` 时，代理服务端会自动生成前端可生成的浏览器环境，并补齐更接近浏览器的请求结构，例如：

- `sessionKeyLC`
- `anthropic-device-id`
- `activitySessionId`
- `ajs_anonymous_id`
- `__ssid`
- `_dd_s`
- `traceparent` / Datadog RUM 相关 Header
- 部分 UI / analytics Cookie
- completion 请求中的 claude.ai web `tools` 字段
- 按阶段变化的 Referer：`/new`、`/chat/<conversation_id>`

这些值属于 UUID、时间戳、前端 session 或 RUM trace 类环境，可以由本服务生成并在一个 client 生命周期内保持一致。

以下属于服务端签名或 Cloudflare 下发的值，不能伪造；在只传 `sessionKey` 时，本服务不会生成，也不会传递：

- `routingHint`
- `cf_clearance`
- `__cf_bm`
- `_cfuvid`

`routingHint` 是 claude.ai 后端签发的路由提示，值通常是 `sk-ant-rh-...` 形式，内部结构类似带签名的 JWT。它一般在登录、会话刷新、账号路由初始化或组织信息加载时由 claude.ai 服务端下发；客户端最多只能原样保存和回传，不能自行生成有效值。

如果确实需要这些值，请使用 `X-Claude-Cookie` 或 `CLAUDE_COOKIE` 传入真实浏览器完整 Cookie。

## 不支持的接口

以下接口没有注册路由，因此不会支持：

- `/v1/images/generations`
- `/v1/images/edits`
- `/v1/images/variations`
