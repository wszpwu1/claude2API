package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"claude2api/claude"
	"claude2api/models"

	"github.com/gin-gonic/gin"
)

// AnthropicMessages handles POST /v1/messages (Anthropic format, streaming + non-streaming)
func (h *Handler) AnthropicMessages(c *gin.Context) {
	var raw json.RawMessage
	if err := c.ShouldBindJSON(&raw); err != nil {
		badRequest(c, "invalid request body: "+err.Error())
		return
	}

	var req models.AnthropicRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		badRequest(c, "invalid request body: "+err.Error())
		return
	}
	req.Messages = normalizeAnthropicToolResults(req.Messages)
	if err := validateAnthropicToolPairing(req.Messages); err != nil {
		badRequest(c, "invalid tool transcript: "+err.Error())
		return
	}

	if len(req.Messages) == 0 {
		var responsesReq models.ResponsesRequest
		if err := json.Unmarshal(raw, &responsesReq); err == nil && responsesReq.Input != nil {
			req.Model = responsesReq.Model
			req.Stream = responsesReq.Stream
			req.ConversationID = responsesReq.ConversationID
			req.MaxTokens = responsesReq.MaxOutputTokens
			req.Temperature = responsesReq.Temperature
			req.TopP = responsesReq.TopP
			req.System = responsesReq.Instructions
			req.Messages = responseInputToAnthropicMessages(responsesReq.Input)
		}
	}
	if len(req.Messages) == 0 {
		badRequest(c, "messages or input must contain at least one message")
		return
	}
	if req.MaxTokens == 0 {
		req.MaxTokens = 4096
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

// responseInputToAnthropicMessages converts an OpenAI Responses API input into
// Anthropic-compatible messages. OpenAI chat messages already unmarshal directly
// into AnthropicRequest because both formats use the messages field.
func responseInputToAnthropicMessages(input interface{}) []models.AnthropicMessage {
	switch v := input.(type) {
	case string:
		if strings.TrimSpace(v) == "" {
			return nil
		}
		return []models.AnthropicMessage{{Role: "user", Content: v}}
	case []interface{}:
		messages := make([]models.AnthropicMessage, 0, len(v))
		for _, item := range v {
			m, ok := item.(map[string]interface{})
			if !ok {
				continue
			}
			role, _ := m["role"].(string)
			if role == "system" {
				role = "user"
			}
			if role == "" {
				role = "user"
			}
			content, exists := m["content"]
			if !exists {
				continue
			}
			messages = append(messages, models.AnthropicMessage{Role: role, Content: normalizeResponseContent(content)})
		}
		return messages
	default:
		return nil
	}
}

// normalizeAnthropicToolResults converts decoded tool_result maps into the
// canonical Anthropic content-block struct so type and tool_use_id remain
// structured protocol fields instead of being treated as ordinary text.
func normalizeAnthropicToolResults(messages []models.AnthropicMessage) []models.AnthropicMessage {
	for i := range messages {
		parts, ok := messages[i].Content.([]interface{})
		if !ok {
			continue
		}
		for j, part := range parts {
			m, ok := part.(map[string]interface{})
			if !ok || m["type"] != "tool_result" {
				continue
			}
			toolUseID, _ := m["tool_use_id"].(string)
			if toolUseID == "" {
				toolUseID, _ = m["use_id"].(string)
			}
			block := models.AnthropicContentBlock{
				Type:    "tool_result",
				UseID:   toolUseID,
				Content: m["content"],
			}
			if isError, ok := m["is_error"].(bool); ok {
				block.IsError = &isError
			}
			if cacheControl, ok := m["cache_control"]; ok {
				block.CacheControl = cacheControl
			}
			parts[j] = block
		}
		messages[i].Content = parts
	}
	return messages
}

// validateAnthropicToolPairing ensures every tool_result references exactly one
// preceding, unresolved tool_use ID. Results must resolve pending calls in the
// same order in which those calls appeared, preventing missing, duplicated,
// unknown, or shifted IDs from corrupting later tool rounds.
func validateAnthropicToolPairing(messages []models.AnthropicMessage) error {
	pending := make([]string, 0)
	seenUses := make(map[string]struct{})
	resolved := make(map[string]struct{})

	for messageIndex, message := range messages {
		parts, ok := message.Content.([]interface{})
		if !ok {
			continue
		}
		for blockIndex, part := range parts {
			var blockType, id string
			switch block := part.(type) {
			case models.AnthropicContentBlock:
				blockType = block.Type
				if blockType == "tool_use" {
					id = block.ID
				} else if blockType == "tool_result" {
					id = block.UseID
				}
			case map[string]interface{}:
				blockType, _ = block["type"].(string)
				if blockType == "tool_use" {
					id, _ = block["id"].(string)
				} else if blockType == "tool_result" {
					id, _ = block["tool_use_id"].(string)
					if id == "" {
						id, _ = block["use_id"].(string)
					}
				}
			default:
				continue
			}

			switch blockType {
			case "tool_use":
				if id == "" {
					return fmt.Errorf("tool_use at messages[%d].content[%d] has an empty id", messageIndex, blockIndex)
				}
				if _, exists := seenUses[id]; exists {
					return fmt.Errorf("duplicate tool_use id %q at messages[%d].content[%d]", id, messageIndex, blockIndex)
				}
				seenUses[id] = struct{}{}
				pending = append(pending, id)
			case "tool_result":
				if id == "" {
					return fmt.Errorf("tool_result at messages[%d].content[%d] has an empty tool_use_id", messageIndex, blockIndex)
				}
				if _, exists := resolved[id]; exists {
					return fmt.Errorf("duplicate tool_result for tool_use id %q at messages[%d].content[%d]", id, messageIndex, blockIndex)
				}
				if _, exists := seenUses[id]; !exists {
					return fmt.Errorf("tool_result references unknown tool_use id %q at messages[%d].content[%d]", id, messageIndex, blockIndex)
				}
				if len(pending) == 0 || pending[0] != id {
					expected := "none"
					if len(pending) > 0 {
						expected = pending[0]
					}
					return fmt.Errorf("tool_result id %q is out of order at messages[%d].content[%d]; expected %q", id, messageIndex, blockIndex, expected)
				}
				pending = pending[1:]
				resolved[id] = struct{}{}
			}
		}
	}

	return nil
}

func normalizeResponseContent(content interface{}) interface{} {
	parts, ok := content.([]interface{})
	if !ok {
		return content
	}
	out := make([]interface{}, 0, len(parts))
	for _, part := range parts {
		m, ok := part.(map[string]interface{})
		if !ok {
			continue
		}
		typ, _ := m["type"].(string)
		if typ == "input_text" || typ == "output_text" {
			m["type"] = "text"
		}
		out = append(out, m)
	}
	return out
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
// array of content blocks (the structured form Claude Code sends). Block
// boundaries are retained with blank lines so independently cached system
// sections cannot be accidentally concatenated into different instructions.
// cache_control is transport metadata and therefore does not alter the text.
func systemToText(system interface{}) string {
	if system == nil {
		return ""
	}
	switch v := system.(type) {
	case string:
		return v
	case []interface{}:
		parts := make([]string, 0, len(v))
		for _, item := range v {
			if m, ok := item.(map[string]interface{}); ok {
				if t, _ := m["type"].(string); t == "text" {
					if s, _ := m["text"].(string); s != "" {
						parts = append(parts, s)
					}
				}
			}
		}
		return strings.Join(parts, "\n\n")
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
	inputTokens := len([]rune(prompt)) / 4
	outputTokens := len([]rune(content)) / 4
	c.Set("inputTokens", int64(inputTokens))
	c.Set("outputTokens", int64(outputTokens))
	resp := models.AnthropicResponse{
		ID:         genID("msg_"),
		Type:       "message",
		Role:       "assistant",
		Content:    []models.AnthropicContentBlock{{Type: "text", Text: content}},
		Model:      claudeModel,
		StopReason: "end_turn",
		Usage: models.AnthropicUsage{
			InputTokens:  inputTokens,
			OutputTokens: outputTokens,
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
		// Use rune count for multi-byte text accuracy.
		usage.InputTokens = len([]rune(systemToText(req.System)))/4 + messagesTokens(req.Messages)
	}
	return usage
}

// messagesTokens estimates total tokens across all messages.
// Uses rune count for more accurate estimation of multi-byte text (e.g. CJK).
func messagesTokens(messages []models.AnthropicMessage) int {
	total := 0
	for _, m := range messages {
		switch v := m.Content.(type) {
		case string:
			total += len([]rune(v)) / 4
		case []interface{}:
			for _, item := range v {
				if block, ok := item.(map[string]interface{}); ok {
					if s, _ := block["text"].(string); s != "" {
						total += len([]rune(s)) / 4
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
	c.Set("inputTokens", int64(len([]rune(prompt))/4))
	c.Set("outputTokens", int64(outputChars/4))

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
	c.Set("inputTokens", int64(usage.InputTokens))
	c.Set("outputTokens", int64(usage.OutputTokens))
	stopReason := "end_turn"
	if containsToolUse(blocks) {
		stopReason = "tool_use"
	}
	resp := models.AnthropicResponse{
		ID:         genID("msg_"),
		Type:       "message",
		Role:       "assistant",
		Content:    blocks,
		Model:      claudeModel,
		StopReason: stopReason,
		Usage:      usage,
	}
	c.JSON(http.StatusOK, resp)
}

// anthropicToolStream runs a tool-use loop, then streams the result back
// as a well-formed Anthropic SSE response. Because runToolLoop already has
// the complete output in memory, each block is emitted as a single delta
// rather than being chopped into small artificial chunks — this eliminates
// hundreds of redundant write/flush calls and reduces response latency.
func (h *Handler) anthropicToolStream(c *gin.Context, client *claude.Client, req models.AnthropicRequest, claudeModel, effort, accountID string) {
	blocks, usage, err := h.runToolLoop(c.Request.Context(), client, req, claudeModel, effort, accountID)
	if err != nil {
		upstreamError(c, err.Error())
		return
	}
	usage = cacheUsage(accountID, req.ConversationID, req, usage)
	c.Set("inputTokens", int64(usage.InputTokens))
	c.Set("outputTokens", int64(usage.OutputTokens))

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

	// Emit each content block as a single SSE sequence.
	// tool_use/tool_result carry structured data; text/thinking carry plain text.
	// All data is already in memory so there is no benefit in sub-chunking.
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
			// text / thinking blocks: send the full text in a single delta.
			writeSSE(c.Writer, models.AnthropicStreamContentBlockStart{
				Type:         "content_block_start",
				Index:        i,
				ContentBlock: models.AnthropicContentBlock{Type: block.Type, Text: ""},
			})
			if block.Text != "" {
				writeSSE(c.Writer, models.AnthropicStreamContentBlockDelta{
					Type:  "content_block_delta",
					Index: i,
					Delta: models.AnthropicStreamDelta{Type: "text_delta", Text: block.Text},
				})
			}
			writeSSE(c.Writer, models.AnthropicStreamContentBlockStop{Type: "content_block_stop", Index: i})
		}
		if flusher != nil {
			flusher.Flush()
		}
	}

	stopReason := "end_turn"
	if containsToolUse(blocks) {
		stopReason = "tool_use"
	}
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

// runToolLoop sends a tool-enabled prompt to claude.ai, parses any
// [TOOL_CALL] blocks the model emits, and returns the assembled content
// blocks together with usage estimates. Tool execution is intentionally
// delegated back to the caller (e.g. RooCode) via tool_use blocks so it
// can run tools in its own workspace rather than the proxy's filesystem.
//
// If the model fails to emit any tool calls on the first round and the
// request's tool_choice requires tool use, one retry is attempted with an
// explicit nudge so the model does not silently fall back to plain text.
func (h *Handler) runToolLoop(ctx context.Context, client *claude.Client, req models.AnthropicRequest, claudeModel, effort, accountID string) ([]models.AnthropicContentBlock, models.AnthropicUsage, error) {
	toolDefText := buildToolDefsPrompt(req.ToolDefs, req.ToolChoice)
	emptyTools := json.RawMessage([]byte("[]"))

	// Per-request thinking overrides proxy config.
	thinkingMode, _ := resolveThinking(h.cfg.Thinking)
	if req.Thinking != nil {
		thinkingMode, _ = resolveThinking(req.Thinking)
	}

	prompt := buildToolPrompt(req.System, req.Messages, toolDefText)

	roundThinking, content, err := h.runCompletion(ctx, client, prompt, claudeModel, effort, req.ConversationID, accountID, nil, []json.RawMessage{emptyTools}, thinkingMode)
	if err != nil {
		return nil, models.AnthropicUsage{}, err
	}

	totalInputChars := len(prompt) + len(content)

	var allBlocks []models.AnthropicContentBlock
	if roundThinking != "" {
		totalInputChars += len(roundThinking)
		allBlocks = append(allBlocks, models.AnthropicContentBlock{Type: "thinking", Text: roundThinking})
	}

	text, calls := extractToolCalls(content)

	// If the model returned no tool calls but tool use is required, do one
	// retry with an explicit nudge before giving up and returning plain text.
	// This handles cases where the model answers in prose instead of calling
	// a tool, or where it cannot locate a file and needs prompting to use Glob.
	if len(calls) == 0 && toolChoiceRequiresTools(req.ToolChoice) {
		nudge := buildToolNudge(content, req.ToolDefs)
		retryPrompt := prompt + "\n\n[Assistant]\n" + content + "\n\n" + nudge
		_, retryContent, retryErr := h.runCompletion(ctx, client, retryPrompt, claudeModel, effort, req.ConversationID, accountID, nil, []json.RawMessage{emptyTools}, thinkingMode)
		if retryErr == nil && retryContent != "" {
			totalInputChars += len(retryPrompt) + len(retryContent)
			// Replace the original plain-text response with the retry output.
			text, calls = extractToolCalls(retryContent)
			if len(calls) > 0 {
				// Retry produced tool calls — discard earlier thinking/text.
				allBlocks = nil
			} else {
				// Still no calls: keep retry text so the caller gets something.
				text = retryContent
			}
		}
	}

	if text != "" {
		allBlocks = append(allBlocks, models.AnthropicContentBlock{Type: "text", Text: text})
	}
	for _, call := range calls {
		allBlocks = append(allBlocks, models.AnthropicContentBlock{
			Type:  "tool_use",
			ID:    call.ID,
			Name:  call.Name,
			Input: call.Input,
		})
	}

	usage := models.AnthropicUsage{
		InputTokens:  totalInputChars / 4,
		OutputTokens: totalOutputTokens(allBlocks),
	}
	return allBlocks, usage, nil
}

// toolChoiceRequiresTools reports whether the caller's tool_choice mandates
// at least one tool call (i.e. "any" or a specific named tool).
func toolChoiceRequiresTools(toolChoice interface{}) bool {
	m, ok := toolChoice.(map[string]interface{})
	if !ok {
		return false
	}
	typ, _ := m["type"].(string)
	return typ == "any" || typ == "tool"
}

// buildToolNudge builds the human-turn nudge message that is appended when
// the model fails to emit any tool calls on the first round.
// It reminds the model about available tools and, if file-access tools are
// present, explicitly tells it to use Glob before guessing a path.
func buildToolNudge(priorText string, tools []models.AnthropicTool) string {
	// Collect tool names so the nudge can list them.
	names := make([]string, 0, len(tools))
	hasFileTools := false
	for _, t := range tools {
		names = append(names, t.Name)
		if t.Name == "Glob" || t.Name == "Read" || t.Name == "Bash" {
			hasFileTools = true
		}
	}

	var sb strings.Builder
	sb.WriteString("[Human]\n")
	sb.WriteString("You responded with plain text but you must call a tool.\n")
	sb.WriteString("Available tools: ")
	sb.WriteString(strings.Join(names, ", "))
	sb.WriteString(".\n")

	if hasFileTools {
		sb.WriteString("If you cannot find a file, call Glob first:\n")
		sb.WriteString("  [TOOL_CALL]{\"name\":\"Glob\",\"id\":\"toolu_retry_1\",\"input\":{\"pattern\":\"**/*\",\"path\":\".\"}}[/TOOL_CALL]\n")
		sb.WriteString("Then use Read with the exact path Glob returns.\n")
	}

	sb.WriteString("Emit a [TOOL_CALL]...[/TOOL_CALL] block now. Do NOT answer in prose.")
	// Suppress the prior prose so the model doesn't repeat it.
	_ = priorText
	return sb.String()
}

func containsToolUse(blocks []models.AnthropicContentBlock) bool {
	for _, block := range blocks {
		if block.Type == "tool_use" {
			return true
		}
	}
	return false
}

// totalOutputTokens estimates total output tokens from content blocks.
func totalOutputTokens(blocks []models.AnthropicContentBlock) int {
	total := 0
	for _, b := range blocks {
		switch b.Type {
		case "text", "thinking":
			// Use rune count for accurate multi-byte (e.g. CJK) token estimation.
			total += len([]rune(b.Text))
		case "tool_use":
			// Marshal to JSON to get the actual byte footprint, not the map entry count.
			if b.Input != nil {
				if raw, err := json.Marshal(b.Input); err == nil {
					total += len(raw)
				}
			}
		}
	}
	return total / 4
}

// buildToolDefsPrompt renders the tool definitions into a prompt appendix.
// toolChoice follows the Anthropic format: nil / {"type":"auto"} / {"type":"none"} /
// {"type":"any"} / {"type":"tool","name":"<name>"}.
func buildToolDefsPrompt(tools []models.AnthropicTool, toolChoice interface{}) string {
	if len(tools) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("[Tools]\n")
	sb.WriteString("You have access to the following tools. When you need to use a tool, emit EXACTLY one block per call using this format:\n")
	sb.WriteString("  [TOOL_CALL]{\"name\":\"<tool_name>\",\"id\":\"<unique_id>\",\"input\":{...}}[/TOOL_CALL]\n")
	sb.WriteString("CRITICAL FORMAT RULES — failure to follow these will break the system:\n")
	sb.WriteString("- Do NOT wrap the block in markdown code fences (no ```json ... ```).\n")
	sb.WriteString("- Do NOT pretty-print or indent the JSON inside the block — keep it on ONE line.\n")
	sb.WriteString("- Do NOT add any text between [TOOL_CALL] and the opening { or between } and [/TOOL_CALL].\n")
	sb.WriteString("- 'id' must be unique per call, e.g. \"toolu_1\", \"toolu_2\".\n")
	sb.WriteString("- 'input' must match the tool's input_schema exactly.\n")
	sb.WriteString("- You may include explanatory text BEFORE or AFTER the block, but never inside it.\n")

	// Inject tool_choice constraint so the model honours the caller's selection policy.
	choiceType := ""
	choiceName := ""
	if m, ok := toolChoice.(map[string]interface{}); ok {
		choiceType, _ = m["type"].(string)
		choiceName, _ = m["name"].(string)
	}
	switch choiceType {
	case "none":
		// Model must NOT call any tool — respond as plain text.
		sb.WriteString("- IMPORTANT: Do NOT use any tool in this response. Respond using plain text only.\n")
	case "any":
		// Model MUST call at least one tool.
		sb.WriteString("- IMPORTANT: You MUST use at least one tool in your response. Do not reply with plain text only.\n")
	case "tool":
		// Model MUST call the specific named tool.
		if choiceName != "" {
			sb.WriteString(fmt.Sprintf("- IMPORTANT: You MUST use the tool named %q in your response. Do not call any other tool or reply with plain text only.\n", choiceName))
		} else {
			sb.WriteString("- IMPORTANT: You MUST use at least one tool in your response.\n")
		}
	default:
		// "auto" or unset: model decides.
		sb.WriteString("- If no tool is needed, just respond normally.\n")
	}

	sb.WriteString("\nFILE ACCESS RULES — always follow these when using Read or Glob:\n")
	sb.WriteString("- NEVER guess or invent a file path. If a path is uncertain, call Glob first.\n")
	sb.WriteString("- If Read returns \"file not found\", call Glob with pattern=\"**/<filename>\" to locate the real path before retrying.\n")
	sb.WriteString("- After Glob returns results, call Read with the exact path from those results.\n")
	sb.WriteString("- Do NOT fabricate paths like \"/project/src/foo.go\" unless Glob confirmed them.\n")
	sb.WriteString("\nAvailable tools (JSON):\n")
	b, _ := json.MarshalIndent(tools, "", "  ")
	sb.Write(b)
	sb.WriteString("\n[/Tools]")
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
	if toolDefText != "" {
		parts = append(parts, toolDefText)
	}
	parts = append(parts, "[Assistant]\n")
	return strings.Join(parts, "\n\n")
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
			switch block := item.(type) {
			case models.AnthropicContentBlock:
				switch block.Type {
				case "text":
					sb.WriteString(block.Text)
				case "tool_use":
					if block.Name != "" {
						sb.WriteString(fmt.Sprintf("\n[Tool Use: %s (%s)]\n", block.Name, block.ID))
						if input, err := json.Marshal(block.Input); err == nil {
							sb.Write(input)
						}
					}
				case "tool_result":
					sb.WriteString(fmt.Sprintf("\n[Tool Result: %s]\n", block.UseID))
					appendToolResultContent(&sb, block.Content)
				}
			case map[string]interface{}:
				m := block
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
					appendToolResultContent(&sb, m["content"])
				}
			}
		}
		return sb.String()
	default:
		return ""
	}
}

func appendToolResultContent(sb *strings.Builder, content interface{}) {
	switch c := content.(type) {
	case string:
		sb.WriteString(c)
	case []interface{}:
		for _, b := range c {
			if bm, ok := b.(map[string]interface{}); ok {
				if typ, _ := bm["type"].(string); typ == "text" {
					if text, _ := bm["text"].(string); text != "" {
						sb.WriteString(text)
					}
				}
			}
		}
	}
}

func mustJSON(v interface{}) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}
