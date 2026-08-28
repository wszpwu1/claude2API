package middleware

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// BearerAuth provides OpenAI-compatible Bearer token authentication.
// The token is the claude.ai sessionKey.
// If an env-level session key is configured, the Bearer token is optional.
func BearerAuth(envSessionKey string) gin.HandlerFunc {
	return BrowserAuth(envSessionKey, "")
}

// BrowserAuth accepts a sessionKey plus an optional full claude.ai Cookie header.
func BrowserAuth(envSessionKey, envClaudeCookie string, accountPool ...bool) gin.HandlerFunc {
	hasAccountPool := len(accountPool) > 0 && accountPool[0]
	return browserAuth(envSessionKey, envClaudeCookie, func() bool { return hasAccountPool })
}

// BrowserAuthDynamic accepts the same credentials as BrowserAuth while checking
// the runtime account pool for every request, allowing account hot updates to
// take effect without rebuilding the Gin middleware chain.
func BrowserAuthDynamic(envSessionKey, envClaudeCookie string, hasAccountPool func() bool) gin.HandlerFunc {
	return browserAuth(envSessionKey, envClaudeCookie, hasAccountPool)
}

func browserAuth(envSessionKey, envClaudeCookie string, hasAccountPool func() bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		// Request-scoped credentials are a pair: never combine an explicit Bearer
		// token with the environment Cookie (or the reverse), as that mixes accounts.
		authHeader := c.GetHeader("Authorization")
		var token string
		if strings.HasPrefix(authHeader, "Bearer ") {
			token = strings.TrimSpace(strings.TrimPrefix(authHeader, "Bearer "))
		}
		cookie := strings.TrimSpace(c.GetHeader("X-Claude-Cookie"))
		explicitCredentials := token != "" || cookie != ""
		if explicitCredentials {
			if token == "" {
				token = sessionKeyFromCookie(cookie)
			}
		} else {
			token = strings.TrimSpace(envSessionKey)
			cookie = strings.TrimSpace(envClaudeCookie)
			if token == "" {
				token = sessionKeyFromCookie(cookie)
			}
		}

		poolAvailable := hasAccountPool != nil && hasAccountPool()
		if token == "" && !poolAvailable {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"error": gin.H{
					"message": "Missing API key. Provide via Authorization: Bearer <sessionKey> or X-Claude-Cookie",
					"type":    "invalid_request_error",
				},
			})
			return
		}

		// Store in context for downstream handlers
		c.Set("sessionKey", token)
		c.Set("claudeCookie", cookie)
		c.Set("explicitCredentials", explicitCredentials)
		c.Next()
	}
}

func sessionKeyFromCookie(cookie string) string {
	for _, part := range strings.Split(cookie, ";") {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) == 2 && kv[0] == "sessionKey" {
			return kv[1]
		}
	}
	return ""
}
