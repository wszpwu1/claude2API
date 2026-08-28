package handlers

import (
	"net/http"
	"strings"
	"time"

	"claude2api/claude"
	"claude2api/models"

	"github.com/gin-gonic/gin"
)

// Responses handles POST /v1/responses (OpenAI Responses API, streaming + non-streaming)
func (h *Handler) Responses(c *gin.Context) {
	var req models.ResponsesRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, "invalid request body: "+err.Error())
		return
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

	prompt := buildResponsesPrompt(req)
	effort := resolveEffort(h.cfg.Effort)

	if req.Stream {
		h.responsesStream(c, lease.client, prompt, claudeModel, effort, req.ConversationID, lease.accountID)
	} else {
		h.responsesNonStream(c, lease.client, prompt, claudeModel, effort, req.ConversationID, lease.accountID)
	}
}

// buildResponsesPrompt converts Responses API input to a prompt
func buildResponsesPrompt(req models.ResponsesRequest) string {
	var parts []string
	if req.Instructions != "" {
		parts = append(parts, "[System]\n"+req.Instructions)
	}
	text := responsesInputToText(req.Input)
	if text != "" {
		parts = append(parts, "[Human]\n"+text)
	}
	parts = append(parts, "[Assistant]\n")
	return strings.Join(parts, "\n\n")
}

// responsesInputToText flattens input (string or []item) to text
func responsesInputToText(input interface{}) string {
	switch v := input.(type) {
	case string:
		return v
	case []interface{}:
		var sb strings.Builder
		for _, item := range v {
			m, ok := item.(map[string]interface{})
			if !ok {
				continue
			}
			role, _ := m["role"].(string)
			content := m["content"]
			var chunk string
			switch c := content.(type) {
			case string:
				chunk = c
			case []interface{}:
				chunk = flattenContentParts(c)
			}
			if chunk == "" {
				continue
			}
			label := "[Human]"
			switch role {
			case "assistant":
				label = "[Assistant]"
			case "system", "developer":
				label = "[System]"
			case "user":
				label = "[Human]"
			}
			sb.WriteString(label + "\n" + chunk + "\n\n")
		}
		return strings.TrimSpace(sb.String())
	default:
		return ""
	}
}

// flattenContentParts extracts text from a content array of parts
func flattenContentParts(parts []interface{}) string {
	var sb strings.Builder
	for _, p := range parts {
		if m, ok := p.(map[string]interface{}); ok {
			if t, _ := m["type"].(string); t == "input_text" || t == "text" {
				if s, _ := m["text"].(string); s != "" {
					sb.WriteString(s)
				}
			}
		}
	}
	return sb.String()
}

func (h *Handler) responsesNonStream(c *gin.Context, client *claude.Client, prompt, claudeModel, effort, conversationID, accountID string) {
	_, content, err := h.runCompletion(c.Request.Context(), client, prompt, claudeModel, effort, conversationID, accountID, nil, nil)
	if err != nil {
		upstreamError(c, err.Error())
		return
	}
	respID := genID("resp_")
	msgID := genID("msg_")
	resp := models.ResponsesResponse{
		ID:        respID,
		Object:    "response",
		CreatedAt: time.Now().Unix(),
		Model:     claudeModel,
		Status:    "completed",
		Output: []models.ResponseOutputItem{{
			Type:    "message",
			ID:      msgID,
			Role:    "assistant",
			Status:  "completed",
			Content: []models.ResponseContentPart{{Type: "output_text", Text: content}},
		}},
		Usage: models.ResponsesUsage{
			OutputTokens: len(content) / 4,
			TotalTokens:  len(content) / 4,
		},
	}
	c.JSON(http.StatusOK, resp)
}

func (h *Handler) responsesStream(c *gin.Context, client *claude.Client, prompt, claudeModel, effort, conversationID, accountID string) {
	c.Writer.Header().Set("Content-Type", "text/event-stream")
	c.Writer.Header().Set("Cache-Control", "no-cache")
	c.Writer.Header().Set("Connection", "keep-alive")
	c.Writer.Header().Set("X-Accel-Buffering", "no")
	flusher, _ := c.Writer.(http.Flusher)

	respID := genID("resp_")
	msgID := genID("msg_")
	created := time.Now().Unix()

	writeSSE(c.Writer, models.ResponsesResponseCreated{
		Type: "response.created",
		Response: models.ResponsesResponse{
			ID: respID, Object: "response", CreatedAt: created, Model: claudeModel, Status: "in_progress",
		},
	})

	writeSSE(c.Writer, models.ResponsesOutputItemAdded{
		Type: "response.output_item.added", OutputIndex: 0,
		Item: models.ResponseOutputItem{
			Type: "message", ID: msgID, Role: "assistant", Status: "in_progress", Content: []models.ResponseContentPart{},
		},
	})

	writeSSE(c.Writer, models.ResponsesContentPartAdded{
		Type: "response.content_part.added", ItemID: msgID, OutputIndex: 0, ContentIndex: 0,
		Part: models.ResponseContentPart{Type: "output_text", Text: ""},
	})

	var full strings.Builder
	_, _, err := h.runCompletion(c.Request.Context(), client, prompt, claudeModel, effort, conversationID, accountID, func(text string) {
		full.WriteString(text)
		writeSSE(c.Writer, models.ResponsesOutputTextDelta{
			Type: "response.output_text.delta", ItemID: msgID, OutputIndex: 0, ContentIndex: 0, Delta: text,
		})
		if flusher != nil {
			flusher.Flush()
		}
	}, nil)

	writeSSE(c.Writer, models.ResponsesContentPartDone{
		Type: "response.content_part.done", ItemID: msgID, OutputIndex: 0, ContentIndex: 0,
		Part: models.ResponseContentPart{Type: "output_text", Text: full.String()},
	})

	writeSSE(c.Writer, models.ResponsesOutputItemDone{
		Type: "response.output_item.done", OutputIndex: 0,
		Item: models.ResponseOutputItem{
			Type: "message", ID: msgID, Role: "assistant", Status: "completed",
			Content: []models.ResponseContentPart{{Type: "output_text", Text: full.String()}},
		},
	})

	status := "completed"
	if err != nil {
		status = "failed"
	}
	writeSSE(c.Writer, models.ResponsesCompleted{
		Type: "response.completed",
		Response: models.ResponsesResponse{
			ID: respID, Object: "response", CreatedAt: created, Model: claudeModel, Status: status,
			Output: []models.ResponseOutputItem{{
				Type: "message", ID: msgID, Role: "assistant", Status: status,
				Content: []models.ResponseContentPart{{Type: "output_text", Text: full.String()}},
			}},
			Usage: models.ResponsesUsage{OutputTokens: full.Len() / 4, TotalTokens: full.Len() / 4},
		},
	})
	if flusher != nil {
		flusher.Flush()
	}
}
