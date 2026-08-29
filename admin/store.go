package admin

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const passwordHashVersion = "sha256-v1"

// Store provides synchronized access to management configuration and persists
// every mutation atomically to disk.
type Store struct {
	mu    sync.RWMutex
	path  string
	state PersistentState
}

// NewStore loads an existing state file or creates one with secure defaults.
func NewStore(path, initialPassword string) (*Store, error) {
	if path == "" {
		path = "data/admin.json"
	}
	store := &Store{path: path}
	if err := store.load(); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		if initialPassword == "" {
			initialPassword = "admin"
		}
		hash, err := hashPassword(initialPassword)
		if err != nil {
			return nil, err
		}
		store.state = PersistentState{
			AdminPasswordHash: hash,
			Accounts:          []Account{},
			APIKeys:           []APIKey{},
			DailyUsage:        []DailyUsage{},
			RecentRequests:    []RecentRequest{},
			RateLimit: RateLimitConfig{
				RequestsPerMinute: 60,
				Burst:             10,
			},
			KeepAlive: KeepAliveConfig{
				Enabled:         true,
				IntervalMinutes: 10,
				TimeoutSeconds:  15,
			},
			Settings: PanelSettings{
				Language:      "zh-CN",
				Theme:         "system",
				RoutingPolicy: "least-loaded",
			},
		}
		if err := store.saveLocked(); err != nil {
			return nil, err
		}
	}
	return store, nil
}

func (s *Store) load() error {
	data, err := os.ReadFile(s.path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, &s.state); err != nil {
		return fmt.Errorf("decode admin state: %w", err)
	}
	if s.state.Accounts == nil {
		s.state.Accounts = []Account{}
	}
	if s.state.APIKeys == nil {
		s.state.APIKeys = []APIKey{}
	}
	if s.state.DailyUsage == nil {
		s.state.DailyUsage = []DailyUsage{}
	}
	if s.state.RecentRequests == nil {
		s.state.RecentRequests = []RecentRequest{}
	}
	if s.state.RateLimit.RequestsPerMinute < 1 {
		s.state.RateLimit.RequestsPerMinute = 60
	}
	if s.state.RateLimit.Burst < 1 {
		s.state.RateLimit.Burst = 10
	}
	if s.state.KeepAlive.IntervalMinutes < 1 {
		s.state.KeepAlive.IntervalMinutes = 10
	}
	if s.state.KeepAlive.TimeoutSeconds < 1 {
		s.state.KeepAlive.TimeoutSeconds = 15
	}
	return nil
}

// Snapshot returns an independent copy of the current state.
func (s *Store) Snapshot() PersistentState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	data, _ := json.Marshal(s.state)
	var snapshot PersistentState
	_ = json.Unmarshal(data, &snapshot)
	return snapshot
}

// Update applies a mutation while holding the store lock and commits it only
// when both the mutation and persistence succeed. A failed save restores the
// previous in-memory state so runtime state cannot diverge from disk.
func (s *Store) Update(fn func(*PersistentState) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	previous, err := clonePersistentState(s.state)
	if err != nil {
		return fmt.Errorf("snapshot admin state before update: %w", err)
	}
	if err := fn(&s.state); err != nil {
		s.state = previous
		return err
	}
	if err := s.saveLocked(); err != nil {
		s.state = previous
		return err
	}
	return nil
}

func clonePersistentState(state PersistentState) (PersistentState, error) {
	data, err := json.Marshal(state)
	if err != nil {
		return PersistentState{}, err
	}
	var clone PersistentState
	if err := json.Unmarshal(data, &clone); err != nil {
		return PersistentState{}, err
	}
	return clone, nil
}

func (s *Store) saveLocked() error {
	dir := filepath.Dir(s.path)
	if dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create admin data directory: %w", err)
		}
	}
	data, err := json.MarshalIndent(s.state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode admin state: %w", err)
	}
	temporary := s.path + ".tmp"
	if err := os.WriteFile(temporary, data, 0o600); err != nil {
		return fmt.Errorf("write admin state: %w", err)
	}
	if err := os.Rename(temporary, s.path); err != nil {
		_ = os.Remove(temporary)
		return fmt.Errorf("replace admin state: %w", err)
	}
	return nil
}

// VerifyPassword performs a constant-time comparison against the stored hash.
func (s *Store) VerifyPassword(password string) bool {
	s.mu.RLock()
	stored := s.state.AdminPasswordHash
	s.mu.RUnlock()
	return verifyPassword(stored, password)
}

// ChangePassword validates the current password before persisting the new one.
func (s *Store) ChangePassword(currentPassword, newPassword string) error {
	currentPassword = strings.TrimSpace(currentPassword)
	newPassword = strings.TrimSpace(newPassword)
	if !s.VerifyPassword(currentPassword) {
		return errors.New("current password is incorrect")
	}
	if len(newPassword) < 10 {
		return errors.New("new password must contain at least 10 characters")
	}
	if len(newPassword) > 128 {
		return errors.New("new password must not exceed 128 characters")
	}
	if constantTimeEqual(currentPassword, newPassword) {
		return errors.New("new password must be different from current password")
	}
	hash, err := hashPassword(newPassword)
	if err != nil {
		return err
	}
	return s.Update(func(state *PersistentState) error {
		state.AdminPasswordHash = hash
		return nil
	})
}

func hashPassword(password string) (string, error) {
	if password == "" {
		return "", errors.New("password cannot be empty")
	}
	salt := make([]byte, 32)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generate password salt: %w", err)
	}
	digest := sha256.Sum256(append(salt, []byte(password)...))
	return passwordHashVersion + "$" + hex.EncodeToString(salt) + "$" + hex.EncodeToString(digest[:]), nil
}

func verifyPassword(encoded, password string) bool {
	parts := splitHash(encoded)
	if len(parts) != 3 {
		return false
	}
	version, saltHex, expectedHex := parts[0], parts[1], parts[2]
	if version != passwordHashVersion {
		return false
	}
	salt, err := hex.DecodeString(saltHex)
	if err != nil {
		return false
	}
	expected, err := hex.DecodeString(expectedHex)
	if err != nil {
		return false
	}
	digest := sha256.Sum256(append(salt, []byte(password)...))
	if len(expected) != len(digest) {
		return false
	}
	return subtle.ConstantTimeCompare(expected, digest[:]) == 1
}

func splitHash(value string) []string {
	parts := make([]string, 0, 3)
	start := 0
	for i := 0; i < len(value); i++ {
		if value[i] == '$' {
			parts = append(parts, value[start:i])
			start = i + 1
		}
	}
	parts = append(parts, value[start:])
	return parts
}

// PruneUsage keeps only the most recent seven calendar days.
func PruneUsage(usage []DailyUsage, now time.Time) []DailyUsage {
	cutoff := now.AddDate(0, 0, -6).Format("2006-01-02")
	filtered := make([]DailyUsage, 0, len(usage))
	for _, item := range usage {
		if item.Date >= cutoff {
			filtered = append(filtered, item)
		}
	}
	return filtered
}
