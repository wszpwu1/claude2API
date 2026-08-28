package admin

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestRuntimeMiddlewareCollectsRequestMetrics(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store, err := NewStore(filepath.Join(t.TempDir(), "admin.json"), "test-password")
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	metrics := NewMetrics()
	middleware := NewRuntimeMiddleware(store, metrics)

	router := gin.New()
	router.Use(middleware.Handler())
	router.GET("/success", func(c *gin.Context) {
		c.Status(http.StatusOK)
	})
	router.GET("/failure", func(c *gin.Context) {
		c.Status(http.StatusInternalServerError)
	})

	for _, path := range []string{"/success", "/failure"} {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
	}

	snapshot := metrics.Snapshot()
	if snapshot.Requests != 2 {
		t.Fatalf("requests = %d, want 2", snapshot.Requests)
	}
	if snapshot.Successes != 1 || snapshot.Failures != 1 {
		t.Fatalf("successes/failures = %d/%d, want 1/1", snapshot.Successes, snapshot.Failures)
	}
	if snapshot.ActiveRequests != 0 {
		t.Fatalf("active requests = %d, want 0", snapshot.ActiveRequests)
	}
	if snapshot.SuccessRate != 50 {
		t.Fatalf("success rate = %f, want 50", snapshot.SuccessRate)
	}
}

func TestRuntimeMiddlewareDoesNotCountRejectedRequests(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store, err := NewStore(filepath.Join(t.TempDir(), "admin.json"), "test-password")
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if err := store.Update(func(state *PersistentState) error {
		state.RateLimit = RateLimitConfig{Enabled: true, RequestsPerMinute: 1, Burst: 1}
		return nil
	}); err != nil {
		t.Fatalf("enable rate limit: %v", err)
	}

	metrics := NewMetrics()
	middleware := NewRuntimeMiddleware(store, metrics)
	router := gin.New()
	router.Use(middleware.Handler())
	router.GET("/test", func(c *gin.Context) {
		c.Status(http.StatusOK)
	})

	first := httptest.NewRecorder()
	router.ServeHTTP(first, httptest.NewRequest(http.MethodGet, "/test", nil))
	second := httptest.NewRecorder()
	router.ServeHTTP(second, httptest.NewRequest(http.MethodGet, "/test", nil))

	if first.Code != http.StatusOK {
		t.Fatalf("first status = %d, want %d", first.Code, http.StatusOK)
	}
	if second.Code != http.StatusTooManyRequests {
		t.Fatalf("second status = %d, want %d", second.Code, http.StatusTooManyRequests)
	}
	if snapshot := metrics.Snapshot(); snapshot.Requests != 1 {
		t.Fatalf("counted requests = %d, want 1", snapshot.Requests)
	}
}

func TestRuntimeMiddlewareRecordsAccountAndDailyUsage(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store, err := NewStore(filepath.Join(t.TempDir(), "admin.json"), "test-password")
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if err := store.Update(func(state *PersistentState) error {
		state.Accounts = append(state.Accounts, Account{ID: "account-1", SessionLimit: 10})
		return nil
	}); err != nil {
		t.Fatalf("add account: %v", err)
	}

	middleware := NewRuntimeMiddleware(store, NewMetrics())
	router := gin.New()
	router.Use(middleware.Handler())
	router.GET("/success", func(c *gin.Context) {
		c.Set("accountID", "account-1")
		c.Status(http.StatusOK)
	})
	router.GET("/failure", func(c *gin.Context) {
		c.Set("accountID", "account-1")
		c.Status(http.StatusBadGateway)
	})

	for _, path := range []string{"/success", "/failure"} {
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
	}

	state := store.Snapshot()
	account := state.Accounts[0]
	if account.RequestCount != 2 || account.SessionUsed != 2 {
		t.Fatalf("request/session usage = %d/%d, want 2/2", account.RequestCount, account.SessionUsed)
	}
	if account.SuccessCount != 1 || account.FailureCount != 1 {
		t.Fatalf("success/failure count = %d/%d, want 1/1", account.SuccessCount, account.FailureCount)
	}
	if len(state.DailyUsage) != 1 {
		t.Fatalf("daily usage entries = %d, want 1", len(state.DailyUsage))
	}
	usage := state.DailyUsage[0]
	if usage.AccountID != "account-1" || usage.Requests != 2 || usage.Successes != 1 || usage.Failures != 1 {
		t.Fatalf("unexpected daily usage: %#v", usage)
	}
}
