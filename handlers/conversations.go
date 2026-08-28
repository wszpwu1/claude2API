package handlers

import (
	"context"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

// DeleteConversation deletes a persistent conversation mapping and its upstream claude.ai conversation.
func (h *Handler) DeleteConversation(c *gin.Context) {
	conversationID := c.Param("id")
	if conversationID == "" {
		badRequest(c, "conversation id is required")
		return
	}

	lease, err := h.acquireClient(c, conversationID)
	if err != nil {
		internalError(c, "create client: "+err.Error())
		return
	}
	defer lease.release()

	state, ok := h.conversations.delete(lease.accountID, conversationID)
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": gin.H{"message": "conversation not found", "type": "not_found_error"}})
		return
	}
	defer state.mu.Unlock()

	ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
	defer cancel()
	if err := lease.client.DeleteConversation(ctx, state.ClaudeConversationID); err != nil {
		upstreamError(c, err.Error())
		return
	}

	h.clients.forgetConversation(conversationID)
	c.JSON(http.StatusOK, gin.H{"id": conversationID, "deleted": true})
}
