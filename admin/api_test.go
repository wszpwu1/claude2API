package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestAccountManagementLifecycle(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store, err := NewStore(filepath.Join(t.TempDir(), "admin.json"), "test-password")
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	api := NewAPI(store, NewAuthManager(store), NewMetrics())
	refreshes := 0
	api.SetAccountsChangedHandler(func(accounts []Account) error {
		refreshes++
		return nil
	})

	router := gin.New()
	router.POST("/accounts", api.addAccount)
	router.POST("/accounts/import", api.importAccounts)
	router.PUT("/accounts/:id", api.updateAccount)
	router.PATCH("/accounts/:id/status", api.updateAccountStatus)
	router.DELETE("/accounts/:id", api.deleteAccount)

	created := performJSONRequest(t, router, http.MethodPost, "/accounts", map[string]interface{}{
		"name":          "Primary",
		"session_key":   "session-primary",
		"session_limit": 10,
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("add account status = %d, body = %s", created.Code, created.Body.String())
	}
	var account Account
	if err := json.Unmarshal(created.Body.Bytes(), &account); err != nil {
		t.Fatalf("decode created account: %v", err)
	}
	if account.ID == "" || account.Name != "Primary" || !account.Enabled {
		t.Fatalf("unexpected created account: %#v", account)
	}
	if account.SessionKey != "" || account.Cookie != "" {
		t.Fatal("account secrets must not be returned")
	}

	imported := performJSONRequest(t, router, http.MethodPost, "/accounts/import", map[string]interface{}{
		"content": "session-secondary\nsession-primary\n",
	})
	if imported.Code != http.StatusOK {
		t.Fatalf("import accounts status = %d, body = %s", imported.Code, imported.Body.String())
	}
	var importResult struct {
		Imported int `json:"imported"`
		Skipped  int `json:"skipped"`
	}
	if err := json.Unmarshal(imported.Body.Bytes(), &importResult); err != nil {
		t.Fatalf("decode import result: %v", err)
	}
	if importResult.Imported != 1 || importResult.Skipped != 1 {
		t.Fatalf("unexpected import result: %#v", importResult)
	}

	updated := performJSONRequest(t, router, http.MethodPut, "/accounts/"+account.ID, map[string]interface{}{
		"name":          "Primary Updated",
		"session_key":   "session-primary-updated",
		"session_limit": 20,
	})
	if updated.Code != http.StatusOK {
		t.Fatalf("update account status = %d, body = %s", updated.Code, updated.Body.String())
	}
	var updatedAccount Account
	if err := json.Unmarshal(updated.Body.Bytes(), &updatedAccount); err != nil {
		t.Fatalf("decode updated account: %v", err)
	}
	if updatedAccount.Name != "Primary Updated" || updatedAccount.SessionLimit != 20 {
		t.Fatalf("unexpected updated account: %#v", updatedAccount)
	}

	disabled := performJSONRequest(t, router, http.MethodPatch, "/accounts/"+account.ID+"/status", map[string]interface{}{
		"enabled": false,
	})
	if disabled.Code != http.StatusOK {
		t.Fatalf("disable account status = %d, body = %s", disabled.Code, disabled.Body.String())
	}
	var disabledAccount Account
	if err := json.Unmarshal(disabled.Body.Bytes(), &disabledAccount); err != nil {
		t.Fatalf("decode disabled account: %v", err)
	}
	if disabledAccount.Enabled {
		t.Fatal("account should be disabled")
	}

	deleted := performJSONRequest(t, router, http.MethodDelete, "/accounts/"+account.ID, nil)
	if deleted.Code != http.StatusNoContent {
		t.Fatalf("delete account status = %d, body = %s", deleted.Code, deleted.Body.String())
	}
	if refreshes != 5 {
		t.Fatalf("runtime refresh count = %d, want 5", refreshes)
	}
	for _, remaining := range store.Snapshot().Accounts {
		if remaining.ID == account.ID {
			t.Fatal("deleted account remains in persistent state")
		}
	}
}

func TestAccountCheckAndRestoreEndpoints(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store, err := NewStore(filepath.Join(t.TempDir(), "admin.json"), "test-password")
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if err := store.Update(func(state *PersistentState) error {
		state.Accounts = []Account{{ID: "account-1", Name: "Primary", SessionKey: "session-1", Enabled: true, Status: "unknown"}}
		return nil
	}); err != nil {
		t.Fatalf("seed account: %v", err)
	}

	api := NewAPI(store, NewAuthManager(store), NewMetrics())
	checked := 0
	api.SetAccountCheckHandler(func(_ context.Context, accountID string) error {
		checked++
		if accountID != "account-1" {
			t.Fatalf("checked account = %q, want account-1", accountID)
		}
		return nil
	})
	restored := 0
	api.SetAccountRestoreHandler(func(accountID string) bool {
		restored++
		return accountID == "account-1"
	})

	router := gin.New()
	router.POST("/accounts/check", api.checkAccounts)
	router.POST("/accounts/:id/restore", api.restoreAccount)

	checkResponse := performJSONRequest(t, router, http.MethodPost, "/accounts/check", nil)
	if checkResponse.Code != http.StatusOK {
		t.Fatalf("check accounts status = %d, body = %s", checkResponse.Code, checkResponse.Body.String())
	}
	if checked != 1 {
		t.Fatalf("health-check count = %d, want 1", checked)
	}
	account := store.Snapshot().Accounts[0]
	if account.Status != "healthy" || account.LastCheckedAt.IsZero() {
		t.Fatalf("unexpected checked account state: %#v", account)
	}

	restoreResponse := performJSONRequest(t, router, http.MethodPost, "/accounts/account-1/restore", nil)
	if restoreResponse.Code != http.StatusOK {
		t.Fatalf("restore account status = %d, body = %s", restoreResponse.Code, restoreResponse.Body.String())
	}
	if restored != 1 {
		t.Fatalf("restore count = %d, want 1", restored)
	}
	if status := store.Snapshot().Accounts[0].Status; status != "ready" {
		t.Fatalf("restored account status = %q, want ready", status)
	}
}

func TestAccountListIncludesRuntimeCooldownDeadline(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store, err := NewStore(filepath.Join(t.TempDir(), "admin.json"), "test-password")
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if err := store.Update(func(state *PersistentState) error {
		state.Accounts = []Account{{ID: "account-1", Name: "Primary", SessionKey: "session-1", Enabled: true, Status: "ready"}}
		return nil
	}); err != nil {
		t.Fatalf("seed account: %v", err)
	}

	deadline := time.Now().UTC().Add(5 * time.Minute).Truncate(time.Nanosecond)
	api := NewAPI(store, NewAuthManager(store), NewMetrics())
	api.SetAccountCooldownsHandler(func() map[string]time.Time {
		return map[string]time.Time{"account-1": deadline}
	})

	router := gin.New()
	router.GET("/accounts", api.listAccounts)
	response := performJSONRequest(t, router, http.MethodGet, "/accounts", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("list accounts status = %d, body = %s", response.Code, response.Body.String())
	}

	var result struct {
		Accounts []Account `json:"accounts"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode accounts response: %v", err)
	}
	if len(result.Accounts) != 1 {
		t.Fatalf("account count = %d, want 1", len(result.Accounts))
	}
	if !result.Accounts[0].CooldownUntil.Equal(deadline) {
		t.Fatalf("cooldown deadline = %v, want %v", result.Accounts[0].CooldownUntil, deadline)
	}
	if result.Accounts[0].SessionKey != "" || result.Accounts[0].Cookie != "" {
		t.Fatal("account secrets must not be returned with cooldown state")
	}
}

func TestRuntimeSettingsAndProxyRefreshCallbacks(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store, err := NewStore(filepath.Join(t.TempDir(), "admin.json"), "test-password")
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	api := NewAPI(store, NewAuthManager(store), NewMetrics())

	settingsRefreshes := 0
	api.SetSettingsChangedHandler(func(settings PanelSettings) error {
		settingsRefreshes++
		if settings.RoutingPolicy != "round-robin" {
			t.Fatalf("routing policy = %q, want round-robin", settings.RoutingPolicy)
		}
		return nil
	})
	accountRefreshes := 0
	api.SetAccountsChangedHandler(func(accounts []Account) error {
		accountRefreshes++
		return nil
	})

	router := gin.New()
	router.PUT("/settings", api.updateSettings)
	router.PUT("/proxy", api.updateProxy)

	settings := performJSONRequest(t, router, http.MethodPut, "/settings", map[string]interface{}{
		"language":       "zh-CN",
		"theme":          "system",
		"routing_policy": "round-robin",
	})
	if settings.Code != http.StatusOK {
		t.Fatalf("update settings status = %d, body = %s", settings.Code, settings.Body.String())
	}
	if settingsRefreshes != 1 {
		t.Fatalf("settings refresh count = %d, want 1", settingsRefreshes)
	}

	proxy := performJSONRequest(t, router, http.MethodPut, "/proxy", map[string]interface{}{
		"enabled":      true,
		"url_template": "http://proxy.example/{sid}",
	})
	if proxy.Code != http.StatusOK {
		t.Fatalf("update proxy status = %d, body = %s", proxy.Code, proxy.Body.String())
	}
	if accountRefreshes != 1 {
		t.Fatalf("account refresh count after proxy update = %d, want 1", accountRefreshes)
	}
}

func TestMetricsEndpointReturnsRuntimeSnapshot(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store, err := NewStore(filepath.Join(t.TempDir(), "admin.json"), "test-password")
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	metrics := NewMetrics()
	finishSuccess := metrics.Begin()
	finishFailure := metrics.Begin()
	finishSuccess(true)
	finishFailure(false)

	api := NewAPI(store, NewAuthManager(store), metrics)
	router := gin.New()
	router.GET("/metrics", api.getMetrics)

	response := performJSONRequest(t, router, http.MethodGet, "/metrics", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("metrics status = %d, body = %s", response.Code, response.Body.String())
	}
	var snapshot MetricsSnapshot
	if err := json.Unmarshal(response.Body.Bytes(), &snapshot); err != nil {
		t.Fatalf("decode metrics snapshot: %v", err)
	}
	if snapshot.Requests != 2 || snapshot.Successes != 1 || snapshot.Failures != 1 {
		t.Fatalf("unexpected metrics snapshot: %#v", snapshot)
	}
	if snapshot.ActiveRequests != 0 || snapshot.SuccessRate != 50 {
		t.Fatalf("unexpected runtime metrics: %#v", snapshot)
	}
}

func TestAccountResponseIncludesSessionUsageRate(t *testing.T) {
	account := Account{
		SessionKey:   "secret",
		Cookie:       "cookie",
		SessionUsed:  5,
		SessionLimit: 20,
	}

	prepareAccountForResponse(&account)

	if account.SessionKey != "" || account.Cookie != "" {
		t.Fatal("account secrets must not be returned")
	}
	if account.SessionUsageRate != 25 {
		t.Fatalf("session usage rate = %f, want 25", account.SessionUsageRate)
	}
}

func TestUsageEndpointReturnsOnlyRecentSevenDays(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store, err := NewStore(filepath.Join(t.TempDir(), "admin.json"), "test-password")
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	now := time.Now().UTC()
	if err := store.Update(func(state *PersistentState) error {
		state.DailyUsage = []DailyUsage{
			{Date: now.AddDate(0, 0, -7).Format("2006-01-02"), AccountID: "account-1", Requests: 1},
			{Date: now.AddDate(0, 0, -6).Format("2006-01-02"), AccountID: "account-1", Requests: 2},
			{Date: now.Format("2006-01-02"), AccountID: "account-1", Requests: 3},
		}
		return nil
	}); err != nil {
		t.Fatalf("seed daily usage: %v", err)
	}

	api := NewAPI(store, NewAuthManager(store), NewMetrics())
	router := gin.New()
	router.GET("/usage", api.getUsage)
	response := performJSONRequest(t, router, http.MethodGet, "/usage", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("usage status = %d, body = %s", response.Code, response.Body.String())
	}

	var result struct {
		DailyUsage []DailyUsage `json:"daily_usage"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode usage response: %v", err)
	}
	if len(result.DailyUsage) != 2 {
		t.Fatalf("daily usage entries = %d, want 2", len(result.DailyUsage))
	}
	if result.DailyUsage[0].Requests != 2 || result.DailyUsage[1].Requests != 3 {
		t.Fatalf("unexpected usage response: %#v", result.DailyUsage)
	}
}

func TestAPIKeyManagementLifecycle(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store, err := NewStore(filepath.Join(t.TempDir(), "admin.json"), "test-password")
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	api := NewAPI(store, NewAuthManager(store), NewMetrics())
	router := gin.New()
	router.GET("/api-keys", api.listAPIKeys)
	router.POST("/api-keys", api.createAPIKey)
	router.PUT("/api-keys/:id", api.updateAPIKey)
	router.DELETE("/api-keys/:id", api.deleteAPIKey)

	created := performJSONRequest(t, router, http.MethodPost, "/api-keys", map[string]interface{}{"name": "Primary"})
	if created.Code != http.StatusCreated {
		t.Fatalf("create API key status = %d, body = %s", created.Code, created.Body.String())
	}
	var createResult struct {
		APIKey APIKey `json:"api_key"`
		Key    string `json:"key"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &createResult); err != nil {
		t.Fatalf("decode created API key: %v", err)
	}
	if createResult.APIKey.ID == "" || createResult.Key == "" || createResult.APIKey.KeyHash != "" {
		t.Fatalf("unexpected created API key: %#v", createResult)
	}

	updated := performJSONRequest(t, router, http.MethodPut, "/api-keys/"+createResult.APIKey.ID, map[string]interface{}{
		"name":    "Renamed",
		"enabled": false,
	})
	if updated.Code != http.StatusOK {
		t.Fatalf("update API key status = %d, body = %s", updated.Code, updated.Body.String())
	}
	var updatedKey APIKey
	if err := json.Unmarshal(updated.Body.Bytes(), &updatedKey); err != nil {
		t.Fatalf("decode updated API key: %v", err)
	}
	if updatedKey.Name != "Renamed" || updatedKey.Enabled || updatedKey.KeyHash != "" {
		t.Fatalf("unexpected updated API key: %#v", updatedKey)
	}

	listed := performJSONRequest(t, router, http.MethodGet, "/api-keys", nil)
	if listed.Code != http.StatusOK {
		t.Fatalf("list API keys status = %d, body = %s", listed.Code, listed.Body.String())
	}
	if bytes.Contains(listed.Body.Bytes(), []byte(store.Snapshot().APIKeys[0].KeyHash)) {
		t.Fatal("API key hash must not be returned")
	}

	deleted := performJSONRequest(t, router, http.MethodDelete, "/api-keys/"+createResult.APIKey.ID, nil)
	if deleted.Code != http.StatusNoContent {
		t.Fatalf("delete API key status = %d, body = %s", deleted.Code, deleted.Body.String())
	}
	if len(store.Snapshot().APIKeys) != 0 {
		t.Fatal("deleted API key remains in persistent state")
	}
}

func TestChangePasswordValidationAndSessionRevocation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store, err := NewStore(filepath.Join(t.TempDir(), "admin.json"), "old-password")
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	auth := NewAuthManager(store)
	token, _, ok := auth.Login("old-password")
	if !ok {
		t.Fatal("initial administrator login failed")
	}
	api := NewAPI(store, auth, NewMetrics())
	router := gin.New()
	router.PUT("/password", api.changePassword)

	response := performJSONRequest(t, router, http.MethodPut, "/password", map[string]interface{}{
		"current_password": "old-password",
		"new_password":     "new-password-123",
	})
	if response.Code != http.StatusNoContent {
		t.Fatalf("change password status = %d, body = %s", response.Code, response.Body.String())
	}
	if store.VerifyPassword("old-password") || !store.VerifyPassword("new-password-123") {
		t.Fatal("administrator password was not changed correctly")
	}
	if auth.Validate(token) {
		t.Fatal("existing administrator sessions must be revoked after password change")
	}
	if err := store.ChangePassword("new-password-123", "new-password-123"); err == nil {
		t.Fatal("reusing the current password must be rejected")
	}
}

func TestAdminConsoleRoutes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	RegisterWebRoutes(router)

	for _, path := range []string{"/admin", "/admin/"} {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)

		if response.Code != http.StatusOK {
			t.Fatalf("GET %s status = %d, body = %s", path, response.Code, response.Body.String())
		}
		if contentType := response.Header().Get("Content-Type"); contentType != "text/html; charset=utf-8" {
			t.Fatalf("GET %s content type = %q", path, contentType)
		}
		body := response.Body.Bytes()
		if !bytes.Contains(body, []byte("Claude2API 管理控制台")) {
			t.Fatalf("GET %s does not contain the console title", path)
		}
		if !bytes.Contains(body, []byte("/admin/api")) {
			t.Fatalf("GET %s does not contain the management API base path", path)
		}
	}
}

func performJSONRequest(t *testing.T, handler http.Handler, method, path string, payload interface{}) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	if payload != nil {
		if err := json.NewEncoder(&body).Encode(payload); err != nil {
			t.Fatalf("encode request payload: %v", err)
		}
	}
	request := httptest.NewRequest(method, path, &body)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}
