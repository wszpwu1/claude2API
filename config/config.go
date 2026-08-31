package config

import (
	"bufio"
	"encoding/json"
	"os"
	"strings"
)

// Config holds application configuration
type Config struct {
	Port                 string
	ClaudeBaseURL        string
	SessionKey           string
	ClaudeCookie         string
	Timezone             string
	Locale               string
	DefaultModel         string
	Effort               string
	Thinking             interface{}
	Accounts             []Account
	AdminDataFile        string
	AdminInitialPassword string
}

// Account contains credentials for one claude.ai account. Cookie may contain
// the full browser Cookie header; SessionKey is used for Bearer-only mode.
type Account struct {
	ID           string
	SessionKey   string
	Cookie       string
	ProxyURL     string
	SessionLimit int64 // 0 means unlimited
	SessionUsed  int64 // cumulative counter at the time this Account was loaded
}

// New creates a Config from environment variables with sane defaults
func New() *Config {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	baseURL := os.Getenv("CLAUDE_BASE_URL")
	if baseURL == "" {
		baseURL = "https://claude.ai"
	}

	model := os.Getenv("DEFAULT_MODEL")
	if model == "" {
		model = "claude-sonnet-5"
	}

	locale := os.Getenv("CLAUDE_LOCALE")
	if locale == "" {
		locale = "en-US"
	}

	timezone := os.Getenv("CLAUDE_TIMEZONE")
	if timezone == "" {
		timezone = "Asia/Singapore"
	}

	// CLAUDE_CODE_EFFORT_LEVEL is the env var Claude Code reads; support it so
	// the same knob controls both. CLAUDE_EFFORT is the native override.
	effort := os.Getenv("CLAUDE_EFFORT")
	if effort == "" {
		effort = os.Getenv("CLAUDE_CODE_EFFORT_LEVEL")
	}
	if effort == "" {
		effort = "medium"
	}

	// CLAUDE_THINKING accepts: "auto" (default), "none", or a JSON object like
	// {"type":"enabled","budget_tokens":10000}. It is the proxy-level default;
	// per-request "thinking" from Claude Code overrides it.
	thinkingRaw := os.Getenv("CLAUDE_THINKING")
	thinking := parseThinkingEnv(thinkingRaw)

	sessionKey := os.Getenv("CLAUDE_SESSION_KEY")
	claudeCookie := os.Getenv("CLAUDE_COOKIE")
	proxyURLTemplate := strings.TrimSpace(os.Getenv("CLAUDE_PROXY_URL"))
	accounts := loadAccounts(os.Getenv("CLAUDE_ACCOUNTS_FILE"), sessionKey, claudeCookie, proxyURLTemplate)

	adminDataFile := strings.TrimSpace(os.Getenv("ADMIN_DATA_FILE"))
	if adminDataFile == "" {
		adminDataFile = "data/admin.json"
	}
	adminInitialPassword := os.Getenv("ADMIN_INITIAL_PASSWORD")

	return &Config{
		Port:                 port,
		ClaudeBaseURL:        baseURL,
		SessionKey:           sessionKey,
		ClaudeCookie:         claudeCookie,
		Timezone:             timezone,
		Locale:               locale,
		DefaultModel:         model,
		Effort:               effort,
		Thinking:             thinking,
		Accounts:             accounts,
		AdminDataFile:        adminDataFile,
		AdminInitialPassword: adminInitialPassword,
	}
}

func loadAccounts(path, fallbackSessionKey, fallbackCookie, proxyURLTemplate string) []Account {
	if strings.TrimSpace(path) == "" {
		path = "accounts.txt"
	}

	accounts := make([]Account, 0)
	seen := make(map[string]struct{})
	add := func(account Account) {
		account.SessionKey = strings.TrimSpace(account.SessionKey)
		account.Cookie = strings.TrimSpace(account.Cookie)
		account.ProxyURL = strings.TrimSpace(account.ProxyURL)
		if account.ProxyURL == "" {
			account.ProxyURL = proxyURLTemplate
		}
		if account.SessionKey == "" {
			account.SessionKey = sessionKeyFromCookie(account.Cookie)
		}
		if account.SessionKey == "" {
			return
		}
		key := account.SessionKey + "\x00" + account.Cookie
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		accounts = append(accounts, account)
	}

	if f, err := os.Open(path); err == nil {
		defer f.Close()
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 64*1024), 1024*1024)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			if strings.Contains(line, "sessionKey=") {
				add(Account{Cookie: line})
			} else {
				add(Account{SessionKey: line})
			}
		}
	}

	add(Account{SessionKey: fallbackSessionKey, Cookie: fallbackCookie})
	return accounts
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

// parseThinkingEnv parses the CLAUDE_THINKING env var into a value suitable for
// Config.Thinking. Returns nil (auto), a disabled map, an enabled map, or a
// custom JSON object.
//
// Mapping:
//
//	""                          → nil  (proxy default: auto)
//	"auto"                      → nil  (explicit auto; same as unset)
//	"none" / "disabled"         → {"type":"disabled"}
//	"enabled"                   → {"type":"enabled","budget_tokens":10000}
//	JSON object                 → the parsed map (passed through verbatim)
//	anything else               → nil  (fall back to auto rather than silently enabling)
func parseThinkingEnv(s string) interface{} {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	lower := strings.ToLower(s)
	switch lower {
	case "auto":
		// Explicit "auto" means the proxy should let claude.ai decide — same as
		// the unset case. Previously this fell through to the enabled branch,
		// inadvertently switching on thinking mode whenever CLAUDE_THINKING=auto.
		return nil
	case "none", "disabled":
		return map[string]interface{}{"type": "disabled"}
	case "enabled":
		return map[string]interface{}{"type": "enabled", "budget_tokens": 10000}
	default:
		// Try to parse as a JSON object so callers can pass a full config like
		// {"type":"enabled","budget_tokens":5000}.
		var m map[string]interface{}
		if err := json.Unmarshal([]byte(s), &m); err == nil {
			return m
		}
		// Unrecognised string — fall back to auto rather than enabling thinking
		// unexpectedly.
		return nil
	}
}

// SupportedModels is the set of models exposed by the API
var SupportedModels = map[string]string{
	"claude-fable-5":    "claude-fable-5",
	"claude-opus-4-8":   "claude-opus-4-8",
	"claude-haiku-4-5":  "claude-haiku-4-5",
	"claude-opus-4-7":   "claude-opus-4-7",
	"claude-opus-4-6":   "claude-opus-4-6",
	"claude-opus-3":     "claude-opus-3",
	"claude-sonnet-4-6": "claude-sonnet-4-6",
	"claude-sonnet-5":   "claude-sonnet-5",
	"claude-opus-5":     "claude-opus-5",
}
