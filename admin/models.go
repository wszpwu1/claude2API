package admin

import "time"

// PersistentState is the management panel's durable configuration and runtime metadata.
type PersistentState struct {
	AdminPasswordHash string          `json:"admin_password_hash"`
	MasterKey         MasterKeyConfig `json:"master_key"`
	Accounts          []Account       `json:"accounts"`
	APIKeys           []APIKey        `json:"api_keys"`
	Proxy             ProxyConfig     `json:"proxy"`
	RateLimit         RateLimitConfig `json:"rate_limit"`
	KeepAlive         KeepAliveConfig `json:"keep_alive"`
	Settings          PanelSettings   `json:"settings"`
	DailyUsage        []DailyUsage    `json:"daily_usage"`
	RecentRequests    []RecentRequest `json:"recent_requests"`
}

// Account describes one managed claude.ai account. Secrets are never returned directly by management APIs.
type Account struct {
	ID               string    `json:"id"`
	Name             string    `json:"name"`
	SessionKey       string    `json:"session_key"`
	Cookie           string    `json:"cookie,omitempty"`
	Enabled          bool      `json:"enabled"`
	Status           string    `json:"status"`
	StatusMessage    string    `json:"status_message,omitempty"`
	ActiveRequests   int64     `json:"active_requests"`
	RequestCount     int64     `json:"request_count"`
	SuccessCount     int64     `json:"success_count"`
	FailureCount     int64     `json:"failure_count"`
	SessionUsed      int64     `json:"session_used"`
	SessionLimit     int64     `json:"session_limit"`
	SessionUsageRate float64   `json:"session_usage_rate"`
	LastCheckedAt    time.Time `json:"last_checked_at,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

// MasterKeyConfig is the optional global credential that grants access to the
// shared account pool. Only its hash and a display-safe prefix are persisted.
type MasterKeyConfig struct {
	KeyHash    string     `json:"key_hash"`
	KeyPrefix  string     `json:"key_prefix"`
	Enabled    bool       `json:"enabled"`
	UpdatedAt  time.Time  `json:"updated_at,omitempty"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
}

// APIKey is an API credential accepted by the public endpoints.
type APIKey struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	KeyHash    string     `json:"key_hash"`
	KeyPrefix  string     `json:"key_prefix"`
	Enabled    bool       `json:"enabled"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
}

// ProxyConfig controls the optional upstream proxy. URLTemplate may contain {sid},
// which is replaced with a stable per-account identifier before creating a client.
type ProxyConfig struct {
	Enabled     bool   `json:"enabled"`
	URLTemplate string `json:"url_template"`
}

// RateLimitConfig defines a process-wide token-bucket limit for public API traffic.
type RateLimitConfig struct {
	Enabled           bool `json:"enabled"`
	RequestsPerMinute int  `json:"requests_per_minute"`
	Burst             int  `json:"burst"`
}

// KeepAliveConfig controls periodic account health checks. IntervalMinutes is
// applied at runtime without restarting the service.
type KeepAliveConfig struct {
	Enabled         bool `json:"enabled"`
	IntervalMinutes int  `json:"interval_minutes"`
	TimeoutSeconds  int  `json:"timeout_seconds"`
}

// PanelSettings stores user-interface and routing preferences.
type PanelSettings struct {
	Language      string `json:"language"`
	Theme         string `json:"theme"`
	RoutingPolicy string `json:"routing_policy"`
}

// DailyUsage stores aggregated request metrics for one calendar day and account.
type DailyUsage struct {
	Date           string `json:"date"`
	AccountID      string `json:"account_id"`
	Requests       int64  `json:"requests"`
	Successes      int64  `json:"successes"`
	Failures       int64  `json:"failures"`
	LatencyTotalMS int64  `json:"latency_total_ms"`
}

// RecentRequest stores one completed request for the dashboard's recent-request list.
type RecentRequest struct {
	Time        time.Time `json:"time"`
	Model       string    `json:"model"`
	AccountID   string    `json:"account_id"`
	Account     string    `json:"account"`
	Status      int       `json:"status"`
	Success     bool      `json:"success"`
	LatencyMS   int64     `json:"latency_ms"`
	InputToken  int64     `json:"input_tokens"`
	OutputToken int64     `json:"output_tokens"`
}

// MetricsSnapshot is returned by the real-time metrics endpoint.
type MetricsSnapshot struct {
	Requests       int64   `json:"requests"`
	Successes      int64   `json:"successes"`
	Failures       int64   `json:"failures"`
	ActiveRequests int64   `json:"active_requests"`
	AverageLatency float64 `json:"average_latency_ms"`
	SuccessRate    float64 `json:"success_rate"`
}
