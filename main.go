package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"claude2api/admin"
	"claude2api/config"
	"claude2api/handlers"
	"claude2api/middleware"

	"github.com/gin-gonic/gin"
)

func main() {
	cfg := config.New()

	if os.Getenv("GIN_MODE") == "" {
		gin.SetMode(gin.ReleaseMode)
	}

	r := gin.New()
	r.Use(gin.Recovery())
	r.Use(gin.Logger())

	// Health check
	r.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	adminStore, err := admin.NewStore(cfg.AdminDataFile, cfg.AdminInitialPassword)
	if err != nil {
		log.Fatalf("initialize admin store: %v", err)
	}
	managedState := adminStore.Snapshot()
	for _, account := range managedState.Accounts {
		if !account.Enabled {
			continue
		}
		proxyURL := ""
		if managedState.Proxy.Enabled {
			proxyURL = managedState.Proxy.URLTemplate
		}
		cfg.Accounts = append(cfg.Accounts, config.Account{
			ID:           account.ID,
			SessionKey:   account.SessionKey,
			Cookie:       account.Cookie,
			ProxyURL:     proxyURL,
			SessionLimit: account.SessionLimit,
			SessionUsed:  account.SessionUsed,
		})
	}
	adminAuth := admin.NewAuthManager(adminStore)
	adminMetrics := admin.NewMetrics()
	adminAPI := admin.NewAPI(adminStore, adminAuth, adminMetrics)
	adminAPI.RegisterRoutes(r)
	admin.RegisterWebRoutes(r)
	adminRuntime := admin.NewRuntimeMiddleware(adminStore, adminMetrics)

	h := handlers.NewHandler(cfg)
	keepAliveWorker := admin.NewKeepAliveWorker(adminStore, h.CheckAccount)
	keepAliveWorker.Start(context.Background())
	defer keepAliveWorker.Stop()
	h.SetRoutingPolicy(managedState.Settings.RoutingPolicy)
	adminAPI.SetAccountsChangedHandler(func(accounts []admin.Account) error {
		state := adminStore.Snapshot()
		runtimeAccounts := make([]config.Account, 0, len(accounts))
		for _, account := range accounts {
			if !account.Enabled {
				continue
			}
			proxyURL := ""
			if state.Proxy.Enabled {
				proxyURL = state.Proxy.URLTemplate
			}
			// Seed session usage from the runtime pool so the limit check in
			// available() starts from the correct baseline after a hot reload.
			var sessionUsed int64
			if stats, ok := h.AccountStats(account.ID); ok {
				sessionUsed = stats.SessionUsed
			} else {
				sessionUsed = account.SessionUsed
			}
			runtimeAccounts = append(runtimeAccounts, config.Account{
				ID:           account.ID,
				SessionKey:   account.SessionKey,
				Cookie:       account.Cookie,
				ProxyURL:     proxyURL,
				SessionLimit: account.SessionLimit,
				SessionUsed:  sessionUsed,
			})
		}
		return h.ReplaceAccounts(runtimeAccounts)
	})
	adminAPI.SetSettingsChangedHandler(func(settings admin.PanelSettings) error {
		h.SetRoutingPolicy(settings.RoutingPolicy)
		return nil
	})
	adminAPI.SetRateLimitChangedHandler(adminRuntime.SetRateLimit)
	adminAPI.SetKeepAliveChangedHandler(keepAliveWorker.SetConfig)
	adminAPI.SetAccountCheckHandler(h.CheckAccount)
	adminAPI.SetAccountRestoreHandler(h.RestoreAccount)
	adminAPI.SetAccountCooldownsHandler(h.AccountCooldowns)
	adminAPI.SetAccountStatsHandler(func(accountID string) (admin.AccountRuntimeStats, bool) {
		stats, ok := h.AccountStats(accountID)
		if !ok {
			return admin.AccountRuntimeStats{}, false
		}
		return admin.AccountRuntimeStats{
			ActiveRequests: stats.ActiveRequests,
			SessionUsed:    stats.SessionUsed,
		}, true
	})
	adminAPI.SetModelMappingsChangedHandler(func(mappings []admin.ModelMapping) error {
		m := make(map[string]string, len(mappings))
		for _, mapping := range mappings {
			if mapping.Enabled {
				m[mapping.From] = mapping.To
			}
		}
		h.SetModelMappings(m)
		return nil
	})
	// Load persisted model mappings into the runtime handler on startup.
	{
		m := make(map[string]string)
		for _, mapping := range managedState.ModelMappings {
			if mapping.Enabled {
				m[mapping.From] = mapping.To
			}
		}
		h.SetModelMappings(m)
	}

	// OpenAI-compatible endpoints
	v1 := r.Group("/v1")
	v1.Use(adminRuntime.Handler())
	v1.Use(middleware.BrowserAuthDynamic(cfg.SessionKey, cfg.ClaudeCookie, h.HasConfiguredAccounts))
	{
		v1.GET("/models", h.ListModels)
		v1.POST("/chat/completions", h.ChatCompletion)
		v1.POST("/messages", h.AnthropicMessages)
		v1.POST("/responses", h.Responses)
		v1.DELETE("/conversations/:id", h.DeleteConversation)
	}

	srv := &http.Server{
		Addr:    ":" + cfg.Port,
		Handler: r,
	}

	go func() {
		log.Printf("claude2api listening on :%s", cfg.Port)
		log.Printf("  Base URL : %s", cfg.ClaudeBaseURL)
		log.Printf("  Models   : %d", len(config.SupportedModels))
		if len(cfg.Accounts) > 1 {
			log.Printf("  Accounts : %d configured, least-loaded routing enabled", len(cfg.Accounts))
		} else if len(cfg.Accounts) == 1 {
			log.Printf("  Auth     : one configured account")
		} else {
			log.Printf("  Auth     : per-request Bearer token required")
		}
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("listen: %s", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Println("shutting down...")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("forced shutdown: %v", err)
	}
}
