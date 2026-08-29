package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"claude2api/claude"
	"claude2api/models"

	"github.com/gin-gonic/gin"
)

// AnthropicMessages handles POST /v1/messages (Anthropic format, streaming + non-streaming)
func (h *Handler) AnthropicMessages(c *gin.Context) {
	var req models.AnthropicRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, "invalid request body: "+err.Error())
		return
	}
	if len(req.Messages) == 0 {
		badRequest(c, "messages must contain at least one message")
		return
	}
	if req.MaxTokens == 0 {
		req.MaxTokens = 4096
	}

	claudeModel, err := resolveModel(req.Model, h.cfg.DefaultModel)
	if err != nil {
		badRequest(c, err.Error())
		return
	}

	lease, err := h.acquireClient(c, req.ConversationID)
	if err != nil {
		internalError(c, "create client: "+err.Error())
		return
	}
	defer lease.release()

	effort := resolveEffort(h.cfg.Effort)

	// If the request carries tool definitions, run the tool-use loop which
	// simulates native tool calling over claude.ai's text-only interface.
	if len(req.ToolDefs) > 0 {
		if req.Stream {
			h.anthropicToolStream(c, lease.client, req, claudeModel, effort, lease.accountID)
		} else {
			h.anthropicToolNonStream(c, lease.client, req, claudeModel, effort, lease.accountID)
		}
		return
	}

	prompt := buildAnthropicPrompt(req)
	if req.Stream {
		h.anthropicStream(c, lease.client, prompt, claudeModel, effort, req.ConversationID, lease.accountID)
	} else {
		h.anthropicNonStream(c, lease.client, prompt, claudeModel, effort, req.ConversationID, lease.accountID)
	}
}

// buildAnthropicPrompt converts Anthropic messages into the single-prompt format
func buildAnthropicPrompt(req models.AnthropicRequest) string {
	var parts []string
	if s := systemToText(req.System); s != "" {
		parts = append(parts, "[System]\n"+s)
	}
	for _, m := range req.Messages {
		text := anthropicContentToString(m.Content)
		switch m.Role {
		case "user":
			parts = append(parts, "[Human]\n"+text)
		case "assistant":
			parts = append(parts, "[Assistant]\n"+text)
		default:
			parts = append(parts, text)
		}
	}
	parts = append(parts, "[Assistant]\n")
	return strings.Join(parts, "\n\n")
}

// systemToText extracts text from a system field that may be a string or an
// array of content blocks (the structured form Claude Code sends).
func systemToText(system interface{}) string {
	if system == nil {
		return ""
	}
	switch v := system.(type) {
	case string:
		return v
	case []interface{}:
		var sb strings.Builder
		for _, item := range v {
			if m, ok := item.(map[string]interface{}); ok {
				if t, _ := m["type"].(string); t == "text" {
					if s, _ := m["text"].(string); s != "" {
						sb.WriteString(s)
					}
				}
			}
		}
		return sb.String()
	default:
		return ""
	}
}

// anthropicContentToString flattens a message content (string or []block) to text
func anthropicContentToString(content interface{}) string {
	switch v := content.(type) {
	case string:
		return v
	case []interface{}:
		var sb strings.Builder
		for _, item := range v {
			if m, ok := item.(map[string]interface{}); ok {
				if t, _ := m["type"].(string); t == "text" {
					if s, _ := m["text"].(string); s != "" {
						sb.WriteString(s)
					}
				}
			}
		}
		return sb.String()
	default:
		return ""
	}
}

func (h *Handler) anthropicNonStream(c *gin.Context, client *claude.Client, prompt, claudeModel, effort, conversationID, accountID string) {
	_, content, err := h.runCompletion(c.Request.Context(), client, prompt, claudeModel, effort, conversationID, accountID, nil, nil)
	if err != nil {
		upstreamError(c, err.Error())
		return
	}
	resp := models.AnthropicResponse{
		ID:         genID("msg_"),
		Type:       "message",
		Role:       "assistant",
		Content:    []models.AnthropicContentBlock{{Type: "text", Text: content}},
		Model:      claudeModel,
		StopReason: "end_turn",
		Usage: models.AnthropicUsage{
			OutputTokens: len(content) / 4,
		},
	}
	c.JSON(http.StatusOK, resp)
}

// cacheUsage computes cache creation/read token counts for a request and
// merges them into the given usage. Pure-text (non-tool) requests have no
// cache_control blocks, so this is effectively a no-op there.
func cacheUsage(accountID, conversationID string, req models.AnthropicRequest, usage models.AnthropicUsage) models.AnthropicUsage {
	creation, read := globalCacheTracker.record(accountID+"|"+conversationID, req)
	usage.CacheCreationInputTokens = creation
	usage.CacheReadInputTokens = read
	// InputTokens is already populated by runToolLoop from real prompt sizes.
	// For pure-text (non-tool) paths, approximate from the request.
	if usage.InputTokens == 0 {
		usage.InputTokens = len(systemToText(req.System))/4 + messagesTokens(req.Messages)
	}
	return usage
}

// messagesTokens estimates total tokens across all messages.
func messagesTokens(messages []models.AnthropicMessage) int {
	total := 0
	for _, m := range messages {
		switch v := m.Content.(type) {
		case string:
			total += len(v) / 4
		case []interface{}:
			for _, item := range v {
				if m, ok := item.(map[string]interface{}); ok {
					if s, _ := m["text"].(string); s != "" {
						total += len(s) / 4
					}
				}
			}
		}
	}
	return total
}

func (h *Handler) anthropicStream(c *gin.Context, client *claude.Client, prompt, claudeModel, effort, conversationID, accountID string) {
	c.Writer.Header().Set("Content-Type", "text/event-stream")
	c.Writer.Header().Set("Cache-Control", "no-cache")
	c.Writer.Header().Set("Connection", "keep-alive")
	c.Writer.Header().Set("X-Accel-Buffering", "no")
	flusher, _ := c.Writer.(http.Flusher)

	msgID := genID("msg_")

	// message_start
	writeSSE(c.Writer, models.AnthropicStreamMessageStart{
		Type: "message_start",
		Message: models.AnthropicStartMsg{
			ID:      msgID,
			Type:    "message",
			Role:    "assistant",
			Content: []models.AnthropicContentBlock{},
			Model:   claudeModel,
			Usage:   models.AnthropicUsage{},
		},
	})

	// content_block_start
	writeSSE(c.Writer, models.AnthropicStreamContentBlockStart{
		Type:         "content_block_start",
		Index:        0,
		ContentBlock: models.AnthropicContentBlock{Type: "text", Text: ""},
	})

	var outputChars int
	_, _, err := h.runCompletion(c.Request.Context(), client, prompt, claudeModel, effort, conversationID, accountID, func(text string) {
		outputChars += len(text)
		writeSSE(c.Writer, models.AnthropicStreamContentBlockDelta{
			Type:  "content_block_delta",
			Index: 0,
			Delta: models.AnthropicStreamDelta{Type: "text_delta", Text: text},
		})
		if flusher != nil {
			flusher.Flush()
		}
	}, nil)

	// content_block_stop
	writeSSE(c.Writer, models.AnthropicStreamContentBlockStop{Type: "content_block_stop", Index: 0})

	stopReason := "end_turn"
	if err != nil {
		stopReason = "error"
	}
	// message_delta
	writeSSE(c.Writer, models.AnthropicStreamMessageDelta{
		Type:  "message_delta",
		Delta: models.AnthropicStopDelta{StopReason: stopReason},
		Usage: models.AnthropicUsage{OutputTokens: outputChars / 4},
	})

	// message_stop
	writeSSE(c.Writer, models.AnthropicStreamMessageStop{Type: "message_stop"})
	if flusher != nil {
		flusher.Flush()
	}
}

// -------- tool-use simulation over text-only claude.ai --------

// anthropicToolNonStream runs a tool-use loop and returns a single Anthropic response.
func (h *Handler) anthropicToolNonStream(c *gin.Context, client *claude.Client, req models.AnthropicRequest, claudeModel, effort, accountID string) {
	blocks, usage, err := h.runToolLoop(c.Request.Context(), client, req, claudeModel, effort, accountID)
	if err != nil {
		upstreamError(c, err.Error())
		return
	}
	usage = cacheUsage(accountID, req.ConversationID, req, usage)
	resp := models.AnthropicResponse{
		ID:         genID("msg_"),
		Type:       "message",
		Role:       "assistant",
		Content:    blocks,
		Model:      claudeModel,
		StopReason: "end_turn",
		Usage:      usage,
	}
	c.JSON(http.StatusOK, resp)
}

// anthropicToolStream runs a tool-use loop, then streams the final text back
// chunk-by-chunk so Claude Code sees a streaming response.
func (h *Handler) anthropicToolStream(c *gin.Context, client *claude.Client, req models.AnthropicRequest, claudeModel, effort, accountID string) {
	blocks, usage, err := h.runToolLoop(c.Request.Context(), client, req, claudeModel, effort, accountID)
	if err != nil {
		upstreamError(c, err.Error())
		return
	}
	usage = cacheUsage(accountID, req.ConversationID, req, usage)

	c.Writer.Header().Set("Content-Type", "text/event-stream")
	c.Writer.Header().Set("Cache-Control", "no-cache")
	c.Writer.Header().Set("Connection", "keep-alive")
	c.Writer.Header().Set("X-Accel-Buffering", "no")
	flusher, _ := c.Writer.(http.Flusher)

	msgID := genID("msg_")

	// message_start
	writeSSE(c.Writer, models.AnthropicStreamMessageStart{
		Type: "message_start",
		Message: models.AnthropicStartMsg{
			ID:      msgID,
			Type:    "message",
			Role:    "assistant",
			Content: []models.AnthropicContentBlock{},
			Model:   claudeModel,
			Usage:   models.AnthropicUsage{},
		},
	})

	// Emit each content block. tool_use and tool_result blocks are sent
	// verbatim; text/thinking blocks are streamed chunk-by-chunk.
	for i, block := range blocks {
		switch block.Type {
		case "tool_use":
			writeSSE(c.Writer, models.AnthropicStreamContentBlockStart{
				Type:         "content_block_start",
				Index:        i,
				ContentBlock: block,
			})
			writeSSE(c.Writer, models.AnthropicStreamContentBlockDelta{
				Type:  "content_block_delta",
				Index: i,
				Delta: models.AnthropicStreamDelta{Type: "input_json_delta", PartialJSON: mustJSON(block.Input)},
			})
			writeSSE(c.Writer, models.AnthropicStreamContentBlockStop{Type: "content_block_stop", Index: i})
		case "tool_result":
			writeSSE(c.Writer, models.AnthropicStreamContentBlockStart{
				Type:         "content_block_start",
				Index:        i,
				ContentBlock: block,
			})
			if content, ok := block.Content.(string); ok && content != "" {
				writeSSE(c.Writer, models.AnthropicStreamContentBlockDelta{
					Type:  "content_block_delta",
					Index: i,
					Delta: models.AnthropicStreamDelta{Type: "text_delta", Text: content},
				})
			}
			writeSSE(c.Writer, models.AnthropicStreamContentBlockStop{Type: "content_block_stop", Index: i})
		default:
			writeSSE(c.Writer, models.AnthropicStreamContentBlockStart{
				Type:         "content_block_start",
				Index:        i,
				ContentBlock: models.AnthropicContentBlock{Type: block.Type, Text: ""},
			})
			if block.Text != "" {
				streamText(c.Writer, block.Text, i, flusher)
			}
			writeSSE(c.Writer, models.AnthropicStreamContentBlockStop{Type: "content_block_stop", Index: i})
		}
		if flusher != nil {
			flusher.Flush()
		}
	}

	stopReason := "end_turn"
	writeSSE(c.Writer, models.AnthropicStreamMessageDelta{
		Type:  "message_delta",
		Delta: models.AnthropicStopDelta{StopReason: stopReason},
		Usage: usage,
	})
	writeSSE(c.Writer, models.AnthropicStreamMessageStop{Type: "message_stop"})
	if flusher != nil {
		flusher.Flush()
	}
}

// streamText sends text in small chunks with a slight delay to mimic streaming.
func streamText(w io.Writer, text string, index int, flusher http.Flusher) {
	chunks := chunkString(text, 12)
	for _, chunk := range chunks {
		writeSSE(w, models.AnthropicStreamContentBlockDelta{
			Type:  "content_block_delta",
			Index: index,
			Delta: models.AnthropicStreamDelta{Type: "text_delta", Text: chunk},
		})
		if flusher != nil {
			flusher.Flush()
		}
	}
}

// chunkString splits s into pieces of at most n chars, without breaking runes.
func chunkString(s string, n int) []string {
	if s == "" {
		return nil
	}
	runes := []rune(s)
	var chunks []string
	for i := 0; i < len(runes); i += n {
		end := i + n
		if end > len(runes) {
			end = len(runes)
		}
		chunks = append(chunks, string(runes[i:end]))
	}
	return chunks
}

// runToolLoop drives the multi-round conversation with claude.ai, injecting tool
// definitions into the prompt and executing any tool calls the model embeds in
// its text response.
func (h *Handler) runToolLoop(ctx context.Context, client *claude.Client, req models.AnthropicRequest, claudeModel, effort, accountID string) ([]models.AnthropicContentBlock, models.AnthropicUsage, error) {
	toolDefText := buildToolDefsPrompt(req.ToolDefs)
	emptyTools := json.RawMessage([]byte("[]"))

	// Per-request thinking overrides proxy config.
	thinkingMode, _ := resolveThinking(h.cfg.Thinking)
	if req.Thinking != nil {
		thinkingMode, _ = resolveThinking(req.Thinking)
	}

	// We keep an internal transcript of the conversation so we can keep feeding
	// claude.ai a single prompt each round.
	messages := cloneMessages(req.Messages)
	system := req.System

	// Collect all content blocks across rounds so the caller can stream them.
	var allBlocks []models.AnthropicContentBlock
	totalInputChars := 0

	for round := 0; round < MaxToolRounds; round++ {
		prompt := buildToolPrompt(system, messages, toolDefText)
		totalInputChars += len(prompt)

		roundThinking, content, err := h.runCompletion(ctx, client, prompt, claudeModel, effort, req.ConversationID, accountID, nil, []json.RawMessage{emptyTools}, thinkingMode)
		if err != nil {
			return allBlocks, models.AnthropicUsage{}, err
		}

		totalInputChars += len(content)
		if roundThinking != "" {
			totalInputChars += len(roundThinking)
			allBlocks = append(allBlocks, models.AnthropicContentBlock{
				Type: "thinking",
				Text: roundThinking,
			})
		}

		text, calls := extractToolCalls(content)

		// Remember the assistant turn (text + embedded tool calls) for transcript.
		assistantBlocks := []models.AnthropicContentBlock{}
		if text != "" {
			allBlocks = append(allBlocks, models.AnthropicContentBlock{Type: "text", Text: text})
			assistantBlocks = append(assistantBlocks, models.AnthropicContentBlock{Type: "text", Text: text})
		}

		if len(calls) == 0 {
			// No tool calls — conversation is done.
			usage := models.AnthropicUsage{
				InputTokens:  totalInputChars / 4,
				OutputTokens: totalOutputChars(allBlocks),
			}
			return allBlocks, usage, nil
		}

		// Execute each tool call and emit tool_use + tool_result blocks so the
		// caller can display the full tool-use trace.
		userResultBlocks := []models.AnthropicContentBlock{}
		for _, call := range calls {
			toolBlock := models.AnthropicContentBlock{
				Type:  "tool_use",
				ID:    call.ID,
				Name:  call.Name,
				Input: call.Input,
			}
			allBlocks = append(allBlocks, toolBlock)
			assistantBlocks = append(assistantBlocks, toolBlock)

			result := runTool(call)
			resultBlock := models.AnthropicContentBlock{
				Type:    "tool_result",
				UseID:   call.ID,
				Content: result,
			}
			allBlocks = append(allBlocks, resultBlock)
			userResultBlocks = append(userResultBlocks, resultBlock)
		}

		// Append assistant + tool_result turns to transcript for the next round.
		messages = append(messages, models.AnthropicMessage{Role: "assistant", Content: blocksToInterface(assistantBlocks)})
		messages = append(messages, models.AnthropicMessage{Role: "user", Content: blocksToInterface(userResultBlocks)})
	}

	// Hit the round cap — return whatever we have.
	usage := models.AnthropicUsage{
		InputTokens:  totalInputChars / 4,
		OutputTokens: totalOutputChars(allBlocks),
	}
	return allBlocks, usage, nil
}

// totalOutputChars estimates total output tokens from content blocks.
func totalOutputChars(blocks []models.AnthropicContentBlock) int {
	total := 0
	for _, b := range blocks {
		switch b.Type {
		case "text", "thinking":
			total += len(b.Text)
		case "tool_use":
			if b.Input != nil {
				total += len(b.Input) * 4
			}
		}
	}
	return total / 4
}

// buildContentBlocks assembles the final content block list, prepending a
// thinking block when the model produced thinking text.
func buildContentBlocks(thinking string, rest []models.AnthropicContentBlock) []models.AnthropicContentBlock {
	if thinking == "" {
		return rest
	}
	block := models.AnthropicContentBlock{
		Type: "thinking",
		Text: thinking,
	}
	return append([]models.AnthropicContentBlock{block}, rest...)
}

// buildToolDefsPrompt renders the tool definitions into a prompt appendix.
func buildToolDefsPrompt(tools []models.AnthropicTool) string {
	if len(tools) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("\n\n[Tools]\n")
	sb.WriteString("You have access to the following tools. When you need to use a tool, emit EXACTLY one block per call using this format, and NOTHING else around the block:\n")
	sb.WriteString("  [TOOL_CALL]{\"name\":\"<tool_name>\",\"id\":\"<unique_id>\",\"input\":{...}}[/TOOL_CALL]\n")
	sb.WriteString("Rules:\n")
	sb.WriteString("- 'id' must be unique per call, e.g. \"toolu_1\", \"toolu_2\".\n")
	sb.WriteString("- 'input' must match the tool's input_schema.\n")
	sb.WriteString("- You may include explanatory text before or after the block.\n")
	sb.WriteString("- If no tool is needed, just respond normally.\n\n")
	sb.WriteString("Available tools (JSON):\n")
	b, _ := json.MarshalIndent(tools, "", "  ")
	sb.Write(b)
	sb.WriteString("\n[/Tools]\n")
	return sb.String()
}

// buildToolPrompt assembles the full prompt for one round.
func buildToolPrompt(system interface{}, messages []models.AnthropicMessage, toolDefText string) string {
	var parts []string
	if s := systemToText(system); s != "" {
		parts = append(parts, "[System]\n"+s)
	}
	for _, m := range messages {
		text := anthropicContentToToolText(m.Content)
		switch m.Role {
		case "user":
			parts = append(parts, "[Human]\n"+text)
		case "assistant":
			parts = append(parts, "[Assistant]\n"+text)
		default:
			parts = append(parts, text)
		}
	}
	parts = append(parts, "[Assistant]\n")
	return strings.Join(parts, "\n\n") + toolDefText
}

// anthropicContentToToolText serializes message content to text, preserving
// tool_use and tool_result blocks so claude.ai can follow the tool loop.
func anthropicContentToToolText(content interface{}) string {
	switch v := content.(type) {
	case string:
		return v
	case []interface{}:
		var sb strings.Builder
		for _, item := range v {
			if m, ok := item.(map[string]interface{}); ok {
				switch m["type"] {
				case "text":
					if s, _ := m["text"].(string); s != "" {
						sb.WriteString(s)
					}
				case "tool_use":
					name, _ := m["name"].(string)
					id, _ := m["id"].(string)
					if name != "" {
						sb.WriteString(fmt.Sprintf("\n[Tool Use: %s (%s)]\n", name, id))
						if input, err := json.Marshal(m["input"]); err == nil {
							sb.Write(input)
						}
					}
				case "tool_result":
					id, _ := m["tool_use_id"].(string)
					if id == "" {
						// Accept the legacy internal field name for compatibility with
						// transcripts created before tool_use_id was standardized.
						id, _ = m["use_id"].(string)
					}
					sb.WriteString(fmt.Sprintf("\n[Tool Result: %s]\n", id))
					switch c := m["content"].(type) {
					case string:
						sb.WriteString(c)
					case []interface{}:
						for _, b := range c {
							if bm, ok := b.(map[string]interface{}); ok {
								if t, _ := bm["type"].(string); t == "text" {
									if s, _ := bm["text"].(string); s != "" {
										sb.WriteString(s)
									}
								}
							}
						}
					}
				}
			}
		}
		return sb.String()
	default:
		return ""
	}
}

func cloneMessages(in []models.AnthropicMessage) []models.AnthropicMessage {
	out := make([]models.AnthropicMessage, len(in))
	copy(out, in)
	return out
}

func blocksToInterface(blocks []models.AnthropicContentBlock) []interface{} {
	out := make([]interface{}, 0, len(blocks))
	for _, b := range blocks {
		out = append(out, b)
	}
	return out
}

func mustJSON(v interface{}) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}
