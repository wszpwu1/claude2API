package handlers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

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
		var pendingToolResults []interface{}
		flushPendingToolResults := func() {
			if len(pendingToolResults) == 0 {
				return
			}
			messages = append(messages, models.AnthropicMessage{Role: "user", Content: pendingToolResults})
			pendingToolResults = nil
		}

		for i := 0; i < len(v); i++ {
			m, ok := v[i].(map[string]interface{})
			if !ok {
				continue
			}
			typ, _ := m["type"].(string)
			if isResponsesToolOutputType(typ) {
				callID, output, ok := extractResponsesToolOutput(m)
				if ok {
					outputLen := contentSize(output)
					log.Printf("tool loop input: round=input_normalize item=%s call_id=%q output_bytes=%d", typ, callID, outputLen)
					pendingToolResults = append(pendingToolResults, map[string]interface{}{
						"type":        "tool_result",
						"tool_use_id": callID,
						"content":     normalizeToolResultContent(output),
					})
				}
				continue
			}
			// Ignore non-message metadata items without breaking pending tool-result aggregation.
			if typ == "reasoning" {
				continue
			}

			// Any non-tool-result item marks a turn boundary: flush pending results first.
			flushPendingToolResults()

			if typ == "function_call" {
				callID, _ := m["call_id"].(string)
				if callID == "" {
					callID, _ = m["id"].(string)
				}
				name, _ := m["name"].(string)
				arguments := m["arguments"]
				if function, ok := m["function"].(map[string]interface{}); ok {
					if name == "" {
						name, _ = function["name"].(string)
					}
					if callID == "" {
						callID, _ = function["call_id"].(string)
					}
					if arguments == nil {
						arguments = function["arguments"]
					}
				}
				if name != "" && callID != "" {
					messages = append(messages, models.AnthropicMessage{
						Role: "assistant",
						Content: []interface{}{map[string]interface{}{
							"type":  "tool_use",
							"id":    callID,
							"name":  name,
							"input": parseFunctionCallArguments(arguments),
						}},
					})
					continue
				}
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
		flushPendingToolResults()
		return messages
	default:
		return nil
	}
}

func normalizeToolResultContent(output interface{}) interface{} {
	switch v := output.(type) {
	case nil:
		return ""
	case string:
		return v
	case []interface{}:
		return v
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return fmt.Sprintf("%v", v)
		}
		return string(b)
	}
}

func parseFunctionCallArguments(raw interface{}) map[string]interface{} {
	switch v := raw.(type) {
	case map[string]interface{}:
		return v
	case string:
		if strings.TrimSpace(v) == "" {
			return map[string]interface{}{}
		}
		var parsed map[string]interface{}
		if err := json.Unmarshal([]byte(v), &parsed); err == nil {
			return parsed
		}
		return map[string]interface{}{"_raw_arguments": v}
	default:
		return map[string]interface{}{}
	}
}

func isResponsesToolOutputType(typ string) bool {
	switch typ {
	case "function_call_output", "custom_tool_call_output", "computer_call_output":
		return true
	default:
		return false
	}
}

func extractResponsesToolOutput(item map[string]interface{}) (string, interface{}, bool) {
	callID, _ := item["call_id"].(string)
	if callID == "" {
		callID, _ = item["tool_call_id"].(string)
	}
	if callID == "" {
		callID, _ = item["id"].(string)
	}
	if callID == "" {
		return "", nil, false
	}
	if output, exists := item["output"]; exists {
		return callID, output, true
	}
	if content, exists := item["content"]; exists {
		return callID, content, true
	}
	return callID, "", true
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
	_, content, err := h.runCompletion(c.Request.Context(), client, prompt, claudeModel, effort, conversationID, accountID, nil, []json.RawMessage{})
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

	var outputRunes int
	_, _, err := h.runCompletion(c.Request.Context(), client, prompt, claudeModel, effort, conversationID, accountID, func(text string) {
		// Count runes for accurate multi-byte (CJK) token estimation, consistent
		// with all other token counting paths in the proxy.
		outputRunes += len([]rune(text))
		writeSSE(c.Writer, models.AnthropicStreamContentBlockDelta{
			Type:  "content_block_delta",
			Index: 0,
			Delta: models.AnthropicStreamDelta{Type: "text_delta", Text: text},
		})
		if flusher != nil {
			flusher.Flush()
		}
	}, []json.RawMessage{})
	c.Set("inputTokens", int64(len([]rune(prompt))/4))
	c.Set("outputTokens", int64(outputRunes/4))

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
		Usage: models.AnthropicUsage{OutputTokens: outputRunes / 4},
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
			// content_block_start for tool_use MUST carry an empty Input per Anthropic's
			// streaming spec. The full input arrives via the subsequent input_json_delta.
			// Sending the full input here AND again in the delta causes clients to see
			// duplicated / malformed arguments.
			writeSSE(c.Writer, models.AnthropicStreamContentBlockStart{
				Type:  "content_block_start",
				Index: i,
				ContentBlock: models.AnthropicContentBlock{
					Type: "tool_use",
					ID:   block.ID,
					Name: block.Name,
					// Input intentionally omitted; delivered via input_json_delta below.
				},
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
// The loop enforces three graduated interventions:
//  1. If the model outputs switch_mode without first calling a file tool on a
//     file-operation task, the switch_mode directive is stripped.
//  2. If tool_choice requires tools (or it is a file task) and no calls were
//     emitted, a standard nudge retry is attempted.
//  3. If the file task still has no file-tool calls after the nudge retry, a
//     hardcoded system warning is injected to force file-tool use.
func (h *Handler) runToolLoop(ctx context.Context, client *claude.Client, req models.AnthropicRequest, claudeModel, effort, accountID string) ([]models.AnthropicContentBlock, models.AnthropicUsage, error) {
	toolDefText := buildToolDefsPrompt(req.ToolDefs, req.ToolChoice)
	emptyTools := json.RawMessage([]byte("[]"))

	// Per-request thinking overrides proxy config.
	thinkingMode, _ := resolveThinking(h.cfg.Thinking)
	if req.Thinking != nil {
		thinkingMode, _ = resolveThinking(req.Thinking)
	}

	// Detect whether this turn explicitly asks for file/code operations so we
	// can apply stricter tool-use discipline and prevent switch_mode escapes.
	// Clients like Roo Code always send a full tool list, even for plain chat.
	// Treating tool availability alone as a file-task signal causes unnecessary
	// retry rounds and multi-minute latency on simple greetings.
	fileTask := isFileOperationTask(req.Messages)

	prompt := buildToolPrompt(req.System, req.Messages, toolDefText)

	roundThinking, content, err := h.runCompletion(ctx, client, prompt, claudeModel, effort, req.ConversationID, accountID, nil, []json.RawMessage{emptyTools}, thinkingMode)
	if err != nil {
		return nil, models.AnthropicUsage{}, err
	}

	// Use rune count throughout for accurate multi-byte (CJK) token estimation,
	// consistent with all other token-counting paths in the proxy.
	totalInputRunes := len([]rune(prompt)) + len([]rune(content))

	// Guard 1: intercept switch_mode without any file tool call.
	// If the model tried to escape via switch_mode instead of searching for
	// the file, strip the directive so it cannot propagate to the client.
	if fileTask && hasSwitchModePattern(content) {
		_, firstCalls := extractToolCalls(content)
		if !containsFileToolCall(firstCalls) {
			content = stripSwitchModePattern(content)
		}
	}

	var allBlocks []models.AnthropicContentBlock
	if roundThinking != "" {
		totalInputRunes += len([]rune(roundThinking))
		allBlocks = append(allBlocks, models.AnthropicContentBlock{Type: "thinking", Text: roundThinking})
	}

	text, calls := extractToolCalls(content)
	// 禁用工具调用重写 - 保持透明性
	// calls = rewriteInitialReadCalls(req.Messages, req.ToolDefs, calls)
	logToolCallRound("initial", text, calls)

	// Guard 2: nudge retry.
	// Trigger when tool_choice requires a tool call, or when this is a file
	// task and the model returned no calls at all (prose answer or switch_mode).
	if (len(calls) == 0 && toolChoiceRequiresTools(req.ToolChoice)) ||
		(fileTask && len(calls) == 0) {
		nudge := buildToolNudge(text, req.ToolDefs)
		// Use `text` (markers stripped) rather than `content` (raw) so the model
		// does not see broken or partial [TOOL_CALL] fragments in its prior
		// response, which could cause it to repeat the same malformed format.
		retryPrompt := prompt + "\n\n[Assistant]\n" + text + "\n\n" + nudge
		_, retryContent, retryErr := h.runCompletion(ctx, client, retryPrompt, claudeModel, effort, req.ConversationID, accountID, nil, []json.RawMessage{emptyTools}, thinkingMode)
		if retryErr == nil && retryContent != "" {
			totalInputRunes += len([]rune(retryPrompt)) + len([]rune(retryContent))
			retryText, retryCalls := extractToolCalls(retryContent)
			// 禁用工具调用重写 - 保持透明性
			// retryCalls = rewriteInitialReadCalls(req.Messages, req.ToolDefs, retryCalls)
			logToolCallRound("nudge_retry", retryText, retryCalls)
			if len(retryCalls) > 0 {
				// Retry produced tool calls — discard earlier thinking/text.
				allBlocks = nil
				text, calls = retryText, retryCalls
			} else {
				// Still no calls: use retryText (markers stripped) rather than
				// retryContent (raw response that may contain unparsed TOOL_CALL
				// fragments) so clients never see internal marker syntax.
				text = retryText
			}
		}
	}

	// Guard 3: system-warning retry (file tasks only).
	// Two consecutive rounds without any file tool calls → inject the
	// hardcoded correction prompt to force immediate file-tool use.
	if fileTask && len(calls) == 0 {
		// Use `text` (markers stripped) here as well — including raw `content`
		// with broken TOOL_CALL fragments confuses the model into retrying the
		// same malformed syntax instead of switching to a correct format.
		warningPrompt := prompt + "\n\n[Assistant]\n" + text + "\n\n" + buildFileTaskWarning()
		_, warnContent, warnErr := h.runCompletion(ctx, client, warningPrompt, claudeModel, effort, req.ConversationID, accountID, nil, []json.RawMessage{emptyTools}, thinkingMode)
		if warnErr == nil && warnContent != "" {
			totalInputRunes += len([]rune(warningPrompt)) + len([]rune(warnContent))
			warnText, warnCalls := extractToolCalls(warnContent)
			// 禁用工具调用重写 - 保持透明性
			// warnCalls = rewriteInitialReadCalls(req.Messages, req.ToolDefs, warnCalls)
			logToolCallRound("warning_retry", warnText, warnCalls)
			if len(warnCalls) > 0 {
				allBlocks = nil
				text, calls = warnText, warnCalls
			} else {
				// Use warnText (markers stripped) rather than warnContent (raw
				// response that may contain unparsed TOOL_CALL fragments) so
				// clients never see internal marker syntax — same discipline as
				// Guard 2's retryText fallback.
				text = warnText
			}
		}
	}

	// Only include the text block when there are no tool calls.
	// When the model emits both text and tool calls, the text is typically
	// internal reasoning ("I'll use X to…") that clients don't need and that
	// can confuse tool-use parsers (e.g. Roo Code) expecting pure tool_use blocks.
	if text != "" && len(calls) == 0 {
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
		InputTokens:  totalInputRunes / 4,
		OutputTokens: totalOutputTokens(allBlocks),
	}
	return allBlocks, usage, nil
}

// switchModeRe matches text patterns where the model attempts to invoke a mode
// switch instead of using the required file tools.
var switchModeRe = regexp.MustCompile(`(?i)switch_mode|<switch_mode>|"switch_mode"|switchMode`)

// fileToolNames is the set of tool names that indicate file/code access.
// When any of these tools is present in the request, stricter retry discipline
// is activated unconditionally, without relying on keyword matching.
// Includes both Claude Code native names and Roo Code / OpenAI-style snake_case names.
var fileToolNames = map[string]struct{}{
	// Claude Code native tool names
	"Glob": {}, "Read": {}, "Write": {}, "Edit": {}, "Bash": {}, "Grep": {},
	// Roo Code / snake_case variants
	"read_file": {}, "write_to_file": {}, "create_file": {}, "delete_file": {},
	"list_files": {}, "search_files": {}, "apply_diff": {}, "execute_command": {},
	"str_replace_editor": {}, "str_replace_based_edit_tool": {},
	"new_file": {}, "insert_content": {},
}

// fileTaskWordRe matches file/code operation keywords at word boundaries so that
// general conversation does not trigger the stricter tool-discipline retry logic.
// Intentionally narrow: only match phrases that unambiguously refer to file/path
// operations. Generic terms like "function", "class", "import" are excluded
// because they appear in normal conversation ("what does this function do?") and
// would cause every code-question to be misclassified as a file-operation task,
// triggering unnecessary multi-round retries.
var fileTaskWordRe = regexp.MustCompile(
	`(?i)\b(file|path|directory|folder|glob|grep|` +
		`source\s+file|source\s+code|` +
		`modify\s+file|modify\s+code|` +
		`create\s+file|delete\s+file|move\s+file|rename\s+file|` +
		`read\s+file|write\s+file|open\s+file|edit\s+file)\b`)

// chineseFileTaskRe matches Chinese file operation phrases.
var chineseFileTaskRe = regexp.MustCompile(
	`(文件|路径|搜索文件|查找文件|编辑文件|读取文件|写入文件|修改文件|目录|创建文件|删除文件|重命名文件|移动文件)`)

// isFileOperationTask reports whether the latest user turn explicitly requests
// file operations.
//
// Keyword matching on the most recent user message avoids over-classifying
// pure chat turns as file tasks when the client always sends full tool defs.
func isFileOperationTask(messages []models.AnthropicMessage) bool {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role != "user" {
			continue
		}
		text := anthropicContentToString(messages[i].Content)
		if fileTaskWordRe.MatchString(text) || chineseFileTaskRe.MatchString(text) {
			return true
		}
		break
	}
	return false
}

// hasSwitchModePattern reports whether the model response text contains an
// attempt to switch modes (switch_mode / switchMode variants).
func hasSwitchModePattern(text string) bool {
	return switchModeRe.MatchString(text)
}

// stripSwitchModePattern removes lines that contain a switch_mode invocation
// from the model response so the directive is not forwarded to the client.
func stripSwitchModePattern(text string) string {
	lines := strings.Split(text, "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		if !switchModeRe.MatchString(line) {
			out = append(out, line)
		}
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

// buildFileTaskWarning returns the hardcoded system-warning turn that is
// injected as a [Human] message when the model refuses to call file tools
// after two consecutive rounds on a file-operation task.
func buildFileTaskWarning() string {
	return "[Human]\n[System Warning: 严禁在未通过 Glob 或 Grep 找到真实文件路径前直接得出结论或切换模式。你必须立即输出工具调用来搜索目标文件。]"
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
// It reminds the model about available tools and provides a concrete live
// example using the first available tool so the model can copy the exact syntax.
func buildToolNudge(priorText string, tools []models.AnthropicTool) string {
	// Collect tool names so the nudge can list them.
	names := make([]string, 0, len(tools))
	hasFileTools := false
	var globTool, listTool, firstTool *models.AnthropicTool
	for i := range tools {
		names = append(names, tools[i].Name)
		if _, ok := fileToolNames[tools[i].Name]; ok {
			hasFileTools = true
		}
		// Prefer Glob; fall back to list_files for Roo Code environments.
		if tools[i].Name == "Glob" && globTool == nil {
			globTool = &tools[i]
		}
		if tools[i].Name == "list_files" && listTool == nil {
			listTool = &tools[i]
		}
		if firstTool == nil {
			firstTool = &tools[i]
		}
	}

	nudgeID := fmt.Sprintf("toolu_retry_%d", time.Now().UnixNano())

	var sb strings.Builder
	sb.WriteString("[Human]\n")
	sb.WriteString("Your previous response contained no [TOOL_CALL] block. You MUST call a tool.\n")
	sb.WriteString("Available tools: ")
	sb.WriteString(strings.Join(names, ", "))
	sb.WriteString(".\n\n")
	sb.WriteString("FORMAT REMINDER — emit exactly this pattern (one block per call, no code fences):\n")

	// Show a concrete example with real tool name and real input keys.
	if hasFileTools && globTool != nil {
		sb.WriteString(fmt.Sprintf("  [TOOL_CALL]{\"name\":\"Glob\",\"id\":%q,\"input\":{\"pattern\":\"**/*\",\"path\":\".\"}}[/TOOL_CALL]\n", nudgeID))
		sb.WriteString("Then use Read with the exact path Glob returns.\n")
	} else if hasFileTools && listTool != nil {
		sb.WriteString(fmt.Sprintf("  [TOOL_CALL]{\"name\":\"list_files\",\"id\":%q,\"input\":{\"path\":\".\",\"recursive\":false}}[/TOOL_CALL]\n", nudgeID))
		sb.WriteString("Then use read_file with the exact path list_files returns.\n")
	} else if firstTool != nil {
		exInput := buildExampleInput(*firstTool)
		sb.WriteString(fmt.Sprintf("  [TOOL_CALL]{\"name\":%q,\"id\":%q,\"input\":%s}[/TOOL_CALL]\n", firstTool.Name, nudgeID, exInput))
	} else {
		sb.WriteString(fmt.Sprintf("  [TOOL_CALL]{\"name\":\"<tool_name>\",\"id\":%q,\"input\":{}}[/TOOL_CALL]\n", nudgeID))
	}

	sb.WriteString("\nOutput ONLY the [TOOL_CALL] block(s). Do NOT answer in prose.")
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

func rewriteInitialReadCalls(messages []models.AnthropicMessage, tools []models.AnthropicTool, calls []toolCall) []toolCall {
	if len(calls) == 0 || hasAnyToolResults(messages) {
		return calls
	}
	out := make([]toolCall, len(calls))
	copy(out, calls)
	mapped := 0
	discovered := 0
	for i := range out {
		if !isReadToolName(out[i].Name) {
			continue
		}
		filePath := toolCallFilePath(out[i].Input)
		if mappedPath, ok := legacyRepoPathMapping(filePath); ok {
			out[i].Input = replaceToolCallFilePath(out[i].Input, mappedPath)
			mapped++
			continue
		}
		discoveryTool := fileDiscoveryToolName(tools)
		if discoveryTool == "" {
			continue
		}
		base := toolPathBase(filePath)
		if base == "" {
			continue
		}
		out[i].Name = discoveryTool
		out[i].Input = fileDiscoveryInput(discoveryTool, base)
		discovered++
	}
	if mapped > 0 {
		log.Printf("remapped %d initial read tool call(s) from legacy claudeapi paths", mapped)
	}
	if discovered > 0 {
		log.Printf("rewrote %d initial read tool call(s) into discovery call(s)", discovered)
	}
	return out
}

func hasAnyToolResults(messages []models.AnthropicMessage) bool {
	for _, message := range messages {
		parts, ok := message.Content.([]interface{})
		if !ok {
			continue
		}
		for _, part := range parts {
			block, ok := part.(map[string]interface{})
			if !ok {
				continue
			}
			if typ, _ := block["type"].(string); typ == "tool_result" {
				return true
			}
		}
	}
	return false
}

func fileDiscoveryToolName(tools []models.AnthropicTool) string {
	for _, tool := range tools {
		if tool.Name == "Glob" {
			return tool.Name
		}
	}
	for _, tool := range tools {
		if tool.Name == "list_files" {
			return tool.Name
		}
	}
	return ""
}

func isReadToolName(name string) bool {
	return name == "Read" || name == "read_file"
}

func toolCallFilePath(input map[string]interface{}) string {
	if input == nil {
		return ""
	}
	if filePath, _ := input["file_path"].(string); strings.TrimSpace(filePath) != "" {
		return filePath
	}
	if filePath, _ := input["path"].(string); strings.TrimSpace(filePath) != "" {
		return filePath
	}
	return ""
}

func toolPathBase(filePath string) string {
	filePath = strings.TrimSpace(filePath)
	if filePath == "" {
		return ""
	}
	filePath = strings.ReplaceAll(filePath, "\\", "/")
	filePath = strings.TrimRight(filePath, "/")
	base := path.Base(filePath)
	if base == "." || base == "/" {
		return ""
	}
	return base
}

var legacyRepoPathAliases = map[string]string{
	"claudeapi/internal/proxy/claude.go":    "claude/client.go",
	"claudeapi/internal/proxy/sse.go":       "utils/sse.go",
	"claudeapi/internal/handlers/models.go": "handlers/common.go",
	"internal/proxy/claude.go":              "claude/client.go",
	"internal/proxy/sse.go":                 "utils/sse.go",
	"internal/handlers/models.go":           "handlers/common.go",
}

var (
	repoFilesOnce sync.Once
	repoFiles     map[string]struct{}
)

func legacyRepoPathMapping(filePath string) (string, bool) {
	normalized := normalizeToolFilePath(filePath)
	if normalized == "" {
		return "", false
	}
	if mapped, ok := legacyRepoPathAliases[normalized]; ok {
		return mapped, true
	}
	for oldPath, newPath := range legacyRepoPathAliases {
		if strings.HasSuffix(normalized, "/"+oldPath) {
			return newPath, true
		}
	}
	if mapped, ok := existingRepoFileMapping(normalized); ok {
		return mapped, true
	}
	return "", false
}

func existingRepoFileMapping(normalized string) (string, bool) {
	files := repositoryFiles()
	if len(files) == 0 {
		return "", false
	}
	parts := strings.Split(normalized, "/")
	for i := 0; i < len(parts); i++ {
		candidate := path.Clean(strings.Join(parts[i:], "/"))
		if candidate == "." || candidate == "" {
			continue
		}
		if _, ok := files[candidate]; ok {
			return candidate, true
		}
	}
	return "", false
}

func repositoryFiles() map[string]struct{} {
	repoFilesOnce.Do(func() {
		repoFiles = make(map[string]struct{})
		root := findRepoRoot()
		if root == "" {
			return
		}
		_ = filepath.WalkDir(root, func(fullPath string, d os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			name := d.Name()
			if d.IsDir() {
				if strings.HasPrefix(name, ".") && fullPath != root {
					return filepath.SkipDir
				}
				return nil
			}
			rel, err := filepath.Rel(root, fullPath)
			if err != nil {
				return nil
			}
			repoFiles[path.Clean(filepath.ToSlash(rel))] = struct{}{}
			return nil
		})
	})
	return repoFiles
}

func findRepoRoot() string {
	wd, err := os.Getwd()
	if err != nil {
		return ""
	}
	for dir := wd; dir != "" && dir != filepath.Dir(dir); dir = filepath.Dir(dir) {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
	}
	return ""
}

func normalizeToolFilePath(filePath string) string {
	filePath = strings.TrimSpace(filePath)
	if filePath == "" {
		return ""
	}
	filePath = strings.ReplaceAll(filePath, "\\", "/")
	filePath = strings.TrimRight(filePath, "/")
	filePath = strings.TrimPrefix(filePath, "./")
	filePath = strings.TrimPrefix(filePath, "/")
	return path.Clean(filePath)
}

func replaceToolCallFilePath(input map[string]interface{}, mappedPath string) map[string]interface{} {
	if input == nil {
		return map[string]interface{}{"file_path": mappedPath}
	}
	replaced := make(map[string]interface{}, len(input))
	for k, v := range input {
		replaced[k] = v
	}
	if _, ok := replaced["file_path"]; ok {
		replaced["file_path"] = mappedPath
	}
	if _, ok := replaced["path"]; ok {
		replaced["path"] = mappedPath
	}
	if _, ok := replaced["file_path"]; !ok && replaced["path"] == nil {
		replaced["file_path"] = mappedPath
	}
	return replaced
}

func fileDiscoveryInput(toolName, base string) map[string]interface{} {
	switch toolName {
	case "Glob":
		return map[string]interface{}{"pattern": "**/" + base, "path": "."}
	case "list_files":
		return map[string]interface{}{"path": ".", "recursive": true}
	default:
		return map[string]interface{}{}
	}
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
	sb.WriteString("\n\nYou can call external functions when needed. To invoke a function, output it in this format:\n")
	sb.WriteString("[TOOL_CALL]{\"name\":\"function_name\",\"id\":\"call_1\",\"input\":{\"param\":\"value\"}}[/TOOL_CALL]\n\n")
	sb.WriteString("Available functions:\n")

	// Inject tool_choice constraint so the model honours the caller's selection policy.
	choiceType := ""
	choiceName := ""
	if m, ok := toolChoice.(map[string]interface{}); ok {
		choiceType, _ = m["type"].(string)
		choiceName, _ = m["name"].(string)
	}
	switch choiceType {
	case "none":
		sb.WriteString("")
	case "any":
		sb.WriteString("Note: Please invoke at least one function for this request.\n")
	case "tool":
		if choiceName != "" {
			sb.WriteString(fmt.Sprintf("Note: Please invoke the %q function.\n", choiceName))
		} else {
			sb.WriteString("Note: Please invoke one of the available functions.\n")
		}
	default:
		sb.WriteString("")
	}

	sb.WriteString("\n")
	b, _ := json.MarshalIndent(tools, "", "  ")
	sb.Write(b)
	sb.WriteString("\n")
	return sb.String()
}

// buildExampleInput returns a compact single-line JSON object with placeholder
// values for each required property in the tool's input_schema. It is used only
// for illustrative few-shot examples inside the prompt — values don't matter,
// only the key names and JSON structure need to be recognisable.
func buildExampleInput(tool models.AnthropicTool) string {
	if tool.InputSchema == nil {
		return "{}"
	}
	props, _ := tool.InputSchema["properties"].(map[string]interface{})
	required, _ := tool.InputSchema["required"].([]interface{})
	if len(props) == 0 {
		return "{}"
	}
	// Prefer required properties; fall back to all properties when none are marked.
	keys := make([]string, 0, len(required))
	for _, r := range required {
		if k, ok := r.(string); ok {
			if _, exists := props[k]; exists {
				keys = append(keys, k)
			}
		}
	}
	if len(keys) == 0 {
		for k := range props {
			keys = append(keys, k)
		}
	}
	// Cap at 3 keys to keep the example concise.
	if len(keys) > 3 {
		keys = keys[:3]
	}
	// Build a placeholder value for each key based on its declared type.
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		propDef, _ := props[k].(map[string]interface{})
		typ, _ := propDef["type"].(string)
		var placeholder string
		switch typ {
		case "integer", "number":
			placeholder = "0"
		case "boolean":
			placeholder = "false"
		case "array":
			placeholder = "[]"
		case "object":
			placeholder = "{}"
		default:
			placeholder = fmt.Sprintf("%q", "<"+k+">")
		}
		parts = append(parts, fmt.Sprintf("%q:%s", k, placeholder))
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// buildToolPrompt assembles the full prompt for one round.
func buildToolPrompt(system interface{}, messages []models.AnthropicMessage, toolDefText string) string {
	var parts []string
	systemText := systemToText(system)
	if toolDefText != "" {
		// 将工具定义融入系统消息中
		if systemText != "" {
			parts = append(parts, "[System]\n"+systemText+"\n\n"+toolDefText)
		} else {
			parts = append(parts, "[System]\n"+toolDefText)
		}
	} else if systemText != "" {
		parts = append(parts, "[System]\n"+systemText)
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
	// Append a short format reminder immediately before the assistant turn so the
	// model's most recent instruction is the correct syntax, reducing the chance
	// that it adopts a different format from earlier conversation context.
	parts = append(parts, "[Human]\nRemember: use [TOOL_CALL]{...}[/TOOL_CALL] for every tool call — one block per call, flat JSON, no code fences.")
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

func logToolCallRound(round string, strippedText string, calls []toolCall) {
	log.Printf("tool loop round=%s text_runes=%d tool_calls=%d", round, len([]rune(strippedText)), len(calls))
	for i, call := range calls {
		arguments := mustJSON(call.Input)
		sum := sha256.Sum256([]byte(arguments))
		log.Printf(
			"tool loop round=%s call_index=%d call_id=%q tool=%q args_bytes=%d args_sha256=%s",
			round,
			i,
			call.ID,
			call.Name,
			len(arguments),
			hex.EncodeToString(sum[:8]),
		)
	}
}

func contentSize(v interface{}) int {
	if v == nil {
		return 0
	}
	switch x := v.(type) {
	case string:
		return len(x)
	default:
		b, err := json.Marshal(x)
		if err != nil {
			return len(fmt.Sprintf("%v", x))
		}
		return len(b)
	}
}
