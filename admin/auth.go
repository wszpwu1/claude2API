package admin

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

const adminSessionTTL = 24 * time.Hour

type adminSession struct {
	ExpiresAt time.Time
}

// AuthManager manages short-lived administrator sessions in memory.
type AuthManager struct {
	store    *Store
	mu       sync.RWMutex
	sessions map[string]adminSession
}

func NewAuthManager(store *Store) *AuthManager {
	return &AuthManager{
		store:    store,
		sessions: make(map[string]adminSession),
	}
}

func (a *AuthManager) Login(password string) (string, time.Time, bool) {
	if !a.store.VerifyPassword(password) {
		return "", time.Time{}, false
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", time.Time{}, false
	}
	token := hex.EncodeToString(raw)
	expiresAt := time.Now().Add(adminSessionTTL)
	a.mu.Lock()
	a.sessions[hashToken(token)] = adminSession{ExpiresAt: expiresAt}
	a.pruneLocked(time.Now())
	a.mu.Unlock()
	return token, expiresAt, true
}

func (a *AuthManager) Logout(token string) {
	if token == "" {
		return
	}
	a.mu.Lock()
	delete(a.sessions, hashToken(token))
	a.mu.Unlock()
}

func (a *AuthManager) Validate(token string) bool {
	if token == "" {
		return false
	}
	now := time.Now()
	a.mu.RLock()
	session, ok := a.sessions[hashToken(token)]
	a.mu.RUnlock()
	if !ok || !session.ExpiresAt.After(now) {
		if ok {
			a.Logout(token)
		}
		return false
	}
	return true
}

func (a *AuthManager) Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		token := adminToken(c)
		if !a.Validate(token) {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"error": gin.H{"message": "administrator authentication required", "type": "unauthorized"},
			})
			return
		}
		c.Next()
	}
}

func (a *AuthManager) RevokeAll() {
	a.mu.Lock()
	a.sessions = make(map[string]adminSession)
	a.mu.Unlock()
}

func (a *AuthManager) pruneLocked(now time.Time) {
	for token, session := range a.sessions {
		if !session.ExpiresAt.After(now) {
			delete(a.sessions, token)
		}
	}
}

func adminToken(c *gin.Context) string {
	authorization := strings.TrimSpace(c.GetHeader("Authorization"))
	if strings.HasPrefix(authorization, "Bearer ") {
		return strings.TrimSpace(strings.TrimPrefix(authorization, "Bearer "))
	}
	if cookie, err := c.Cookie("claude2api_admin"); err == nil {
		return strings.TrimSpace(cookie)
	}
	return ""
}

func hashToken(token string) string {
	digest := sha256.Sum256([]byte(token))
	return hex.EncodeToString(digest[:])
}

func constantTimeEqual(left, right string) bool {
	if len(left) != len(right) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(left), []byte(right)) == 1
}
