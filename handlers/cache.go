package handlers

import (
	"crypto/sha1"
	"encoding/hex"
	"sync"
	"time"

	"claude2api/models"
)

// cacheTracker simulates prompt-caching metrics per conversation. claude.ai's
// web endpoint does not expose real caching, so we fingerprint cache_control
// blocks and report "cache_creation" on first sight and "cache_read" on reuse.
type cacheTracker struct {
	mu   sync.Mutex
	seen map[string]time.Time // fingerprint -> first seen
	ttl  time.Duration
}

var globalCacheTracker = &cacheTracker{
	seen: make(map[string]time.Time),
	ttl:  5 * time.Minute,
}

// fingerprint derives a stable key for a cacheable content block.
func cacheFingerprint(role string, block map[string]interface{}) string {
	text, _ := block["text"].(string)
	typ, _ := block["type"].(string)
	return role + "|" + typ + "|" + text
}

// record processes all cache_control blocks in a request and returns the token
// counts for cache creation and cache read.
func (t *cacheTracker) record(conversationID string, req models.AnthropicRequest) (creationTokens, readTokens int) {
	blocks := collectCacheBlocks(req)
	if len(blocks) == 0 {
		return 0, 0
	}
	accountScope := conversationID
	if accountScope == "" {
		accountScope = "stateless"
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	t.gc()
	now := time.Now()
	for _, b := range blocks {
		fp := accountScope + "|" + b.fp
		tokens := b.tokens
		if _, ok := t.seen[fp]; ok {
			readTokens += tokens
		} else {
			creationTokens += tokens
			t.seen[fp] = now
		}
	}
	return
}

// gc evicts expired fingerprints.
func (t *cacheTracker) gc() {
	now := time.Now()
	for fp, seen := range t.seen {
		if now.Sub(seen) > t.ttl {
			delete(t.seen, fp)
		}
	}
}

type cacheBlock struct {
	fp     string
	tokens int
}

// collectCacheBlocks walks the request and returns every block marked with
// cache_control, paired with its fingerprint and estimated token count.
func collectCacheBlocks(req models.AnthropicRequest) []cacheBlock {
	var blocks []cacheBlock

	// system blocks
	if arr, ok := req.System.([]interface{}); ok {
		for _, item := range arr {
			if m, ok := item.(map[string]interface{}); ok {
				if _, hasCache := m["cache_control"]; hasCache {
					fp := cacheFingerprint("system", m)
					tokens := estimateTokens(m)
					blocks = append(blocks, cacheBlock{fp, tokens})
				}
			}
		}
	}

	// message blocks
	for _, msg := range req.Messages {
		arr, ok := msg.Content.([]interface{})
		if !ok {
			continue
		}
		for _, item := range arr {
			if m, ok := item.(map[string]interface{}); ok {
				if _, hasCache := m["cache_control"]; hasCache {
					fp := cacheFingerprint(msg.Role, m)
					tokens := estimateTokens(m)
					blocks = append(blocks, cacheBlock{fp, tokens})
				}
			}
		}
	}
	return blocks
}

// estimateTokens returns a rough token count for a content block.
func estimateTokens(m map[string]interface{}) int {
	text, _ := m["text"].(string)
	if text == "" {
		return 0
	}
	// ~4 chars/token heuristic, matching the rest of the proxy.
	tokens := len(text) / 4
	if tokens < 1 {
		tokens = 1
	}
	return tokens
}

// hashFP is an unused helper kept for potential future per-user namespacing.
func hashFP(s string) string {
	h := sha1.Sum([]byte(s))
	return hex.EncodeToString(h[:])
}

// ensureCacheControl makes sure a text block carries a cache_control marker

// ensureCacheControl makes sure a text block carries a cache_control marker
// when the original request used caching. Used when echoing blocks back.
func ensureCacheControl(original, rendered map[string]interface{}) {
	if original == nil {
		return
	}
	if cc, ok := original["cache_control"]; ok {
		rendered["cache_control"] = cc
	}
}

// isCacheable reports whether a raw content block has cache_control set.
func isCacheable(m map[string]interface{}) bool {
	_, ok := m["cache_control"]
	return ok
}
