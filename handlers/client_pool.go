package handlers

import (
	"fmt"
	"hash/fnv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"claude2api/claude"
	"claude2api/config"
)

type accountClient struct {
	id            string
	credential    string
	client        *claude.Client
	active        atomic.Int64
	cooldownUntil atomic.Int64
	unhealthy     atomic.Bool
}

const accountCooldown = 5 * time.Minute

type clientLease struct {
	accountID string
	client    *claude.Client
	release   func()
}

type clientPool struct {
	baseURL string

	accountsMu    sync.RWMutex
	accounts      []*accountClient
	next          atomic.Uint64
	routingPolicy atomic.Value // string: round-robin or least-loaded
	routeMu       sync.Mutex
	affinity      sync.Map // conversation id -> *accountClient

	clientsMu sync.Mutex
	clients   sync.Map // credential id -> *claude.Client
}

func newClientPool(baseURL string, accounts []config.Account) (*clientPool, error) {
	p := &clientPool{baseURL: baseURL}
	p.routingPolicy.Store("least-loaded")
	for _, account := range accounts {
		accountID := strings.TrimSpace(account.ID)
		if accountID == "" {
			accountID = credentialID(account.SessionKey, account.Cookie)
		}
		proxyURL := resolveAccountProxy(account.ProxyURL, accountID)
		client, err := claude.NewClient(baseURL, account.SessionKey, account.Cookie, proxyURL)
		if err != nil {
			return nil, fmt.Errorf("create account client: %w", err)
		}
		p.accounts = append(p.accounts, &accountClient{
			id:         accountID,
			credential: credentialID(account.SessionKey, account.Cookie+"\x00"+proxyURL),
			client:     client,
		})
	}
	return p, nil
}

func (p *clientPool) acquire(sessionKey, cookie string, explicit bool, conversationID string) (*clientLease, error) {
	p.accountsMu.RLock()
	hasConfiguredAccounts := len(p.accounts) > 0
	p.accountsMu.RUnlock()
	if !explicit && hasConfiguredAccounts {
		if conversationID != "" {
			return p.acquireConfiguredConversation(conversationID)
		}
		return p.acquireConfigured()
	}
	if sessionKey == "" {
		return nil, fmt.Errorf("missing account credentials")
	}

	id := credentialID(sessionKey, cookie)
	if value, ok := p.clients.Load(id); ok {
		return &clientLease{accountID: id, client: value.(*claude.Client), release: func() {}}, nil
	}

	// tls-client construction is relatively expensive. Serialize cache misses so
	// concurrent first requests for the same credentials create exactly one client.
	p.clientsMu.Lock()
	defer p.clientsMu.Unlock()
	if value, ok := p.clients.Load(id); ok {
		return &clientLease{accountID: id, client: value.(*claude.Client), release: func() {}}, nil
	}
	client, err := claude.NewClient(p.baseURL, sessionKey, cookie)
	if err != nil {
		return nil, err
	}
	p.clients.Store(id, client)
	return &clientLease{accountID: id, client: client, release: func() {}}, nil
}

func (p *clientPool) acquireConfiguredConversation(conversationID string) (*clientLease, error) {
	p.routeMu.Lock()
	defer p.routeMu.Unlock()

	p.accountsMu.RLock()
	defer p.accountsMu.RUnlock()
	if len(p.accounts) == 0 {
		return nil, fmt.Errorf("no enabled configured accounts")
	}
	if value, ok := p.affinity.Load(conversationID); ok {
		pinned := value.(*accountClient)
		for _, account := range p.accounts {
			if account == pinned && account.available(time.Now()) {
				return leaseAccount(pinned), nil
			}
		}
		p.affinity.Delete(conversationID)
	}
	selected := p.selectConfiguredLocked(time.Now())
	if selected == nil {
		return nil, fmt.Errorf("all configured accounts are unavailable")
	}
	p.affinity.Store(conversationID, selected)
	return leaseAccount(selected), nil
}

func (p *clientPool) forgetConversation(conversationID string) {
	if conversationID != "" {
		p.affinity.Delete(conversationID)
	}
}

func (p *clientPool) acquireConfigured() (*clientLease, error) {
	p.routeMu.Lock()
	defer p.routeMu.Unlock()
	p.accountsMu.RLock()
	defer p.accountsMu.RUnlock()
	if len(p.accounts) == 0 {
		return nil, fmt.Errorf("no enabled configured accounts")
	}
	selected := p.selectConfiguredLocked(time.Now())
	if selected == nil {
		return nil, fmt.Errorf("all configured accounts are unavailable")
	}
	return leaseAccount(selected), nil
}

func (p *clientPool) selectConfiguredLocked(now time.Time) *accountClient {
	start := int(p.next.Add(1)-1) % len(p.accounts)
	var selected *accountClient
	for i := 0; i < len(p.accounts); i++ {
		candidate := p.accounts[(start+i)%len(p.accounts)]
		if !candidate.available(now) {
			continue
		}
		if p.routingPolicy.Load().(string) == "round-robin" {
			return candidate
		}
		if selected == nil || candidate.active.Load() < selected.active.Load() {
			selected = candidate
		}
	}
	return selected
}

func (a *accountClient) coolingDown(now time.Time) bool {
	return !a.cooldownDeadline(now).IsZero()
}

func (a *accountClient) cooldownDeadline(now time.Time) time.Time {
	until := a.cooldownUntil.Load()
	if until <= 0 || now.UnixNano() >= until {
		return time.Time{}
	}
	return time.Unix(0, until).UTC()
}

func (a *accountClient) available(now time.Time) bool {
	return !a.unhealthy.Load() && !a.coolingDown(now)
}

func (p *clientPool) accountCooldowns(now time.Time) map[string]time.Time {
	p.accountsMu.RLock()
	defer p.accountsMu.RUnlock()

	cooldowns := make(map[string]time.Time)
	for _, account := range p.accounts {
		if deadline := account.cooldownDeadline(now); !deadline.IsZero() {
			cooldowns[account.id] = deadline
		}
	}
	return cooldowns
}

func (p *clientPool) setAccountHealthy(accountID string, healthy bool) bool {
	p.accountsMu.RLock()
	defer p.accountsMu.RUnlock()
	for _, account := range p.accounts {
		if account.id == accountID {
			account.unhealthy.Store(!healthy)
			if healthy {
				account.cooldownUntil.Store(0)
			} else {
				p.clearAccountAffinity(account)
			}
			return true
		}
	}
	return false
}

func (p *clientPool) cooldownAccount(accountID string) bool {
	p.accountsMu.RLock()
	defer p.accountsMu.RUnlock()
	for _, account := range p.accounts {
		if account.id == accountID {
			account.cooldownUntil.Store(time.Now().Add(accountCooldown).UnixNano())
			p.clearAccountAffinity(account)
			return true
		}
	}
	return false
}

func (p *clientPool) restoreAccount(accountID string) bool {
	p.accountsMu.RLock()
	defer p.accountsMu.RUnlock()
	for _, account := range p.accounts {
		if account.id == accountID {
			account.cooldownUntil.Store(0)
			account.unhealthy.Store(false)
			return true
		}
	}
	return false
}

func (p *clientPool) accountByID(accountID string) (*accountClient, bool) {
	p.accountsMu.RLock()
	defer p.accountsMu.RUnlock()
	for _, account := range p.accounts {
		if account.id == accountID {
			return account, true
		}
	}
	return nil, false
}

func (p *clientPool) clearAccountAffinity(target *accountClient) {
	p.affinity.Range(func(key, value interface{}) bool {
		if value.(*accountClient) == target {
			p.affinity.Delete(key)
		}
		return true
	})
}

func (p *clientPool) setRoutingPolicy(policy string) {
	if policy != "round-robin" {
		policy = "least-loaded"
	}
	p.routingPolicy.Store(policy)
}

func (p *clientPool) replaceAccounts(accounts []config.Account) error {
	p.accountsMu.RLock()
	existing := make(map[string]*accountClient, len(p.accounts))
	for _, account := range p.accounts {
		existing[account.id] = account
	}
	p.accountsMu.RUnlock()

	replacements := make([]*accountClient, 0, len(accounts))
	retained := make(map[*accountClient]struct{}, len(accounts))
	for _, account := range accounts {
		accountID := strings.TrimSpace(account.ID)
		if accountID == "" {
			accountID = credentialID(account.SessionKey, account.Cookie)
		}
		proxyURL := resolveAccountProxy(account.ProxyURL, accountID)
		credential := credentialID(account.SessionKey, account.Cookie+"\x00"+proxyURL)
		if current, ok := existing[accountID]; ok && current.credential == credential {
			replacements = append(replacements, current)
			retained[current] = struct{}{}
			continue
		}
		client, err := claude.NewClient(p.baseURL, account.SessionKey, account.Cookie, proxyURL)
		if err != nil {
			return fmt.Errorf("create account client: %w", err)
		}
		created := &accountClient{id: accountID, credential: credential, client: client}
		replacements = append(replacements, created)
		retained[created] = struct{}{}
	}

	p.routeMu.Lock()
	defer p.routeMu.Unlock()
	p.accountsMu.Lock()
	p.accounts = replacements
	p.accountsMu.Unlock()
	p.affinity.Range(func(key, value interface{}) bool {
		if _, ok := retained[value.(*accountClient)]; !ok {
			p.affinity.Delete(key)
		}
		return true
	})
	return nil
}

func leaseAccount(account *accountClient) *clientLease {
	account.active.Add(1)
	return &clientLease{
		accountID: account.id,
		client:    account.client,
		release: func() {
			account.active.Add(-1)
		},
	}
}

func resolveAccountProxy(proxyURLTemplate, accountID string) string {
	return strings.ReplaceAll(strings.TrimSpace(proxyURLTemplate), "{sid}", accountID)
}

func credentialID(sessionKey, cookie string) string {
	h := fnv.New64a()
	_, _ = h.Write([]byte(sessionKey))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(cookie))
	return fmt.Sprintf("%016x", h.Sum64())
}
