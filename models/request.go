package models

import "encoding/json"

// ChatCompletionRequest represents an OpenAI-compatible chat request.
type ChatCompletionRequest struct {
	Model             string       `json:"model"`
	Messages          []Message    `json:"messages"`
	Stream            bool         `json:"stream"`
	ConversationID    string       `json:"conversation_id,omitempty"`
	MaxTokensToSample int          `json:"max_tokens,omitempty"`
	Temperature       float64      `json:"temperature,omitempty"`
	TopP              float64      `json:"top_p,omitempty"`
	System            string       `json:"system,omitempty"`
	Attachments       []Attachment `json:"attachments,omitempty"`
}

// Message represents an OpenAI-compatible chat message.
type Message struct {
	Role        string       `json:"role"`
	Content     string       `json:"content"`
	Attachments []Attachment `json:"attachments,omitempty"`
}

// Attachment represents an attachment accepted by the public API.
type Attachment struct {
	FileName         string `json:"file_name"`
	FileType         string `json:"file_type"`
	FileSize         int    `json:"file_size,omitempty"`
	FileBase64       string `json:"file_base64,omitempty"`
	URL              string `json:"url,omitempty"`
	ExtractedContent string `json:"extracted_content,omitempty"`
}

// ResponsesRequest is the OpenAI Responses API request.
type ResponsesRequest struct {
	Model           string        `json:"model"`
	Input           ResponseInput `json:"input"`
	Instructions    string        `json:"instructions,omitempty"`
	Stream          bool          `json:"stream,omitempty"`
	ConversationID  string        `json:"conversation_id,omitempty"`
	MaxOutputTokens int           `json:"max_output_tokens,omitempty"`
	Temperature     float64       `json:"temperature,omitempty"`
	TopP            float64       `json:"top_p,omitempty"`
}

// ResponseInput can be a string or an array of input items.
type ResponseInput interface{}

// ResponseInputItem is one item in a structured Responses API input array.
type ResponseInputItem struct {
	Type    string              `json:"type"`
	Role    string              `json:"role"`
	Content ResponseItemContent `json:"content"`
}

// ResponseItemContent can be a string or an array of content parts.
type ResponseItemContent interface{}

// AnthropicTool is a tool definition supplied by an Anthropic-compatible client.
type AnthropicTool struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description,omitempty"`
	InputSchema map[string]interface{} `json:"input_schema,omitempty"`
}

// AnthropicRequest is the Anthropic Messages API request.
type AnthropicRequest struct {
	Model          string             `json:"model"`
	Messages       []AnthropicMessage `json:"messages"`
	System         interface{}        `json:"system,omitempty"`
	MaxTokens      int                `json:"max_tokens"`
	Stream         bool               `json:"stream,omitempty"`
	ConversationID string             `json:"conversation_id,omitempty"`
	ToolDefs       []AnthropicTool    `json:"tools,omitempty"`
	Thinking       interface{}        `json:"thinking,omitempty"`
	Temperature    float64            `json:"temperature,omitempty"`
	TopP           float64            `json:"top_p,omitempty"`
	TopK           int                `json:"top_k,omitempty"`
	StopSequences  []string           `json:"stop_sequences,omitempty"`
}

// AnthropicMessage content can be a string or an array of content blocks.
type AnthropicMessage struct {
	Role    string      `json:"role"`
	Content interface{} `json:"content"`
}

// AnthropicContentBlock is shared by Anthropic-compatible requests and responses.
type AnthropicContentBlock struct {
	Type         string                 `json:"type"`
	Text         string                 `json:"text,omitempty"`
	ID           string                 `json:"id,omitempty"`
	Name         string                 `json:"name,omitempty"`
	Input        map[string]interface{} `json:"input,omitempty"`
	Content      interface{}            `json:"content,omitempty"`
	IsError      *bool                  `json:"is_error,omitempty"`
	UseID        string                 `json:"use_id,omitempty"`
	CacheControl interface{}            `json:"cache_control,omitempty"`
}

// ClaudeCreateConversationRequest is sent to claude.ai when creating a conversation.
type ClaudeCreateConversationRequest struct {
	Name             string `json:"name"`
	OrganizationUUID string `json:"organization_uuid,omitempty"`
}

// ClaudeCompletionRequest is sent to the claude.ai completion endpoint.
type ClaudeCompletionRequest struct {
	Prompt                   string                    `json:"prompt"`
	ParentMessageUUID        string                    `json:"parent_message_uuid,omitempty"`
	Timezone                 string                    `json:"timezone"`
	Locale                   string                    `json:"locale"`
	Model                    string                    `json:"model"`
	Effort                   string                    `json:"effort,omitempty"`
	ThinkingMode             string                    `json:"thinking_mode,omitempty"`
	Tools                    json.RawMessage           `json:"tools,omitempty"`
	TurnMessageUUIDs         *TurnMessageUUIDs         `json:"turn_message_uuids,omitempty"`
	Attachments              []ClaudeAttachment        `json:"attachments"`
	Files                    []ClaudeFile              `json:"files"`
	SyncSources              []interface{}             `json:"sync_sources"`
	RenderingMode            string                    `json:"rendering_mode"`
	CreateConversationParams *CreateConversationParams `json:"create_conversation_params,omitempty"`
}

// CreateConversationParams mirrors claude.ai web create-conversation options.
type CreateConversationParams struct {
	Name                           string      `json:"name"`
	Model                          string      `json:"model"`
	IncludeConversationPreferences bool        `json:"include_conversation_preferences"`
	PaprikaMode                    interface{} `json:"paprika_mode"`
	CompassMode                    interface{} `json:"compass_mode"`
	ToolSearchMode                 string      `json:"tool_search_mode"`
	IsTemporary                    bool        `json:"is_temporary"`
	EnabledImagine                 bool        `json:"enabled_imagine"`
}

// TurnMessageUUIDs holds the human and assistant UUIDs for one upstream turn.
type TurnMessageUUIDs struct {
	HumanMessageUUID     string `json:"human_message_uuid"`
	AssistantMessageUUID string `json:"assistant_message_uuid"`
}

// ClaudeAttachment is an attachment in claude.ai's internal format.
type ClaudeAttachment struct {
	FileName         string `json:"file_name"`
	FileType         string `json:"file_type"`
	FileSize         int    `json:"file_size"`
	ExtractedContent string `json:"extracted_content,omitempty"`
}

// ClaudeFile is a file uploaded to claude.ai.
type ClaudeFile struct {
	FileName   string `json:"file_name"`
	FileType   string `json:"file_type"`
	FileSize   int    `json:"file_size"`
	FileBase64 string `json:"file_base64,omitempty"`
}
