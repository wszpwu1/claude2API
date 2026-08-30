package admin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"time"

	"claude2api/utils"

	"github.com/gin-gonic/gin"
)

var errAccountExists = errors.New("account already exists")

// AccountRuntimeStats carries per-account counters fetched from the runtime pool.
type AccountRuntimeStats struct {
	ActiveRequests int64
	SessionUsed    int64
}

// API exposes administrator authentication and management endpoints.
type API struct {
	store                  *Store
	auth                   *AuthManager
	metrics                *Metrics
	onAccountsChanged      func([]Account) error
	onSettingsChanged      func(PanelSettings) error
	onRateLimitChanged     func(RateLimitConfig) error
	onKeepAliveChanged     func(KeepAliveConfig) error
	onAccountCheck         func(context.Context, string) error
	onAccountRestore       func(string) bool
	onAccountCooldowns     func() map[string]time.Time
	onAccountStats         func(accountID string) (AccountRuntimeStats, bool)
	onModelMappingsChanged func([]ModelMapping) error
}

// NewAPI creates the management API.
func NewAPI(store *Store, auth *AuthManager, metrics *Metrics) *API {
	return &API{store: store, auth: auth, metrics: metrics}
}

// SetAccountsChangedHandler registers a callback that refreshes runtime account
// clients after an account mutation has been persisted successfully.
func (a *API) SetAccountsChangedHandler(handler func([]Account) error) {
	a.onAccountsChanged = handler
}

func (a *API) notifyAccountsChanged() error {
	if a.onAccountsChanged == nil {
		return nil
	}
	return a.onAccountsChanged(a.store.Snapshot().Accounts)
}

// SetSettingsChangedHandler registers a callback that applies routing settings
// to the runtime account pool without restarting the service.
func (a *API) SetSettingsChangedHandler(handler func(PanelSettings) error) {
	a.onSettingsChanged = handler
}

// SetRateLimitChangedHandler registers a callback that applies global rate-limit
// configuration immediately without restarting the service.
func (a *API) SetRateLimitChangedHandler(handler func(RateLimitConfig) error) {
	a.onRateLimitChanged = handler
}

// SetKeepAliveChangedHandler registers a callback that updates the account
// keep-alive worker immediately without restarting the service.
func (a *API) SetKeepAliveChangedHandler(handler func(KeepAliveConfig) error) {
	a.onKeepAliveChanged = handler
}

// SetAccountCheckHandler registers the runtime account health-check callback.
func (a *API) SetAccountCheckHandler(handler func(context.Context, string) error) {
	a.onAccountCheck = handler
}

// SetAccountRestoreHandler registers the runtime cooldown restore callback.
func (a *API) SetAccountRestoreHandler(handler func(string) bool) {
	a.onAccountRestore = handler
}

// SetAccountCooldownsHandler registers the runtime cooldown snapshot callback.
func (a *API) SetAccountCooldownsHandler(handler func() map[string]time.Time) {
	a.onAccountCooldowns = handler
}

// SetAccountStatsHandler registers a callback that returns live per-account
// runtime counters (active requests, sessions used) from the client pool.
func (a *API) SetAccountStatsHandler(handler func(string) (AccountRuntimeStats, bool)) {
	a.onAccountStats = handler
}

// SetModelMappingsChangedHandler registers a callback that applies model mapping
// changes to the runtime handler immediately without restarting the service.
func (a *API) SetModelMappingsChangedHandler(handler func([]ModelMapping) error) {
	a.onModelMappingsChanged = handler
}

func (a *API) notifyModelMappingsChanged() error {
	if a.onModelMappingsChanged == nil {
		return nil
	}
	return a.onModelMappingsChanged(a.store.Snapshot().ModelMappings)
}

// RegisterRoutes mounts public authentication and protected management routes.
func (a *API) RegisterRoutes(router *gin.Engine) {
	admin := router.Group("/admin/api")
	admin.POST("/login", a.login)

	protected := admin.Group("")
	protected.Use(a.auth.Middleware())
	protected.POST("/logout", a.logout)
	protected.PUT("/password", a.changePassword)
	protected.GET("/state", a.state)
	protected.GET("/metrics", a.getMetrics)
	protected.GET("/usage", a.getUsage)
	protected.GET("/recent-requests", a.getRecentRequests)
	protected.GET("/accounts", a.listAccounts)
	protected.POST("/accounts", a.addAccount)
	protected.POST("/accounts/import", a.importAccounts)
	protected.POST("/accounts/check", a.checkAccounts)
	protected.POST("/accounts/restore", a.restoreAccounts)
	protected.DELETE("/accounts/blocked", a.deleteBlockedAccounts)
	protected.PUT("/accounts/:id", a.updateAccount)
	protected.PATCH("/accounts/:id/status", a.updateAccountStatus)
	protected.POST("/accounts/:id/restore", a.restoreAccount)
	protected.DELETE("/accounts/:id", a.deleteAccount)
	protected.GET("/settings", a.getSettings)
	protected.PUT("/settings", a.updateSettings)
	protected.GET("/proxy", a.getProxy)
	protected.PUT("/proxy", a.updateProxy)
	protected.GET("/rate-limit", a.getRateLimit)
	protected.PUT("/rate-limit", a.updateRateLimit)
	protected.GET("/keep-alive", a.getKeepAlive)
	protected.PUT("/keep-alive", a.updateKeepAlive)
	protected.GET("/master-key", a.getMasterKey)
	protected.PUT("/master-key", a.updateMasterKey)
	protected.DELETE("/master-key", a.deleteMasterKey)
	protected.GET("/api-keys", a.listAPIKeys)
	protected.POST("/api-keys", a.createAPIKey)
	protected.PUT("/api-keys/:id", a.updateAPIKey)
	protected.DELETE("/api-keys/:id", a.deleteAPIKey)
	protected.GET("/model-mappings", a.listModelMappings)
	protected.POST("/model-mappings", a.createModelMapping)
	protected.PUT("/model-mappings/:id", a.updateModelMapping)
	protected.DELETE("/model-mappings/:id", a.deleteModelMapping)
}

type loginRequest struct {
	Password string `json:"password" binding:"required"`
}

func (a *API) login(c *gin.Context) {
	var request loginRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "password is required", "type": "invalid_request_error"}})
		return
	}

	token, expiresAt, ok := a.auth.Login(request.Password)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": gin.H{"message": "invalid administrator password", "type": "unauthorized"}})
		return
	}

	c.SetSameSite(http.SameSiteStrictMode)
	c.SetCookie("claude2api_admin", token, int(time.Until(expiresAt).Seconds()), "/", "", c.Request.TLS != nil, true)
	c.JSON(http.StatusOK, gin.H{"token": token, "expires_at": expiresAt})
}

func (a *API) logout(c *gin.Context) {
	a.auth.Logout(adminToken(c))
	c.SetSameSite(http.SameSiteStrictMode)
	c.SetCookie("claude2api_admin", "", -1, "/", "", c.Request.TLS != nil, true)
	c.Status(http.StatusNoContent)
}

type changePasswordRequest struct {
	CurrentPassword string `json:"current_password" binding:"required"`
	NewPassword     string `json:"new_password" binding:"required"`
}

func (a *API) changePassword(c *gin.Context) {
	var request changePasswordRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "current_password and new_password are required", "type": "invalid_request_error"}})
		return
	}
	if err := a.store.ChangePassword(request.CurrentPassword, request.NewPassword); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": err.Error(), "type": "invalid_request_error"}})
		return
	}
	a.auth.RevokeAll()
	c.SetSameSite(http.SameSiteStrictMode)
	c.SetCookie("claude2api_admin", "", -1, "/", "", c.Request.TLS != nil, true)
	c.Status(http.StatusNoContent)
}

func (a *API) state(c *gin.Context) {
	state := a.store.Snapshot()
	state.AdminPasswordHash = ""
	state.MasterKey.KeyHash = ""
	cooldowns := a.accountCooldownSnapshot()
	for index := range state.Accounts {
		if t, ok := cooldowns[state.Accounts[index].ID]; ok {
			state.Accounts[index].CooldownUntil = &t
		}
		a.mergeRuntimeStats(&state.Accounts[index])
		prepareAccountForResponse(&state.Accounts[index])
	}
	for index := range state.APIKeys {
		state.APIKeys[index].KeyHash = ""
	}
	if state.ModelMappings == nil {
		state.ModelMappings = []ModelMapping{}
	}
	c.JSON(http.StatusOK, state)
}

func (a *API) getMetrics(c *gin.Context) {
	c.JSON(http.StatusOK, a.metrics.Snapshot())
}

func (a *API) getUsage(c *gin.Context) {
	state := a.store.Snapshot()
	usage := PruneUsage(state.DailyUsage, time.Now().UTC())
	c.JSON(http.StatusOK, gin.H{"daily_usage": usage})
}

func (a *API) getRecentRequests(c *gin.Context) {
	requests := a.store.Snapshot().RecentRequests
	if len(requests) > 50 {
		requests = requests[:50]
	}
	c.JSON(http.StatusOK, gin.H{"recent_requests": requests})
}

type accountRequest struct {
	Name         string `json:"name"`
	SessionKey   string `json:"session_key"`
	Cookie       string `json:"cookie"`
	SessionLimit int64  `json:"session_limit"`
}

type accountImportRequest struct {
	Content string `json:"content" binding:"required"`
}

type accountUpdateRequest struct {
	Name         *string `json:"name"`
	SessionKey   *string `json:"session_key"`
	Cookie       *string `json:"cookie"`
	Enabled      *bool   `json:"enabled"`
	SessionLimit *int64  `json:"session_limit"`
}

type accountStatusRequest struct {
	Enabled *bool `json:"enabled" binding:"required"`
}

func (a *API) listAccounts(c *gin.Context) {
	accounts := a.store.Snapshot().Accounts
	cooldowns := a.accountCooldownSnapshot()
	for index := range accounts {
		if t, ok := cooldowns[accounts[index].ID]; ok {
			accounts[index].CooldownUntil = &t
		}
		a.mergeRuntimeStats(&accounts[index])
		prepareAccountForResponse(&accounts[index])
	}
	c.JSON(http.StatusOK, gin.H{"accounts": accounts})
}

func (a *API) accountCooldownSnapshot() map[string]time.Time {
	if a.onAccountCooldowns == nil {
		return nil
	}
	return a.onAccountCooldowns()
}

// mergeRuntimeStats overlays live counters (active requests, session usage)
// from the runtime pool onto a persisted Account before it is returned to the
// caller. The persisted SessionUsed acts as a floor: runtime usage is taken
// when it is higher (i.e. since the last persistence cycle).
func (a *API) mergeRuntimeStats(account *Account) {
	if a.onAccountStats == nil {
		return
	}
	stats, ok := a.onAccountStats(account.ID)
	if !ok {
		return
	}
	account.ActiveRequests = stats.ActiveRequests
	if stats.SessionUsed > account.SessionUsed {
		account.SessionUsed = stats.SessionUsed
	}
}

func (a *API) checkAccounts(c *gin.Context) {
	if a.onAccountCheck == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": gin.H{"message": "account health check is unavailable", "type": "runtime_error"}})
		return
	}
	accounts := a.store.Snapshot().Accounts

	// Run all health checks first, then persist results in one atomic write.
	type checkResult struct {
		status  string
		message string
	}
	results := make(map[string]checkResult, len(accounts))
	checked, healthy := 0, 0
	for _, account := range accounts {
		if !account.Enabled {
			continue
		}
		checked++
		if err := a.onAccountCheck(c.Request.Context(), account.ID); err != nil {
			results[account.ID] = checkResult{status: "unhealthy", message: err.Error()}
		} else {
			healthy++
			results[account.ID] = checkResult{status: "healthy"}
		}
	}
	now := time.Now().UTC()
	if err := a.store.Update(func(state *PersistentState) error {
		for index := range state.Accounts {
			r, ok := results[state.Accounts[index].ID]
			if !ok {
				continue
			}
			state.Accounts[index].Status = r.status
			state.Accounts[index].StatusMessage = r.message
			state.Accounts[index].LastCheckedAt = now
			state.Accounts[index].UpdatedAt = now
		}
		return nil
	}); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"message": err.Error(), "type": "storage_error"}})
		return
	}
	c.JSON(http.StatusOK, gin.H{"checked": checked, "healthy": healthy, "unhealthy": checked - healthy})
}

func (a *API) restoreAccounts(c *gin.Context) {
	if a.onAccountRestore == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": gin.H{"message": "account restore is unavailable", "type": "runtime_error"}})
		return
	}
	accounts := a.store.Snapshot().Accounts
	restoredIDs := make(map[string]struct{})
	for _, account := range accounts {
		if !account.Enabled || !a.onAccountRestore(account.ID) {
			continue
		}
		restoredIDs[account.ID] = struct{}{}
	}
	if err := a.store.Update(func(state *PersistentState) error {
		now := time.Now().UTC()
		for index := range state.Accounts {
			if _, ok := restoredIDs[state.Accounts[index].ID]; ok {
				state.Accounts[index].Status = "ready"
				state.Accounts[index].StatusMessage = ""
				state.Accounts[index].UpdatedAt = now
			}
		}
		return nil
	}); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"message": err.Error(), "type": "storage_error"}})
		return
	}
	c.JSON(http.StatusOK, gin.H{"restored": len(restoredIDs)})
}

func (a *API) deleteBlockedAccounts(c *gin.Context) {
	deleted := 0
	if err := a.store.Update(func(state *PersistentState) error {
		kept := state.Accounts[:0]
		for _, account := range state.Accounts {
			status := strings.ToLower(account.Status)
			// Match both the legacy "ban/block" convention and the runtime
			// "unhealthy" status written by checkAccounts so that accounts
			// marked unhealthy via health checks are also eligible for removal.
			if strings.Contains(status, "ban") || strings.Contains(status, "block") ||
				status == "unhealthy" {
				deleted++
				continue
			}
			kept = append(kept, account)
		}
		state.Accounts = kept
		return nil
	}); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"message": err.Error(), "type": "storage_error"}})
		return
	}
	if deleted > 0 {
		if err := a.notifyAccountsChanged(); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"message": err.Error(), "type": "runtime_refresh_error"}})
			return
		}
	}
	c.JSON(http.StatusOK, gin.H{"deleted": deleted})
}

func (a *API) restoreAccount(c *gin.Context) {
	id := strings.TrimSpace(c.Param("id"))
	// Verify the account exists in persistent state before touching the runtime.
	found := false
	for _, acc := range a.store.Snapshot().Accounts {
		if acc.ID == id {
			found = true
			break
		}
	}
	if !found {
		c.JSON(http.StatusNotFound, gin.H{"error": gin.H{"message": "account not found", "type": "not_found"}})
		return
	}
	// Best-effort: clear the runtime cooldown if one is active. The store status
	// is updated unconditionally so the UI always reflects the operator's intent.
	if a.onAccountRestore != nil {
		a.onAccountRestore(id)
	}
	if err := a.store.Update(func(state *PersistentState) error {
		for index := range state.Accounts {
			if state.Accounts[index].ID == id {
				state.Accounts[index].Status = "ready"
				state.Accounts[index].StatusMessage = ""
				state.Accounts[index].UpdatedAt = time.Now().UTC()
				break
			}
		}
		return nil
	}); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"message": err.Error(), "type": "storage_error"}})
		return
	}
	c.JSON(http.StatusOK, gin.H{"restored": true})
}

func prepareAccountForResponse(account *Account) {
	account.SessionKey = ""
	account.Cookie = ""
	account.SessionUsageRate = 0
	if account.SessionLimit > 0 {
		account.SessionUsageRate = float64(account.SessionUsed) / float64(account.SessionLimit) * 100
	}
}

func (a *API) addAccount(c *gin.Context) {
	var request accountRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "invalid account payload", "type": "invalid_request_error"}})
		return
	}
	if request.SessionLimit < 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "session_limit cannot be negative", "type": "invalid_request_error"}})
		return
	}
	request.SessionKey = strings.TrimSpace(request.SessionKey)
	request.Cookie = strings.TrimSpace(request.Cookie)
	if request.SessionKey == "" {
		request.SessionKey = sessionKeyFromCookie(request.Cookie)
	}
	if request.SessionKey == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "session_key or a cookie containing sessionKey is required", "type": "invalid_request_error"}})
		return
	}

	now := time.Now().UTC()
	account := Account{
		ID:           utils.GenerateUUID(),
		Name:         strings.TrimSpace(request.Name),
		SessionKey:   request.SessionKey,
		Cookie:       request.Cookie,
		Enabled:      true,
		Status:       "unknown",
		SessionLimit: request.SessionLimit,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if account.Name == "" {
		account.Name = "Account " + account.ID[:8]
	}

	err := a.store.Update(func(state *PersistentState) error {
		for _, existing := range state.Accounts {
			if strings.EqualFold(strings.TrimSpace(existing.Name), account.Name) ||
				(existing.SessionKey == account.SessionKey && existing.Cookie == account.Cookie) {
				return errAccountExists
			}
		}
		state.Accounts = append(state.Accounts, account)
		return nil
	})
	if err != nil {
		status := http.StatusInternalServerError
		if err == errAccountExists {
			status = http.StatusConflict
		}
		c.JSON(status, gin.H{"error": gin.H{"message": err.Error(), "type": "account_error"}})
		return
	}
	if err := a.notifyAccountsChanged(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"message": err.Error(), "type": "runtime_refresh_error"}})
		return
	}
	prepareAccountForResponse(&account)
	c.JSON(http.StatusCreated, account)
}

func (a *API) importAccounts(c *gin.Context) {
	var request accountImportRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "content is required", "type": "invalid_request_error"}})
		return
	}

	lines := strings.Split(strings.ReplaceAll(request.Content, "\r\n", "\n"), "\n")
	imported := 0
	skipped := 0
	err := a.store.Update(func(state *PersistentState) error {
		for _, raw := range lines {
			line := strings.TrimSpace(raw)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			account := accountFromImportLine(line)
			if account.SessionKey == "" {
				skipped++
				continue
			}
			duplicate := false
			for _, existing := range state.Accounts {
				if existing.SessionKey == account.SessionKey && existing.Cookie == account.Cookie {
					duplicate = true
					break
				}
			}
			if duplicate {
				skipped++
				continue
			}
			state.Accounts = append(state.Accounts, account)
			imported++
		}
		return nil
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"message": err.Error(), "type": "storage_error"}})
		return
	}
	if imported > 0 {
		if err := a.notifyAccountsChanged(); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"message": err.Error(), "type": "runtime_refresh_error"}})
			return
		}
	}
	c.JSON(http.StatusOK, gin.H{"imported": imported, "skipped": skipped})
}

func (a *API) updateAccount(c *gin.Context) {
	id := strings.TrimSpace(c.Param("id"))
	var request accountUpdateRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "invalid account payload", "type": "invalid_request_error"}})
		return
	}
	if request.SessionLimit != nil && *request.SessionLimit < 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "session_limit cannot be negative", "type": "invalid_request_error"}})
		return
	}

	var updated Account
	found := false
	err := a.store.Update(func(state *PersistentState) error {
		for index := range state.Accounts {
			if state.Accounts[index].ID != id {
				continue
			}
			if request.Name != nil {
				name := strings.TrimSpace(*request.Name)
				if name == "" {
					return errors.New("account name cannot be empty")
				}
				state.Accounts[index].Name = name
			}
			if request.SessionKey != nil || request.Cookie != nil {
				sessionKey := state.Accounts[index].SessionKey
				cookie := state.Accounts[index].Cookie
				if request.SessionKey != nil {
					sessionKey = strings.TrimSpace(*request.SessionKey)
				}
				if request.Cookie != nil {
					cookie = strings.TrimSpace(*request.Cookie)
				}
				if sessionKey == "" {
					sessionKey = sessionKeyFromCookie(cookie)
				}
				if sessionKey == "" {
					return errors.New("session_key or a cookie containing sessionKey is required")
				}
				for otherIndex, existing := range state.Accounts {
					if otherIndex != index && existing.SessionKey == sessionKey && existing.Cookie == cookie {
						return errAccountExists
					}
				}
				state.Accounts[index].SessionKey = sessionKey
				state.Accounts[index].Cookie = cookie
			}
			if request.Enabled != nil {
				state.Accounts[index].Enabled = *request.Enabled
			}
			if request.SessionLimit != nil {
				state.Accounts[index].SessionLimit = *request.SessionLimit
			}
			state.Accounts[index].UpdatedAt = time.Now().UTC()
			updated = state.Accounts[index]
			found = true
			break
		}
		return nil
	})
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, errAccountExists) {
			status = http.StatusConflict
		}
		c.JSON(status, gin.H{"error": gin.H{"message": err.Error(), "type": "account_error"}})
		return
	}
	if !found {
		c.JSON(http.StatusNotFound, gin.H{"error": gin.H{"message": "account not found", "type": "not_found"}})
		return
	}
	if err := a.notifyAccountsChanged(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"message": err.Error(), "type": "runtime_refresh_error"}})
		return
	}
	prepareAccountForResponse(&updated)
	c.JSON(http.StatusOK, updated)
}

func (a *API) updateAccountStatus(c *gin.Context) {
	id := strings.TrimSpace(c.Param("id"))
	var request accountStatusRequest
	if err := c.ShouldBindJSON(&request); err != nil || request.Enabled == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "enabled is required", "type": "invalid_request_error"}})
		return
	}
	var updated Account
	found := false
	if err := a.store.Update(func(state *PersistentState) error {
		for index := range state.Accounts {
			if state.Accounts[index].ID == id {
				state.Accounts[index].Enabled = *request.Enabled
				state.Accounts[index].UpdatedAt = time.Now().UTC()
				updated = state.Accounts[index]
				found = true
				break
			}
		}
		return nil
	}); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"message": err.Error(), "type": "storage_error"}})
		return
	}
	if !found {
		c.JSON(http.StatusNotFound, gin.H{"error": gin.H{"message": "account not found", "type": "not_found"}})
		return
	}
	if err := a.notifyAccountsChanged(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"message": err.Error(), "type": "runtime_refresh_error"}})
		return
	}
	prepareAccountForResponse(&updated)
	c.JSON(http.StatusOK, updated)
}

func (a *API) deleteAccount(c *gin.Context) {
	id := strings.TrimSpace(c.Param("id"))
	deleted := false
	err := a.store.Update(func(state *PersistentState) error {
		for index, account := range state.Accounts {
			if account.ID == id {
				state.Accounts = append(state.Accounts[:index], state.Accounts[index+1:]...)
				deleted = true
				break
			}
		}
		return nil
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"message": err.Error(), "type": "storage_error"}})
		return
	}
	if !deleted {
		c.JSON(http.StatusNotFound, gin.H{"error": gin.H{"message": "account not found", "type": "not_found"}})
		return
	}
	if err := a.notifyAccountsChanged(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"message": err.Error(), "type": "runtime_refresh_error"}})
		return
	}
	c.Status(http.StatusNoContent)
}

func accountFromImportLine(line string) Account {
	now := time.Now().UTC()
	account := Account{
		ID:        utils.GenerateUUID(),
		Enabled:   true,
		Status:    "unknown",
		CreatedAt: now,
		UpdatedAt: now,
	}
	if strings.Contains(line, "sessionKey=") {
		account.Cookie = line
		account.SessionKey = sessionKeyFromCookie(line)
	} else {
		account.SessionKey = line
	}
	account.Name = "Account " + account.ID[:8]
	return account
}

func isValidProxyURL(u string) bool {
	return strings.HasPrefix(u, "http://") ||
		strings.HasPrefix(u, "https://") ||
		strings.HasPrefix(u, "socks5://")
}

func sessionKeyFromCookie(cookie string) string {
	for _, part := range strings.Split(cookie, ";") {
		keyValue := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(keyValue) == 2 && keyValue[0] == "sessionKey" {
			return strings.TrimSpace(keyValue[1])
		}
	}
	return ""
}

func (a *API) getSettings(c *gin.Context) {
	c.JSON(http.StatusOK, a.store.Snapshot().Settings)
}

func (a *API) updateSettings(c *gin.Context) {
	var request PanelSettings
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "invalid settings payload", "type": "invalid_request_error"}})
		return
	}
	if request.Language != "zh-CN" && request.Language != "en-US" {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "language must be zh-CN or en-US", "type": "invalid_request_error"}})
		return
	}
	if request.Theme != "light" && request.Theme != "dark" && request.Theme != "system" {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "theme must be light, dark, or system", "type": "invalid_request_error"}})
		return
	}
	if request.RoutingPolicy != "round-robin" && request.RoutingPolicy != "least-loaded" {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "routing_policy must be round-robin or least-loaded", "type": "invalid_request_error"}})
		return
	}
	if err := a.store.Update(func(state *PersistentState) error {
		state.Settings = request
		return nil
	}); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"message": err.Error(), "type": "storage_error"}})
		return
	}
	if a.onSettingsChanged != nil {
		if err := a.onSettingsChanged(request); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"message": err.Error(), "type": "runtime_refresh_error"}})
			return
		}
	}
	c.JSON(http.StatusOK, request)
}

func (a *API) getProxy(c *gin.Context) {
	c.JSON(http.StatusOK, a.store.Snapshot().Proxy)
}

func (a *API) updateProxy(c *gin.Context) {
	var request ProxyConfig
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "invalid proxy payload", "type": "invalid_request_error"}})
		return
	}
	request.URLTemplate = strings.TrimSpace(request.URLTemplate)
	if request.Enabled && request.URLTemplate == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "url_template is required when proxy is enabled", "type": "invalid_request_error"}})
		return
	}
	if request.URLTemplate != "" && !isValidProxyURL(request.URLTemplate) {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "url_template must start with http://, https://, or socks5://", "type": "invalid_request_error"}})
		return
	}
	if err := a.store.Update(func(state *PersistentState) error {
		state.Proxy = request
		return nil
	}); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"message": err.Error(), "type": "storage_error"}})
		return
	}
	if err := a.notifyAccountsChanged(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"message": err.Error(), "type": "runtime_refresh_error"}})
		return
	}
	c.JSON(http.StatusOK, request)
}

func (a *API) getRateLimit(c *gin.Context) {
	c.JSON(http.StatusOK, a.store.Snapshot().RateLimit)
}

func (a *API) updateRateLimit(c *gin.Context) {
	var request RateLimitConfig
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "invalid rate-limit payload", "type": "invalid_request_error"}})
		return
	}
	if request.Enabled && (request.RequestsPerMinute < 1 || request.Burst < 1) {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "requests_per_minute and burst must be positive", "type": "invalid_request_error"}})
		return
	}
	if err := a.store.Update(func(state *PersistentState) error {
		state.RateLimit = request
		return nil
	}); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"message": err.Error(), "type": "storage_error"}})
		return
	}
	if a.onRateLimitChanged != nil {
		if err := a.onRateLimitChanged(request); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"message": err.Error(), "type": "runtime_refresh_error"}})
			return
		}
	}
	c.JSON(http.StatusOK, request)
}

func (a *API) getKeepAlive(c *gin.Context) {
	c.JSON(http.StatusOK, a.store.Snapshot().KeepAlive)
}

func (a *API) updateKeepAlive(c *gin.Context) {
	var request KeepAliveConfig
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "invalid keep-alive payload", "type": "invalid_request_error"}})
		return
	}
	if request.IntervalMinutes < 1 || request.IntervalMinutes > 1440 {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "interval_minutes must be between 1 and 1440", "type": "invalid_request_error"}})
		return
	}
	if request.TimeoutSeconds < 1 || request.TimeoutSeconds > 300 {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "timeout_seconds must be between 1 and 300", "type": "invalid_request_error"}})
		return
	}
	if err := a.store.Update(func(state *PersistentState) error {
		state.KeepAlive = request
		return nil
	}); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"message": err.Error(), "type": "storage_error"}})
		return
	}
	if a.onKeepAliveChanged != nil {
		if err := a.onKeepAliveChanged(request); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"message": err.Error(), "type": "runtime_refresh_error"}})
			return
		}
	}
	c.JSON(http.StatusOK, request)
}

type updateMasterKeyRequest struct {
	Key     string `json:"key"`
	Enabled *bool  `json:"enabled"`
}

func (a *API) getMasterKey(c *gin.Context) {
	config := a.store.Snapshot().MasterKey
	config.KeyHash = ""
	c.JSON(http.StatusOK, config)
}

func (a *API) updateMasterKey(c *gin.Context) {
	var request updateMasterKeyRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "invalid master-key payload", "type": "invalid_request_error"}})
		return
	}

	rawKey := strings.TrimSpace(request.Key)
	if rawKey != "" && (len(rawKey) < 16 || len(rawKey) > 256) {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "master key must contain between 16 and 256 characters", "type": "invalid_request_error"}})
		return
	}
	current := a.store.Snapshot().MasterKey
	if rawKey == "" && current.KeyHash == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "key is required when configuring the master key for the first time", "type": "invalid_request_error"}})
		return
	}

	if err := a.store.Update(func(state *PersistentState) error {
		if rawKey != "" {
			digest := sha256.Sum256([]byte(rawKey))
			state.MasterKey.KeyHash = hex.EncodeToString(digest[:])
			prefixLength := 12
			if len(rawKey) < prefixLength {
				prefixLength = len(rawKey)
			}
			state.MasterKey.KeyPrefix = rawKey[:prefixLength]
			state.MasterKey.UpdatedAt = time.Now().UTC()
			state.MasterKey.LastUsedAt = nil
		}
		if request.Enabled != nil {
			state.MasterKey.Enabled = *request.Enabled
		} else if rawKey != "" {
			state.MasterKey.Enabled = true
		}
		return nil
	}); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"message": err.Error(), "type": "storage_error"}})
		return
	}

	config := a.store.Snapshot().MasterKey
	config.KeyHash = ""
	c.JSON(http.StatusOK, config)
}

func (a *API) deleteMasterKey(c *gin.Context) {
	if err := a.store.Update(func(state *PersistentState) error {
		state.MasterKey = MasterKeyConfig{}
		return nil
	}); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"message": err.Error(), "type": "storage_error"}})
		return
	}
	c.Status(http.StatusNoContent)
}

type createAPIKeyRequest struct {
	Name string `json:"name" binding:"required"`
}

type updateAPIKeyRequest struct {
	Name    *string `json:"name"`
	Enabled *bool   `json:"enabled"`
}

func (a *API) listAPIKeys(c *gin.Context) {
	keys := a.store.Snapshot().APIKeys
	for index := range keys {
		keys[index].KeyHash = ""
	}
	c.JSON(http.StatusOK, gin.H{"api_keys": keys})
}

func (a *API) createAPIKey(c *gin.Context) {
	var request createAPIKeyRequest
	if err := c.ShouldBindJSON(&request); err != nil || strings.TrimSpace(request.Name) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "name is required", "type": "invalid_request_error"}})
		return
	}
	name := strings.TrimSpace(request.Name)
	if len(name) > 100 {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "name must not exceed 100 characters", "type": "invalid_request_error"}})
		return
	}
	rawKey := "c2a_" + strings.ReplaceAll(utils.GenerateUUID(), "-", "") + strings.ReplaceAll(utils.GenerateUUID(), "-", "")
	digest := sha256.Sum256([]byte(rawKey))
	now := time.Now().UTC()
	key := APIKey{
		ID:        utils.GenerateUUID(),
		Name:      name,
		KeyHash:   hex.EncodeToString(digest[:]),
		KeyPrefix: rawKey[:12],
		Enabled:   true,
		CreatedAt: now,
	}
	if err := a.store.Update(func(state *PersistentState) error {
		state.APIKeys = append(state.APIKeys, key)
		return nil
	}); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"message": err.Error(), "type": "storage_error"}})
		return
	}
	key.KeyHash = ""
	c.JSON(http.StatusCreated, gin.H{"api_key": key, "key": rawKey})
}

func (a *API) updateAPIKey(c *gin.Context) {
	id := strings.TrimSpace(c.Param("id"))
	if id == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "API key id is required", "type": "invalid_request_error"}})
		return
	}

	var request updateAPIKeyRequest
	if err := c.ShouldBindJSON(&request); err != nil || (request.Name == nil && request.Enabled == nil) {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "name or enabled is required", "type": "invalid_request_error"}})
		return
	}

	var updated APIKey
	found := false
	if err := a.store.Update(func(state *PersistentState) error {
		for index := range state.APIKeys {
			if state.APIKeys[index].ID != id {
				continue
			}
			if request.Name != nil {
				name := strings.TrimSpace(*request.Name)
				if name == "" {
					return errors.New("API key name cannot be empty")
				}
				if len(name) > 100 {
					return errors.New("API key name must not exceed 100 characters")
				}
				state.APIKeys[index].Name = name
			}
			if request.Enabled != nil {
				state.APIKeys[index].Enabled = *request.Enabled
			}
			updated = state.APIKeys[index]
			found = true
			break
		}
		return nil
	}); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": err.Error(), "type": "invalid_request_error"}})
		return
	}
	if !found {
		c.JSON(http.StatusNotFound, gin.H{"error": gin.H{"message": "API key not found", "type": "not_found"}})
		return
	}
	updated.KeyHash = ""
	c.JSON(http.StatusOK, updated)
}

func (a *API) deleteAPIKey(c *gin.Context) {
	id := strings.TrimSpace(c.Param("id"))
	deleted := false
	if err := a.store.Update(func(state *PersistentState) error {
		for index, key := range state.APIKeys {
			if key.ID == id {
				state.APIKeys = append(state.APIKeys[:index], state.APIKeys[index+1:]...)
				deleted = true
				break
			}
		}
		return nil
	}); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"message": err.Error(), "type": "storage_error"}})
		return
	}
	if !deleted {
		c.JSON(http.StatusNotFound, gin.H{"error": gin.H{"message": "API key not found", "type": "not_found"}})
		return
	}
	c.Status(http.StatusNoContent)
}

// -------- Model Mappings --------

type createModelMappingRequest struct {
	From    string `json:"from" binding:"required"`
	To      string `json:"to" binding:"required"`
	Enabled *bool  `json:"enabled"`
}

type updateModelMappingRequest struct {
	From    *string `json:"from"`
	To      *string `json:"to"`
	Enabled *bool   `json:"enabled"`
}

func (a *API) listModelMappings(c *gin.Context) {
	mappings := a.store.Snapshot().ModelMappings
	if mappings == nil {
		mappings = []ModelMapping{}
	}
	c.JSON(http.StatusOK, gin.H{"model_mappings": mappings})
}

func (a *API) createModelMapping(c *gin.Context) {
	var request createModelMappingRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "from and to are required", "type": "invalid_request_error"}})
		return
	}
	request.From = strings.TrimSpace(request.From)
	request.To = strings.TrimSpace(request.To)
	if request.From == "" || request.To == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "from and to must not be empty", "type": "invalid_request_error"}})
		return
	}

	enabled := true
	if request.Enabled != nil {
		enabled = *request.Enabled
	}
	now := time.Now().UTC()
	mapping := ModelMapping{
		ID:        utils.GenerateUUID(),
		From:      request.From,
		To:        request.To,
		Enabled:   enabled,
		CreatedAt: now,
		UpdatedAt: now,
	}

	if err := a.store.Update(func(state *PersistentState) error {
		for _, existing := range state.ModelMappings {
			if strings.EqualFold(existing.From, mapping.From) {
				return errors.New("a mapping for model '" + mapping.From + "' already exists")
			}
		}
		state.ModelMappings = append(state.ModelMappings, mapping)
		return nil
	}); err != nil {
		status := http.StatusInternalServerError
		if strings.Contains(err.Error(), "already exists") {
			status = http.StatusConflict
		}
		c.JSON(status, gin.H{"error": gin.H{"message": err.Error(), "type": "mapping_error"}})
		return
	}
	if err := a.notifyModelMappingsChanged(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"message": err.Error(), "type": "runtime_refresh_error"}})
		return
	}
	c.JSON(http.StatusCreated, mapping)
}

func (a *API) updateModelMapping(c *gin.Context) {
	id := strings.TrimSpace(c.Param("id"))
	var request updateModelMappingRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "invalid model mapping payload", "type": "invalid_request_error"}})
		return
	}

	var updated ModelMapping
	found := false
	if err := a.store.Update(func(state *PersistentState) error {
		for index := range state.ModelMappings {
			if state.ModelMappings[index].ID != id {
				continue
			}
			if request.From != nil {
				from := strings.TrimSpace(*request.From)
				if from == "" {
					return errors.New("from must not be empty")
				}
				// Check for duplicate from value among other mappings.
				for otherIndex, m := range state.ModelMappings {
					if otherIndex != index && strings.EqualFold(m.From, from) {
						return errors.New("a mapping for model '" + from + "' already exists")
					}
				}
				state.ModelMappings[index].From = from
			}
			if request.To != nil {
				to := strings.TrimSpace(*request.To)
				if to == "" {
					return errors.New("to must not be empty")
				}
				state.ModelMappings[index].To = to
			}
			if request.Enabled != nil {
				state.ModelMappings[index].Enabled = *request.Enabled
			}
			state.ModelMappings[index].UpdatedAt = time.Now().UTC()
			updated = state.ModelMappings[index]
			found = true
			break
		}
		return nil
	}); err != nil {
		status := http.StatusBadRequest
		if strings.Contains(err.Error(), "already exists") {
			status = http.StatusConflict
		}
		c.JSON(status, gin.H{"error": gin.H{"message": err.Error(), "type": "mapping_error"}})
		return
	}
	if !found {
		c.JSON(http.StatusNotFound, gin.H{"error": gin.H{"message": "model mapping not found", "type": "not_found"}})
		return
	}
	if err := a.notifyModelMappingsChanged(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"message": err.Error(), "type": "runtime_refresh_error"}})
		return
	}
	c.JSON(http.StatusOK, updated)
}

func (a *API) deleteModelMapping(c *gin.Context) {
	id := strings.TrimSpace(c.Param("id"))
	deleted := false
	if err := a.store.Update(func(state *PersistentState) error {
		for index, m := range state.ModelMappings {
			if m.ID == id {
				state.ModelMappings = append(state.ModelMappings[:index], state.ModelMappings[index+1:]...)
				deleted = true
				break
			}
		}
		return nil
	}); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"message": err.Error(), "type": "storage_error"}})
		return
	}
	if !deleted {
		c.JSON(http.StatusNotFound, gin.H{"error": gin.H{"message": "model mapping not found", "type": "not_found"}})
		return
	}
	if err := a.notifyModelMappingsChanged(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"message": err.Error(), "type": "runtime_refresh_error"}})
		return
	}
	c.Status(http.StatusNoContent)
}
