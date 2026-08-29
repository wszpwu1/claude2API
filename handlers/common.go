package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"claude2api/claude"
	"claude2api/config"
	"claude2api/models"
	"claude2api/utils"

	"github.com/gin-gonic/gin"
)

// Handler holds shared config and routes requests to claude.ai
type Handler struct {
	cfg           *config.Config
	conversations *conversationStore
	clients       *clientPool
	deleteSlots   chan struct{}
}

// NewHandler creates a handler.
func NewHandler(cfg *config.Config) *Handler {
	clients, err := newClientPool(cfg.ClaudeBaseURL, cfg.Accounts)
	if err != nil {
		panic(err)
	}
	return &Handler{
		cfg:           cfg,
		conversations: newConversationStore(),
		clients:       clients,
		deleteSlots:   make(chan struct{}, 32),
	}
}

// ReplaceAccounts atomically rebuilds the runtime client pool so account
// management changes take effect without restarting the service.
func (h *Handler) ReplaceAccounts(accounts []config.Account) error {
	if err := h.clients.replaceAccounts(accounts); err != nil {
		return err
	}
	h.cfg.Accounts = append([]config.Account(nil), accounts...)
	return nil
}

// HasConfiguredAccounts reports whether the runtime pool currently contains at
// least one enabled account. It is safe to call concurrently with hot updates.
func (h *Handler) HasConfiguredAccounts() bool {
	h.clients.accountsMu.RLock()
	defer h.clients.accountsMu.RUnlock()
	return len(h.clients.accounts) > 0
}

// SetRoutingPolicy switches runtime account selection between round-robin and
// least-loaded routing without restarting the service.
func (h *Handler) SetRoutingPolicy(policy string) {
	h.clients.setRoutingPolicy(policy)
}

// RestoreAccount immediately clears an account's cooldown state.
func (h *Handler) RestoreAccount(accountID string) bool {
	return h.clients.restoreAccount(accountID)
}

// AccountCooldowns returns active account cooldown deadlines keyed by account ID.
func (h *Handler) AccountCooldowns() map[string]time.Time {
	return h.clients.accountCooldowns(time.Now())
}

// CheckAccount verifies that a configured account can access the upstream API.
// The result immediately updates whether the account participates in routing.
func (h *Handler) CheckAccount(ctx context.Context, accountID string) error {
	account, ok := h.clients.accountByID(accountID)
	if !ok {
		return fmt.Errorf("account not found")
	}
	checkCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if _, err := account.client.ValidateOrganization(checkCtx); err != nil {
		h.clients.setAccountHealthy(accountID, false)
		h.handleAccountError(accountID, err)
		return err
	}
	h.clients.setAccountHealthy(accountID, true)
	return nil
}

func (h *Handler) handleAccountError(accountID string, err error) {
	if err == nil || accountID == "" {
		return
	}
	message := strings.ToLower(err.Error())
	switch {
	case strings.Contains(message, "status 429"):
		// Rate limiting is temporary: cool the account down and retry it later.
		h.clients.cooldownAccount(accountID)
	case strings.Contains(message, "status 401"), strings.Contains(message, "status 403"):
		// Authentication rejection or account blocking is not a temporary rate
		// limit. Remove the account from routing until an explicit health check
		// succeeds or an administrator restores it.
		h.clients.setAccountHealthy(accountID, false)
	}
}

// resolveModel returns the claude.ai model id for a requested model, or an error
func resolveModel(requested, fallback string) (string, error) {
	if requested == "" {
		requested = fallback
	}
	m, ok := config.SupportedModels[requested]
	if !ok {
		return "", fmt.Errorf("model '%s' is not supported", requested)
	}
	return m, nil
}

// resolveEffort validates an effort string and returns a claude.ai-safe value.
func resolveEffort(effort string) string {
	switch strings.ToLower(strings.TrimSpace(effort)) {
	case "low", "medium", "high", "xhigh", "max":
		return strings.ToLower(strings.TrimSpace(effort))
	default:
		return "medium"
	}
}

// resolveThinking maps Claude Code's thinking config to claude.ai's thinking_mode.
// Claude Code sends {"type":"enabled","budget_tokens":N} or {"type":"disabled"}.
// claude.ai accepts "auto" | "none". We also return the budget for potential
// future use (claude.ai web does not expose a budget knob).
func resolveThinking(thinking interface{}) (thinkingMode string, budgetTokens int) {
	if thinking == nil {
		return "auto", 0
	}
	m, ok := thinking.(map[string]interface{})
	if !ok {
		return "auto", 0
	}
	typ, _ := m["type"].(string)
	switch typ {
	case "disabled":
		return "none", 0
	case "enabled":
		budget, _ := m["budget_tokens"].(float64)
		return "auto", int(budget)
	default:
		return "auto", 0
	}
}

// acquireClient returns a reusable account client and a release callback.
func (h *Handler) acquireClient(c *gin.Context, conversationID string) (*clientLease, error) {
	sessionKeyValue, _ := c.Get("sessionKey")
	cookieValue, _ := c.Get("claudeCookie")
	explicitValue, _ := c.Get("explicitCredentials")

	sessionKey, _ := sessionKeyValue.(string)
	claudeCookie, _ := cookieValue.(string)
	explicitCredentials, _ := explicitValue.(bool)
	lease, err := h.clients.acquire(sessionKey, claudeCookie, explicitCredentials, conversationID)
	if err != nil {
		return nil, err
	}
	c.Set("accountID", lease.accountID)
	return lease, nil
}

func (h *Handler) deleteTemporaryConversation(client *claude.Client, conversationID string) {
	select {
	case h.deleteSlots <- struct{}{}:
		go func() {
			defer func() { <-h.deleteSlots }()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_ = client.DeleteConversation(ctx, conversationID)
		}()
	default:
		// Cleanup is best-effort. Under a burst, do not create an unbounded number
		// of goroutines or hold response handlers open waiting for delete capacity.
	}
}

// runCompletion drives a full claude.ai round-trip: create conversation, send
// prompt, then invoke onText for each incremental text delta.
// Returns the accumulated thinking text, the accumulated text, or an error.
// When tools is nil, the default claude.ai web tools are sent; pass an empty
// slice (or custom payload) to override — used by the tool-simulation loop.
// thinking overrides the proxy-level thinking config (e.g. from request body).
func (h *Handler) runCompletion(ctx context.Context, client *claude.Client, prompt, claudeModel, effort, conversationID, accountID string,
	onText func(text string), tools []json.RawMessage, thinkingOverride ...string) (string, string, error) {

	convID := ""
	persistent := conversationID != ""
	var state *conversationState
	if persistent {
		var created bool
		var release func()
		state, created, release = h.conversations.acquire(accountID, conversationID)
		defer release()
		if created {
			createdID, err := client.CreateConversation(ctx, "chat")
			if err != nil {
				h.conversations.removeIfSame(state)
				h.handleAccountError(accountID, err)
				return "", "", fmt.Errorf("create conversation: %w", err)
			}
			state.ClaudeConversationID = createdID
			state.UpdatedAt = time.Now()
		}
		convID = state.ClaudeConversationID
	} else {
		createdID, err := client.CreateConversation(ctx, "chat")
		if err != nil {
			h.handleAccountError(accountID, err)
			return "", "", fmt.Errorf("create conversation: %w", err)
		}
		convID = createdID
		defer h.deleteTemporaryConversation(client, convID)
	}

	humanUUID := utils.GenerateUUID()
	assistantUUID := utils.GenerateUUID()
	parentUUID := utils.GenerateUUID()
	if state != nil && state.LastAssistantUUID != "" {
		parentUUID = state.LastAssistantUUID
	}

	// Thinking: per-request override (from Claude Code's request body) wins,
	// else fall back to proxy-level config.
	thinkingMode, _ := resolveThinking(h.cfg.Thinking)
	if len(thinkingOverride) > 0 && thinkingOverride[0] != "" {
		thinkingMode = thinkingOverride[0]
	}

	// Choose tools payload: caller override, or default web tools.
	toolsPayload := claude.WebTools()
	if len(tools) > 0 {
		toolsPayload = tools[0]
	}

	// Build the real request body matching claude.ai's format
	req := &models.ClaudeCompletionRequest{
		Prompt:            prompt,
		ParentMessageUUID: parentUUID,
		Timezone:          h.cfg.Timezone,
		Locale:            h.cfg.Locale,
		Model:             claudeModel,
		Effort:            effort,
		ThinkingMode:      thinkingMode,
		Tools:             toolsPayload,
		TurnMessageUUIDs: &models.TurnMessageUUIDs{
			HumanMessageUUID:     humanUUID,
			AssistantMessageUUID: assistantUUID,
		},
		Attachments:   []models.ClaudeAttachment{},
		Files:         []models.ClaudeFile{},
		SyncSources:   []interface{}{},
		RenderingMode: "messages",
		CreateConversationParams: &models.CreateConversationParams{
			Name:                           "",
			Model:                          claudeModel,
			IncludeConversationPreferences: true,
			PaprikaMode:                    nil,
			CompassMode:                    nil,
			ToolSearchMode:                 "auto",
			IsTemporary:                    false,
			EnabledImagine:                 true,
		},
	}

	events, err := client.SendMessage(ctx, convID, req)
	if err != nil {
		h.handleAccountError(accountID, err)
		return "", "", fmt.Errorf("send message: %w", err)
	}

	var thinkingBuf, sb strings.Builder
	for evt := range events {
		if evt.Error != nil {
			err := fmt.Errorf("upstream: %s", evt.Error.Message)
			h.handleAccountError(accountID, err)
			return thinkingBuf.String(), sb.String(), err
		}
		// Capture thinking blocks.
		if t := claude.ExtractThinkingFromSSE(evt); t != "" {
			thinkingBuf.WriteString(t)
		}
		// Capture text blocks.
		if text := claude.ExtractTextFromSSE(evt); text != "" {
			sb.WriteString(text)
			if onText != nil {
				onText(text)
			}
		}
		if claude.IsStopEvent(evt) {
			break
		}
	}
	if persistent {
		state.LastHumanUUID = humanUUID
		state.LastAssistantUUID = assistantUUID
		state.UpdatedAt = time.Now()
	}
	return thinkingBuf.String(), sb.String(), nil
}

// writeSSE writes a Server-Sent Event. Anthropic clients such as Roo Code
// require the named event line in addition to the JSON data line; without it,
// the stream can be consumed without producing any assistant messages.
func writeSSE(w io.Writer, payload interface{}) {
	b, err := json.Marshal(payload)
	if err != nil {
		return
	}
	if event, ok := payload.(interface{ SSEEvent() string }); ok {
		_, _ = io.WriteString(w, "event: "+event.SSEEvent()+"\n")
	}
	_, _ = io.WriteString(w, "data: "+string(b)+"\n\n")
}

// ListModels returns OpenAI-compatible /v1/models
func (h *Handler) ListModels(c *gin.Context) {
	data := make([]models.ModelInfo, 0, len(config.SupportedModels))
	now := time.Now().Unix()
	for id := range config.SupportedModels {
		data = append(data, models.ModelInfo{
			ID:      id,
			Object:  "model",
			Created: now,
			OwnedBy: "anthropic",
		})
	}
	c.JSON(http.StatusOK, models.ModelsResponse{Object: "list", Data: data})
}

// genID returns a short id like "chatcmpl-xxxxxxxx"
func genID(prefix string) string {
	return prefix + utils.GenerateUUID()[:8]
}

// error helpers
func badRequest(c *gin.Context, msg string) {
	c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": msg, "type": "invalid_request_error"}})
}

func internalError(c *gin.Context, msg string) {
	c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"message": msg, "type": "internal_error"}})
}

func upstreamError(c *gin.Context, msg string) {
	c.JSON(http.StatusBadGateway, gin.H{"error": gin.H{"message": msg, "type": "upstream_error"}})
}
