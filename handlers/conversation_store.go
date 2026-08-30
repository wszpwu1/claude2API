package handlers

import (
	"sync"
	"time"
)

const conversationTTL = 24 * time.Hour

type conversationState struct {
	mu                   sync.Mutex
	ClientConversationID string
	ClaudeConversationID string
	AccountID            string
	LastHumanUUID        string
	LastAssistantUUID    string
	UpdatedAt            time.Time
}

type conversationStore struct {
	mu   sync.RWMutex
	data map[string]*conversationState
}

func newConversationStore() *conversationStore {
	s := &conversationStore{data: make(map[string]*conversationState)}
	go s.gcLoop()
	return s
}

// gcLoop periodically evicts conversation states that have not been used
// within conversationTTL. This prevents unbounded memory growth on long-running
// instances that accumulate many short-lived stateless conversations.
func (s *conversationStore) gcLoop() {
	ticker := time.NewTicker(30 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		s.gc()
	}
}

func (s *conversationStore) gc() {
	cutoff := time.Now().Add(-conversationTTL)
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, state := range s.data {
		// Only evict states whose mutex is not held (i.e. no active request).
		if state.mu.TryLock() {
			if state.UpdatedAt.Before(cutoff) {
				delete(s.data, key)
			}
			state.mu.Unlock()
		}
	}
}

func conversationKey(accountID, conversationID string) string {
	return accountID + "\x00" + conversationID
}

// acquire returns an account-scoped state locked for exclusive use. Serializing
// turns of the same conversation prevents concurrent requests from sharing the
// same parent message UUID and corrupting the upstream conversation branch.
func (s *conversationStore) acquire(accountID, conversationID string) (*conversationState, bool, func()) {
	key := conversationKey(accountID, conversationID)
	s.mu.Lock()
	state, ok := s.data[key]
	if !ok {
		state = &conversationState{
			ClientConversationID: conversationID,
			AccountID:            accountID,
			UpdatedAt:            time.Now(),
		}
		s.data[key] = state
	}
	s.mu.Unlock()

	state.mu.Lock()
	return state, !ok, state.mu.Unlock
}

func (s *conversationStore) removeIfSame(state *conversationState) {
	key := conversationKey(state.AccountID, state.ClientConversationID)
	s.mu.Lock()
	if s.data[key] == state {
		delete(s.data, key)
	}
	s.mu.Unlock()
}

func (s *conversationStore) delete(accountID, conversationID string) (*conversationState, bool) {
	key := conversationKey(accountID, conversationID)
	s.mu.Lock()
	state, ok := s.data[key]
	if ok {
		delete(s.data, key)
	}
	s.mu.Unlock()
	if !ok {
		return nil, false
	}

	state.mu.Lock()
	return state, true
}
