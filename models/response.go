package models

import "encoding/json"

// ChatCompletionResponse represents an OpenAI-compatible chat response.
type ChatCompletionResponse struct {
	ID      string   `json:"id"`
	Object  string   `json:"object"`
	Created int64    `json:"created"`
	Model   string   `json:"model"`
	Choices []Choice `json:"choices"`
	Usage   Usage    `json:"usage"`
}

type Choice struct {
	Index        int     `json:"index"`
	Message      Message `json:"message"`
	FinishReason string  `json:"finish_reason"`
}

type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// ChatCompletionChunk represents an OpenAI-compatible streaming chunk.
type ChatCompletionChunk struct {
	ID      string        `json:"id"`
	Object  string        `json:"object"`
	Created int64         `json:"created"`
	Model   string        `json:"model"`
	Choices []ChunkChoice `json:"choices"`
}

type ChunkChoice struct {
	Index        int     `json:"index"`
	Delta        Delta   `json:"delta"`
	FinishReason *string `json:"finish_reason"`
}

// OpenAIToolCallDelta is one incremental tool call in a Chat Completions stream.
type OpenAIToolCallDelta struct {
	Index    int                `json:"index"`
	ID       string             `json:"id,omitempty"`
	Type     string             `json:"type,omitempty"`
	Function OpenAIFunctionCall `json:"function,omitempty"`
}

type Delta struct {
	Role      string                `json:"role,omitempty"`
	Content   string                `json:"content,omitempty"`
	ToolCalls []OpenAIToolCallDelta `json:"tool_calls,omitempty"`
}

type ModelInfo struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

type ModelsResponse struct {
	Object string      `json:"object"`
	Data   []ModelInfo `json:"data"`
}

// ResponsesResponse is an OpenAI Responses API response.
type ResponsesResponse struct {
	ID        string               `json:"id"`
	Object    string               `json:"object"`
	CreatedAt int64                `json:"created_at"`
	Model     string               `json:"model"`
	Status    string               `json:"status"`
	Output    []ResponseOutputItem `json:"output"`
	Usage     ResponsesUsage       `json:"usage"`
}

type ResponseOutputItem struct {
	Type      string                `json:"type"`
	ID        string                `json:"id"`
	Role      string                `json:"role,omitempty"`
	Status    string                `json:"status"`
	Content   []ResponseContentPart `json:"content,omitempty"`
	CallID    string                `json:"call_id,omitempty"`
	Name      string                `json:"name,omitempty"`
	Arguments string                `json:"arguments,omitempty"`
	Output    interface{}           `json:"output,omitempty"`
}

type ResponseContentPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type ResponsesUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens"`
}

// ResponsesStreamEvent is the common envelope for Responses API SSE events.
type ResponsesStreamEvent struct {
	Type string `json:"type"`
}

type ResponsesResponseCreated struct {
	Type     string            `json:"type"`
	Response ResponsesResponse `json:"response"`
}

type ResponsesOutputItemAdded struct {
	Type        string             `json:"type"`
	OutputIndex int                `json:"output_index"`
	Item        ResponseOutputItem `json:"item"`
}

type ResponsesContentPartAdded struct {
	Type         string              `json:"type"`
	ItemID       string              `json:"item_id"`
	OutputIndex  int                 `json:"output_index"`
	ContentIndex int                 `json:"content_index"`
	Part         ResponseContentPart `json:"part"`
}

type ResponsesOutputTextDelta struct {
	Type         string `json:"type"`
	ItemID       string `json:"item_id"`
	OutputIndex  int    `json:"output_index"`
	ContentIndex int    `json:"content_index"`
	Delta        string `json:"delta"`
}

type ResponsesContentPartDone struct {
	Type         string              `json:"type"`
	ItemID       string              `json:"item_id"`
	OutputIndex  int                 `json:"output_index"`
	ContentIndex int                 `json:"content_index"`
	Part         ResponseContentPart `json:"part"`
}

type ResponsesOutputItemDone struct {
	Type        string             `json:"type"`
	OutputIndex int                `json:"output_index"`
	Item        ResponseOutputItem `json:"item"`
}

type ResponsesCompleted struct {
	Type     string            `json:"type"`
	Response ResponsesResponse `json:"response"`
}

// AnthropicResponse is a non-streaming Anthropic Messages API response.
type AnthropicResponse struct {
	ID           string                  `json:"id"`
	Type         string                  `json:"type"`
	Role         string                  `json:"role"`
	Content      []AnthropicContentBlock `json:"content"`
	Model        string                  `json:"model"`
	StopReason   string                  `json:"stop_reason"`
	StopSequence *string                 `json:"stop_sequence"`
	Usage        AnthropicUsage          `json:"usage"`
}

type AnthropicUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens,omitempty"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens,omitempty"`
}

type AnthropicStreamMessageStart struct {
	Type    string            `json:"type"`
	Message AnthropicStartMsg `json:"message"`
}

func (AnthropicStreamMessageStart) SSEEvent() string { return "message_start" }

type AnthropicStartMsg struct {
	ID           string                  `json:"id"`
	Type         string                  `json:"type"`
	Role         string                  `json:"role"`
	Content      []AnthropicContentBlock `json:"content"`
	Model        string                  `json:"model"`
	StopReason   *string                 `json:"stop_reason"`
	StopSequence *string                 `json:"stop_sequence"`
	Usage        AnthropicUsage          `json:"usage"`
}

type AnthropicStreamContentBlockStart struct {
	Type         string                `json:"type"`
	Index        int                   `json:"index"`
	ContentBlock AnthropicContentBlock `json:"content_block"`
}

func (AnthropicStreamContentBlockStart) SSEEvent() string { return "content_block_start" }

type AnthropicStreamDelta struct {
	Type        string `json:"type"`
	Text        string `json:"text,omitempty"`
	PartialJSON string `json:"partial_json,omitempty"`
}

type AnthropicStreamContentBlockDelta struct {
	Type  string               `json:"type"`
	Index int                  `json:"index"`
	Delta AnthropicStreamDelta `json:"delta"`
}

func (AnthropicStreamContentBlockDelta) SSEEvent() string { return "content_block_delta" }

type AnthropicStreamContentBlockStop struct {
	Type  string `json:"type"`
	Index int    `json:"index"`
}

func (AnthropicStreamContentBlockStop) SSEEvent() string { return "content_block_stop" }

type AnthropicStreamMessageDelta struct {
	Type  string             `json:"type"`
	Delta AnthropicStopDelta `json:"delta"`
	Usage AnthropicUsage     `json:"usage"`
}

func (AnthropicStreamMessageDelta) SSEEvent() string { return "message_delta" }

type AnthropicStopDelta struct {
	StopReason   string  `json:"stop_reason"`
	StopSequence *string `json:"stop_sequence"`
}

type AnthropicStreamMessageStop struct {
	Type string `json:"type"`
}

func (AnthropicStreamMessageStop) SSEEvent() string { return "message_stop" }

// ClaudeOrganization represents an organization returned by claude.ai.
type ClaudeOrganization struct {
	UUID string `json:"uuid"`
	Name string `json:"name"`
	Role string `json:"role"`
}

type ClaudeOrganizationsResponse []ClaudeOrganization

// ClaudeConversation represents an upstream claude.ai conversation.
type ClaudeConversation struct {
	UUID             string              `json:"uuid"`
	Name             string              `json:"name"`
	CreatedAt        string              `json:"created_at"`
	UpdatedAt        string              `json:"updated_at"`
	OrganizationUUID string              `json:"organization_uuid"`
	Summary          string              `json:"summary,omitempty"`
	ChatMessages     []ClaudeChatMessage `json:"chat_messages,omitempty"`
}

type ClaudeChatMessage struct {
	UUID        string             `json:"uuid"`
	Text        string             `json:"text"`
	Sender      string             `json:"sender"`
	CreatedAt   string             `json:"created_at"`
	Attachments []ClaudeAttachment `json:"attachments,omitempty"`
	Files       []ClaudeFile       `json:"files,omitempty"`
}

// ClaudeCompletionEvent is one SSE event returned by claude.ai.
type ClaudeCompletionEvent struct {
	Type          string                   `json:"type"`
	Data          string                   `json:"-"`
	Message       *ClaudeCompletionMessage `json:"message,omitempty"`
	Index         int                      `json:"index,omitempty"`
	ContentBlock  *ContentBlock            `json:"content_block,omitempty"`
	Delta         json.RawMessage          `json:"delta,omitempty"`
	TextDelta     *ClaudeCompletionDelta   `json:"-"`
	MessageDelta  *MessageDeltaPayload     `json:"-"`
	StopTimestamp string                   `json:"stop_timestamp,omitempty"`
	MessageLimit  *MessageLimitPayload     `json:"message_limit,omitempty"`
	Error         *ClaudeError             `json:"error,omitempty"`
}

type ClaudeCompletionMessage struct {
	ID           string `json:"id"`
	Type         string `json:"type"`
	Role         string `json:"role"`
	Model        string `json:"model"`
	ParentUUID   string `json:"parent_uuid"`
	UUID         string `json:"uuid"`
	StopReason   string `json:"stop_reason"`
	StopSequence string `json:"stop_sequence"`
	StopDetails  string `json:"stop_details"`
	TraceID      string `json:"trace_id"`
	RequestID    string `json:"request_id"`
}

type ContentBlock struct {
	Type           string        `json:"type"`
	Text           string        `json:"text"`
	Citations      []interface{} `json:"citations"`
	StartTimestamp string        `json:"start_timestamp"`
	StopTimestamp  string        `json:"stop_timestamp"`
}

type ClaudeCompletionDelta struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	Thinking string `json:"thinking,omitempty"`
}

type MessageDeltaPayload struct {
	StopReason   string `json:"stop_reason"`
	StopSequence string `json:"stop_sequence"`
	StopDetails  string `json:"stop_details"`
}

type MessageLimitPayload struct {
	Type string `json:"type"`
}

type ClaudeError struct {
	ErrorType string `json:"error_type"`
	Message   string `json:"message"`
}

type ClaudeConversationsResponse struct {
	Results []ClaudeConversation `json:"results"`
	HasMore bool                 `json:"has_more"`
	Total   int                  `json:"total"`
}
