package handlers

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"claude2api/claude"
	"claude2api/models"

	"github.com/gin-gonic/gin"
)

// ChatCompletion handles POST /v1/chat/completions (OpenAI format, streaming + non-streaming)
func (h *Handler) ChatCompletion(c *gin.Context) {
	var req models.ChatCompletionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, "invalid request body: "+err.Error())
		return
	}
	if len(req.Messages) == 0 {
		badRequest(c, "messages must contain at least one message")
		return
	}

	claudeModel, err := h.resolveModel(req.Model, h.cfg.DefaultModel)
	if err != nil {
		badRequest(c, err.Error())
		return
	}
	c.Set("requestModel", claudeModel)

	lease, err := h.acquireClient(c, req.ConversationID)
	if err != nil {
		internalError(c, "create client: "+err.Error())
		return
	}
	defer lease.release()

	effort := resolveEffort(h.cfg.Effort)
	if len(req.Tools) > 0 {
		anthropicReq := chatRequestToAnthropic(req)
		if req.Stream {
			h.chatToolStream(c, lease.client, anthropicReq, claudeModel, effort, lease.accountID)
		} else {
			h.chatToolNonStream(c, lease.client, anthropicReq, claudeModel, effort, lease.accountID)
		}
		return
	}

	prompt := claude.BuildPrompt(req.Messages)
	if req.Stream {
		h.chatCompletionStream(c, lease.client, prompt, claudeModel, effort, req.ConversationID, lease.accountID)
	} else {
		h.chatCompletionNonStream(c, lease.client, prompt, claudeModel, effort, req.ConversationID, lease.accountID)
	}
}

func chatRequestToAnthropic(req models.ChatCompletionRequest) models.AnthropicRequest {
	messages := make([]models.AnthropicMessage, 0, len(req.Messages))
	// Collect system/developer messages from the messages array so they are not silently dropped.
	var systemParts []string
	for _, message := range req.Messages {
		if message.Role == "system" || message.Role == "developer" {
			if s := anthropicContentToString(message.Content); s != "" {
				systemParts = append(systemParts, s)
			}
			continue
		}
		if message.Role == "tool" {
			messages = append(messages, models.AnthropicMessage{Role: "user", Content: []interface{}{map[string]interface{}{
				"type": "tool_result", "tool_use_id": message.ToolCallID, "content": message.Content,
			}}})
			continue
		}
		blocks := make([]interface{}, 0, len(message.ToolCalls)+1)
		if message.Content != nil {
			// anthropicContentToString normalizes string and multi-part content alike,
			// preventing a []interface{} from being serialized as a JSON array inside
			// the "text" field, which would produce a malformed Anthropic request.
			if textContent := anthropicContentToString(message.Content); textContent != "" {
				blocks = append(blocks, map[string]interface{}{"type": "text", "text": textContent})
			}
		}
		for _, call := range message.ToolCalls {
			blocks = append(blocks, openAIToolCallToAnthropic(call))
		}
		content := message.Content
		if len(blocks) > 0 {
			content = blocks
		}
		messages = append(messages, models.AnthropicMessage{Role: message.Role, Content: content})
	}
	// Merge collected system messages with the top-level system field (top-level takes precedence as suffix).
	var system interface{} = req.System
	if len(systemParts) > 0 {
		merged := strings.Join(systemParts, "\n\n")
		if req.System != "" {
			merged = merged + "\n\n" + req.System
		}
		system = merged
	}
	return models.AnthropicRequest{
		Model: req.Model, Messages: messages, System: system, MaxTokens: req.MaxTokensToSample,
		Stream: req.Stream, ConversationID: req.ConversationID, ToolDefs: openAIToolsToAnthropic(req.Tools),
		ToolChoice: openAIToolChoiceToAnthropic(req.ToolChoice), Temperature: req.Temperature, TopP: req.TopP,
	}
}

func (h *Handler) chatToolNonStream(c *gin.Context, client *claude.Client, req models.AnthropicRequest, claudeModel, effort, accountID string) {
	blocks, usage, err := h.runToolLoop(c.Request.Context(), client, req, claudeModel, effort, accountID)
	if err != nil {
		upstreamError(c, err.Error())
		return
	}
	message := models.Message{Role: "assistant"}
	var text strings.Builder
	for _, block := range blocks {
		switch block.Type {
		case "text":
			text.WriteString(block.Text)
		case "tool_use":
			message.ToolCalls = append(message.ToolCalls, anthropicToolUseToOpenAI(block))
		}
	}
	message.Content = text.String()
	finishReason := "stop"
	if len(message.ToolCalls) > 0 {
		finishReason = "tool_calls"
	}
	c.Set("inputTokens", int64(usage.InputTokens))
	c.Set("outputTokens", int64(usage.OutputTokens))
	c.JSON(http.StatusOK, models.ChatCompletionResponse{
		ID: genID("chatcmpl-"), Object: "chat.completion", Created: time.Now().Unix(), Model: claudeModel,
		Choices: []models.Choice{{Index: 0, Message: message, FinishReason: finishReason}},
		Usage:   models.Usage{PromptTokens: usage.InputTokens, CompletionTokens: usage.OutputTokens, TotalTokens: usage.InputTokens + usage.OutputTokens},
	})
}

func (h *Handler) chatToolStream(c *gin.Context, client *claude.Client, req models.AnthropicRequest, claudeModel, effort, accountID string) {
	blocks, usage, err := h.runToolLoop(c.Request.Context(), client, req, claudeModel, effort, accountID)
	if err != nil {
		upstreamError(c, err.Error())
		return
	}
	c.Writer.Header().Set("Content-Type", "text/event-stream")
	c.Writer.Header().Set("Cache-Control", "no-cache")
	c.Writer.Header().Set("Connection", "keep-alive")
	c.Writer.Header().Set("X-Accel-Buffering", "no")
	chunkID, created := genID("chatcmpl-"), time.Now().Unix()
	writeSSE(c.Writer, models.ChatCompletionChunk{ID: chunkID, Object: "chat.completion.chunk", Created: created, Model: claudeModel, Choices: []models.ChunkChoice{{Index: 0, Delta: models.Delta{Role: "assistant"}}}})
	toolIndex := 0
	for _, block := range blocks {
		if block.Type == "text" && block.Text != "" {
			writeSSE(c.Writer, models.ChatCompletionChunk{ID: chunkID, Object: "chat.completion.chunk", Created: created, Model: claudeModel, Choices: []models.ChunkChoice{{Index: 0, Delta: models.Delta{Content: block.Text}}}})
		}
		if block.Type == "tool_use" {
			call := anthropicToolUseToOpenAI(block)
			writeSSE(c.Writer, models.ChatCompletionChunk{ID: chunkID, Object: "chat.completion.chunk", Created: created, Model: claudeModel, Choices: []models.ChunkChoice{{Index: 0, Delta: models.Delta{ToolCalls: []models.OpenAIToolCallDelta{{Index: toolIndex, ID: call.ID, Type: call.Type, Function: call.Function}}}}}})
			toolIndex++
		}
	}
	c.Set("inputTokens", int64(usage.InputTokens))
	c.Set("outputTokens", int64(usage.OutputTokens))
	stop := "stop"
	if toolIndex > 0 {
		stop = "tool_calls"
	}
	writeSSE(c.Writer, models.ChatCompletionChunk{ID: chunkID, Object: "chat.completion.chunk", Created: created, Model: claudeModel, Choices: []models.ChunkChoice{{Index: 0, Delta: models.Delta{}, FinishReason: &stop}}})
	_, _ = c.Writer.WriteString("data: [DONE]\n\n")
	if flusher, ok := c.Writer.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (h *Handler) chatCompletionNonStream(c *gin.Context, client *claude.Client, prompt, claudeModel, effort, conversationID, accountID string) {
	_, content, err := h.runCompletion(c.Request.Context(), client, prompt, claudeModel, effort, conversationID, accountID, nil, []json.RawMessage{})
	if err != nil {
		upstreamError(c, err.Error())
		return
	}
	// Use rune count for accurate token estimation with multi-byte text.
	promptTokens := len([]rune(prompt)) / 4
	completionTokens := len([]rune(content)) / 4
	c.Set("inputTokens", int64(promptTokens))
	c.Set("outputTokens", int64(completionTokens))
	resp := models.ChatCompletionResponse{
		ID:      genID("chatcmpl-"),
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   claudeModel,
		Choices: []models.Choice{{
			Index:        0,
			Message:      models.Message{Role: "assistant", Content: content},
			FinishReason: "stop",
		}},
		Usage: models.Usage{
			PromptTokens:     promptTokens,
			CompletionTokens: completionTokens,
			TotalTokens:      promptTokens + completionTokens,
		},
	}
	c.JSON(http.StatusOK, resp)
}

func (h *Handler) chatCompletionStream(c *gin.Context, client *claude.Client, prompt, claudeModel, effort, conversationID, accountID string) {
	c.Writer.Header().Set("Content-Type", "text/event-stream")
	c.Writer.Header().Set("Cache-Control", "no-cache")
	c.Writer.Header().Set("Connection", "keep-alive")
	c.Writer.Header().Set("X-Accel-Buffering", "no")
	flusher, _ := c.Writer.(http.Flusher)

	chunkID := genID("chatcmpl-")
	created := time.Now().Unix()

	// role chunk
	writeSSE(c.Writer, models.ChatCompletionChunk{
		ID: chunkID, Object: "chat.completion.chunk", Created: created, Model: claudeModel,
		Choices: []models.ChunkChoice{{Index: 0, Delta: models.Delta{Role: "assistant"}}},
	})
	if flusher != nil {
		flusher.Flush()
	}

	var full strings.Builder
	_, _, err := h.runCompletion(c.Request.Context(), client, prompt, claudeModel, effort, conversationID, accountID, func(text string) {
		full.WriteString(text)
		writeSSE(c.Writer, models.ChatCompletionChunk{
			ID: chunkID, Object: "chat.completion.chunk", Created: created, Model: claudeModel,
			Choices: []models.ChunkChoice{{Index: 0, Delta: models.Delta{Content: text}}},
		})
		if flusher != nil {
			flusher.Flush()
		}
	}, []json.RawMessage{})
	c.Set("inputTokens", int64(len([]rune(prompt))/4))
	c.Set("outputTokens", int64(len([]rune(full.String()))/4))
	finishReason := "stop"
	if err != nil {
		// Emit the error as an inline delta so clients see it, then close with
		// finish_reason "error" instead of the misleading "stop".
		writeSSE(c.Writer, models.ChatCompletionChunk{
			ID: chunkID, Object: "chat.completion.chunk", Created: created, Model: claudeModel,
			Choices: []models.ChunkChoice{{Index: 0, Delta: models.Delta{Content: "[error: " + err.Error() + "]"}}},
		})
		finishReason = "error"
	}

	writeSSE(c.Writer, models.ChatCompletionChunk{
		ID: chunkID, Object: "chat.completion.chunk", Created: created, Model: claudeModel,
		Choices: []models.ChunkChoice{{Index: 0, Delta: models.Delta{}, FinishReason: &finishReason}},
	})
	_, _ = c.Writer.WriteString("data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}
