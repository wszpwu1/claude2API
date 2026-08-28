package handlers

import (
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

	prompt := claude.BuildPrompt(req.Messages)
	effort := resolveEffort(h.cfg.Effort)

	if req.Stream {
		h.chatCompletionStream(c, lease.client, prompt, claudeModel, effort, req.ConversationID, lease.accountID)
	} else {
		h.chatCompletionNonStream(c, lease.client, prompt, claudeModel, effort, req.ConversationID, lease.accountID)
	}
}

func (h *Handler) chatCompletionNonStream(c *gin.Context, client *claude.Client, prompt, claudeModel, effort, conversationID, accountID string) {
	_, content, err := h.runCompletion(c.Request.Context(), client, prompt, claudeModel, effort, conversationID, accountID, nil, nil)
	if err != nil {
		upstreamError(c, err.Error())
		return
	}
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
			CompletionTokens: len(content) / 4,
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
	}, nil)
	if err != nil {
		writeSSE(c.Writer, models.ChatCompletionChunk{
			ID: chunkID, Object: "chat.completion.chunk", Created: created, Model: claudeModel,
			Choices: []models.ChunkChoice{{Index: 0, Delta: models.Delta{Content: "[error: " + err.Error() + "]"}}},
		})
	}

	stop := "stop"
	writeSSE(c.Writer, models.ChatCompletionChunk{
		ID: chunkID, Object: "chat.completion.chunk", Created: created, Model: claudeModel,
		Choices: []models.ChunkChoice{{Index: 0, Delta: models.Delta{}, FinishReason: &stop}},
	})
	_, _ = c.Writer.WriteString("data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}
