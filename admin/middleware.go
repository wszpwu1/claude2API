package admin

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/gin-gonic/gin"
)

// authCache is a read-optimised copy of the auth config that is cheaply
// replaceable via an atomic pointer. It is rebuilt by the Store whenever
// MasterKey or APIKeys change, and is used in the hot-path middleware so
// we never pay the cost of a full PersistentState clone on every request.
type authCache struct {
	masterKey MasterKeyConfig
	apiKeys   []APIKey
}

// RuntimeMiddleware applies dashboard-managed API-key authentication, global
// rate limiting, and request metrics to public API routes.
type RuntimeMiddleware struct {
	store     *Store
	metrics   *Metrics
	mu        sync.Mutex
	rateLimit RateLimitConfig
	tokens    float64
	updated   time.Time

	// auth is an atomic pointer to the latest authCache.
	// We store a *authCache as an unsafe.Pointer so we can use atomic ops
	// without an extra allocation on every read.
	auth unsafe.Pointer // *authCache
}

func NewRuntimeMiddleware(store *Store, metrics *Metrics) *RuntimeMiddleware {
	snap := store.SnapshotAuth()
	config := store.SnapshotRateLimit()
	initialTokens := float64(config.Burst)
	m := &RuntimeMiddleware{
		store:     store,
		metrics:   metrics,
		rateLimit: config,
		tokens:    initialTokens,
		updated:   time.Now(),
	}
	m.storeAuth(snap)
	// Subscribe to store changes so the auth cache stays fresh.
	store.OnAuthChanged(m.storeAuth)
	return m
}

func (m *RuntimeMiddleware) storeAuth(c authCache) {
	atomic.StorePointer(&m.auth, unsafe.Pointer(&c))
}

func (m *RuntimeMiddleware) loadAuth() authCache {
	p := atomic.LoadPointer(&m.auth)
	if p == nil {
		return authCache{}
	}
	return *(*authCache)(p)
}

// SetRateLimit applies a new global rate-limit configuration immediately and
// resets the token bucket so stale capacity from the previous configuration
// cannot leak into the new policy.
func (m *RuntimeMiddleware) SetRateLimit(config RateLimitConfig) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rateLimit = config
	m.tokens = float64(config.Burst)
	m.updated = time.Now()
	return nil
}

func (m *RuntimeMiddleware) Handler() gin.HandlerFunc {
	return func(c *gin.Context) {
		// Use the cached auth config — no full state clone on every request.
		auth := m.loadAuth()
		if (auth.masterKey.Enabled || len(auth.apiKeys) > 0) && !m.validateAPIKey(c, auth.masterKey, auth.apiKeys) {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": gin.H{"message": "invalid API key", "type": "unauthorized"}})
			return
		}
		m.mu.Lock()
		rateLimit := m.rateLimit
		m.mu.Unlock()
		if rateLimit.Enabled && !m.allow(rateLimit) {
			c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{"error": gin.H{"message": "global rate limit exceeded", "type": "rate_limit_error"}})
			return
		}

		startedAt := time.Now()
		finish := m.metrics.Begin()
		c.Next()
		status := c.Writer.Status()
		success := status >= 200 && status < 400
		finish(success)

		accountID, _ := c.Get("accountID")
		if id, ok := accountID.(string); ok && strings.TrimSpace(id) != "" {
			model, _ := c.Get("requestModel")
			inputTokens, _ := c.Get("inputTokens")
			outputTokens, _ := c.Get("outputTokens")
			modelName, _ := model.(string)
			inputCount, _ := inputTokens.(int64)
			outputCount, _ := outputTokens.(int64)
			// recordAccountUsage runs after c.Next() so the response has already
			// been flushed to the client. The disk write does not add latency
			// visible to the caller, but keeping it synchronous makes the store
			// observable immediately (important for tests and admin dashboards).
			_ = m.recordAccountUsage(id, modelName, status, success, time.Since(startedAt), inputCount, outputCount, time.Now().UTC())
		}
	}
}

func (m *RuntimeMiddleware) validateAPIKey(c *gin.Context, masterKey MasterKeyConfig, keys []APIKey) bool {
	raw := strings.TrimSpace(c.GetHeader("X-API-Key"))
	if raw == "" {
		authorization := strings.TrimSpace(c.GetHeader("Authorization"))
		if strings.HasPrefix(authorization, "Bearer c2a_") {
			raw = strings.TrimSpace(strings.TrimPrefix(authorization, "Bearer "))
		}
	}
	if raw == "" {
		return false
	}
	digest := sha256.Sum256([]byte(raw))
	hash := hex.EncodeToString(digest[:])
	if masterKey.Enabled && constantTimeEqual(masterKey.KeyHash, hash) {
		now := time.Now().UTC()
		// Update LastUsedAt asynchronously — audit data, not needed in the hot path.
		go func() {
			_ = m.store.Update(func(state *PersistentState) error {
				state.MasterKey.LastUsedAt = &now
				return nil
			})
		}()
		c.Set("masterKey", true)
		return true
	}
	for _, key := range keys {
		if key.Enabled && constantTimeEqual(key.KeyHash, hash) {
			keyID := key.ID
			now := time.Now().UTC()
			// Update LastUsedAt asynchronously — audit data, not needed in the hot path.
			go func() {
				_ = m.store.Update(func(state *PersistentState) error {
					for index := range state.APIKeys {
						if state.APIKeys[index].ID == keyID {
							state.APIKeys[index].LastUsedAt = &now
							break
						}
					}
					return nil
				})
			}()
			return true
		}
	}
	return false
}

func (m *RuntimeMiddleware) recordAccountUsage(accountID, model string, status int, success bool, latency time.Duration, inputTokens, outputTokens int64, now time.Time) error {
	date := now.Format("2006-01-02")
	return m.store.Update(func(state *PersistentState) error {
		accountName := accountID
		for index := range state.Accounts {
			if state.Accounts[index].ID != accountID {
				continue
			}
			accountName = state.Accounts[index].Name
			state.Accounts[index].RequestCount++
			state.Accounts[index].SessionUsed++
			if success {
				state.Accounts[index].SuccessCount++
			} else {
				state.Accounts[index].FailureCount++
			}
			break
		}
		state.RecentRequests = append([]RecentRequest{{Time: now, Model: model, AccountID: accountID, Account: accountName, Status: status, Success: success, LatencyMS: latency.Milliseconds(), InputToken: inputTokens, OutputToken: outputTokens}}, state.RecentRequests...)
		if len(state.RecentRequests) > 100 {
			state.RecentRequests = state.RecentRequests[:100]
		}

		state.DailyUsage = PruneUsage(state.DailyUsage, now)
		for index := range state.DailyUsage {
			if state.DailyUsage[index].Date == date && state.DailyUsage[index].AccountID == accountID {
				state.DailyUsage[index].Requests++
				if success {
					state.DailyUsage[index].Successes++
				} else {
					state.DailyUsage[index].Failures++
				}
				state.DailyUsage[index].LatencyTotalMS += latency.Milliseconds()
				return nil
			}
		}

		usage := DailyUsage{
			Date:           date,
			AccountID:      accountID,
			Requests:       1,
			LatencyTotalMS: latency.Milliseconds(),
		}
		if success {
			usage.Successes = 1
		} else {
			usage.Failures = 1
		}
		state.DailyUsage = append(state.DailyUsage, usage)
		return nil
	})
}

func (m *RuntimeMiddleware) allow(config RateLimitConfig) bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := time.Now()
	burst := float64(config.Burst)
	if m.tokens > burst {
		m.tokens = burst
	}
	elapsed := now.Sub(m.updated).Seconds()
	m.tokens += elapsed * float64(config.RequestsPerMinute) / 60
	if m.tokens > burst {
		m.tokens = burst
	}
	m.updated = now
	if m.tokens < 1 {
		return false
	}
	m.tokens--
	return true
}
