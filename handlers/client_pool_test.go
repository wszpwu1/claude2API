package handlers

import (
	"sync"
	"testing"
	"time"

	"claude2api/config"
)

func TestResolveAccountProxyBindsStableSID(t *testing.T) {
	got := resolveAccountProxy("  http://proxy.example.com/session-{sid}  ", "account-a")
	want := "http://proxy.example.com/session-account-a"
	if got != want {
		t.Fatalf("resolveAccountProxy() = %q, want %q", got, want)
	}

	if repeated := resolveAccountProxy("http://{sid}.example.com/{sid}", "account-a"); repeated != "http://account-a.example.com/account-a" {
		t.Fatalf("resolveAccountProxy() did not replace every placeholder: %q", repeated)
	}
}

func TestClientPoolBalancesConfiguredAccounts(t *testing.T) {
	pool, err := newClientPool("https://claude.ai", []config.Account{
		{SessionKey: "account-a"},
		{SessionKey: "account-b"},
	})
	if err != nil {
		t.Fatalf("newClientPool: %v", err)
	}

	first, err := pool.acquireConfigured()
	if err != nil {
		t.Fatalf("first acquireConfigured: %v", err)
	}
	second, err := pool.acquireConfigured()
	if err != nil {
		t.Fatalf("second acquireConfigured: %v", err)
	}
	defer first.release()
	defer second.release()
	if first.accountID == second.accountID {
		t.Fatalf("expected least-loaded routing across accounts, got %q twice", first.accountID)
	}
}

func TestClientPoolPinsConversationToConfiguredAccount(t *testing.T) {
	pool, err := newClientPool("https://claude.ai", []config.Account{
		{SessionKey: "account-a"},
		{SessionKey: "account-b"},
	})
	if err != nil {
		t.Fatalf("newClientPool: %v", err)
	}

	first, err := pool.acquire("", "", false, "conversation-1")
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	firstID := first.accountID
	first.release()

	// Occupy the pinned account so ordinary least-loaded routing would prefer the
	// other account. The same conversation must still retain account affinity.
	pinned, ok := pool.affinity.Load("conversation-1")
	if !ok {
		t.Fatal("conversation affinity was not stored")
	}
	occupied := leaseAccount(pinned.(*accountClient))
	defer occupied.release()

	second, err := pool.acquire("", "", false, "conversation-1")
	if err != nil {
		t.Fatalf("second acquire: %v", err)
	}
	defer second.release()
	if second.accountID != firstID {
		t.Fatalf("conversation moved from account %q to %q", firstID, second.accountID)
	}
}

func TestClientPoolForgetConversationAllowsRebalance(t *testing.T) {
	pool, err := newClientPool("https://claude.ai", []config.Account{
		{SessionKey: "account-a"},
		{SessionKey: "account-b"},
	})
	if err != nil {
		t.Fatalf("newClientPool: %v", err)
	}

	first, err := pool.acquire("", "", false, "conversation-1")
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	firstID := first.accountID
	first.release()
	pinned, ok := pool.affinity.Load("conversation-1")
	if !ok {
		t.Fatal("conversation affinity was not stored")
	}
	pool.forgetConversation("conversation-1")

	occupied := leaseAccount(pinned.(*accountClient))
	defer occupied.release()

	second, err := pool.acquire("", "", false, "conversation-1")
	if err != nil {
		t.Fatalf("second acquire: %v", err)
	}
	defer second.release()
	if second.accountID == firstID {
		t.Fatalf("expected forgotten conversation to rebalance away from busy account %q", firstID)
	}
}

func TestClientPoolConcurrentConversationAffinity(t *testing.T) {
	pool, err := newClientPool("https://claude.ai", []config.Account{
		{SessionKey: "account-a"},
		{SessionKey: "account-b"},
		{SessionKey: "account-c"},
	})
	if err != nil {
		t.Fatalf("newClientPool: %v", err)
	}

	const workers = 64
	ids := make(chan string, workers)
	start := make(chan struct{})
	releaseAll := make(chan struct{})
	var acquired sync.WaitGroup
	var released sync.WaitGroup
	acquired.Add(workers)
	released.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer released.Done()
			<-start
			lease, err := pool.acquire("", "", false, "conversation-1")
			if err != nil {
				t.Errorf("acquire: %v", err)
				acquired.Done()
				return
			}
			ids <- lease.accountID
			acquired.Done()
			<-releaseAll
			lease.release()
		}()
	}
	close(start)
	acquired.Wait()
	close(ids)

	var expected string
	for id := range ids {
		if expected == "" {
			expected = id
		}
		if id != expected {
			t.Fatalf("conversation assigned to multiple accounts: %q and %q", expected, id)
		}
	}
	for _, account := range pool.accounts {
		want := int64(0)
		if account.id == expected {
			want = workers
		}
		if active := account.active.Load(); active != want {
			t.Fatalf("account %q active leases = %d, want %d", account.id, active, want)
		}
	}

	close(releaseAll)
	released.Wait()
	for _, account := range pool.accounts {
		if active := account.active.Load(); active != 0 {
			t.Fatalf("account %q leaked active leases: %d", account.id, active)
		}
	}
}

func TestClientPoolBalancesConcurrentConfiguredRequests(t *testing.T) {
	pool, err := newClientPool("https://claude.ai", []config.Account{
		{SessionKey: "account-a"},
		{SessionKey: "account-b"},
		{SessionKey: "account-c"},
		{SessionKey: "account-d"},
	})
	if err != nil {
		t.Fatalf("newClientPool: %v", err)
	}

	const workers = 64
	start := make(chan struct{})
	releaseAll := make(chan struct{})
	var acquired sync.WaitGroup
	var released sync.WaitGroup
	acquired.Add(workers)
	released.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer released.Done()
			<-start
			lease, err := pool.acquireConfigured()
			if err != nil {
				t.Errorf("acquireConfigured: %v", err)
				acquired.Done()
				return
			}
			acquired.Done()
			<-releaseAll
			lease.release()
		}()
	}
	close(start)
	acquired.Wait()

	for _, account := range pool.accounts {
		if active := account.active.Load(); active != workers/int64(len(pool.accounts)) {
			t.Fatalf("account %q active leases = %d, want %d", account.id, active, workers/len(pool.accounts))
		}
	}
	close(releaseAll)
	released.Wait()
}

func TestClientPoolReplaceAccountsRefreshesRuntimeRouting(t *testing.T) {
	pool, err := newClientPool("https://claude.ai", []config.Account{
		{ID: "account-a", SessionKey: "session-a"},
		{ID: "account-b", SessionKey: "session-b"},
	})
	if err != nil {
		t.Fatalf("newClientPool: %v", err)
	}

	lease, err := pool.acquire("", "", false, "conversation-1")
	if err != nil {
		t.Fatalf("initial acquire: %v", err)
	}
	lease.release()
	if _, ok := pool.affinity.Load("conversation-1"); !ok {
		t.Fatal("expected conversation affinity before account replacement")
	}

	if err := pool.replaceAccounts([]config.Account{
		{ID: "account-c", SessionKey: "session-c"},
	}); err != nil {
		t.Fatalf("replaceAccounts: %v", err)
	}
	if _, ok := pool.affinity.Load("conversation-1"); ok {
		t.Fatal("account replacement must clear stale conversation affinity")
	}

	refreshed, err := pool.acquire("", "", false, "conversation-1")
	if err != nil {
		t.Fatalf("refreshed acquire: %v", err)
	}
	defer refreshed.release()
	if refreshed.accountID != "account-c" {
		t.Fatalf("refreshed routing selected %q, want account-c", refreshed.accountID)
	}
}

func TestClientPoolRoundRobinRouting(t *testing.T) {
	pool, err := newClientPool("https://claude.ai", []config.Account{
		{ID: "account-a", SessionKey: "session-a"},
		{ID: "account-b", SessionKey: "session-b"},
		{ID: "account-c", SessionKey: "session-c"},
	})
	if err != nil {
		t.Fatalf("newClientPool: %v", err)
	}
	pool.setRoutingPolicy("round-robin")

	want := []string{"account-a", "account-b", "account-c", "account-a"}
	for index, expected := range want {
		lease, err := pool.acquireConfigured()
		if err != nil {
			t.Fatalf("acquireConfigured %d: %v", index, err)
		}
		if lease.accountID != expected {
			t.Fatalf("selection %d = %q, want %q", index, lease.accountID, expected)
		}
		lease.release()
	}
}

func TestClientPoolReplaceAccountsPreservesRetainedAffinity(t *testing.T) {
	pool, err := newClientPool("https://claude.ai", []config.Account{
		{ID: "account-a", SessionKey: "session-a"},
		{ID: "account-b", SessionKey: "session-b"},
	})
	if err != nil {
		t.Fatalf("newClientPool: %v", err)
	}

	lease, err := pool.acquire("", "", false, "conversation-1")
	if err != nil {
		t.Fatalf("initial acquire: %v", err)
	}
	accountID := lease.accountID
	lease.release()

	if err := pool.replaceAccounts([]config.Account{
		{ID: "account-a", SessionKey: "session-a"},
		{ID: "account-b", SessionKey: "session-b"},
		{ID: "account-c", SessionKey: "session-c"},
	}); err != nil {
		t.Fatalf("replaceAccounts: %v", err)
	}

	refreshed, err := pool.acquire("", "", false, "conversation-1")
	if err != nil {
		t.Fatalf("refreshed acquire: %v", err)
	}
	defer refreshed.release()
	if refreshed.accountID != accountID {
		t.Fatalf("retained conversation moved from %q to %q", accountID, refreshed.accountID)
	}
}

func TestClientPoolSkipsCoolingDownAccountAndRestoresIt(t *testing.T) {
	pool, err := newClientPool("https://claude.ai", []config.Account{
		{ID: "account-a", SessionKey: "session-a"},
		{ID: "account-b", SessionKey: "session-b"},
	})
	if err != nil {
		t.Fatalf("newClientPool: %v", err)
	}

	if !pool.cooldownAccount("account-a") {
		t.Fatal("expected account-a to enter cooldown")
	}
	for index := 0; index < 4; index++ {
		lease, err := pool.acquireConfigured()
		if err != nil {
			t.Fatalf("acquireConfigured: %v", err)
		}
		if lease.accountID != "account-b" {
			t.Fatalf("selected cooling account %q", lease.accountID)
		}
		lease.release()
	}

	if !pool.restoreAccount("account-a") {
		t.Fatal("expected account-a to be restored")
	}
	pool.setRoutingPolicy("round-robin")
	seenA := false
	for index := 0; index < 4; index++ {
		lease, err := pool.acquireConfigured()
		if err != nil {
			t.Fatalf("acquireConfigured after restore: %v", err)
		}
		seenA = seenA || lease.accountID == "account-a"
		lease.release()
	}
	if !seenA {
		t.Fatal("restored account-a was not returned to rotation")
	}
}

func TestClientPoolSkipsUnhealthyAccountUntilHealthy(t *testing.T) {
	pool, err := newClientPool("https://claude.ai", []config.Account{
		{ID: "account-a", SessionKey: "session-a"},
		{ID: "account-b", SessionKey: "session-b"},
	})
	if err != nil {
		t.Fatalf("newClientPool: %v", err)
	}

	if !pool.setAccountHealthy("account-a", false) {
		t.Fatal("expected account-a health state to be updated")
	}
	for index := 0; index < 4; index++ {
		lease, err := pool.acquireConfigured()
		if err != nil {
			t.Fatalf("acquireConfigured: %v", err)
		}
		if lease.accountID != "account-b" {
			t.Fatalf("selected unhealthy account %q", lease.accountID)
		}
		lease.release()
	}

	if !pool.setAccountHealthy("account-a", true) {
		t.Fatal("expected account-a health state to be restored")
	}
	pool.setRoutingPolicy("round-robin")
	seenA := false
	for index := 0; index < 4; index++ {
		lease, err := pool.acquireConfigured()
		if err != nil {
			t.Fatalf("acquireConfigured after healthy: %v", err)
		}
		seenA = seenA || lease.accountID == "account-a"
		lease.release()
	}
	if !seenA {
		t.Fatal("healthy account-a was not returned to rotation")
	}
}

func TestClientPoolUnhealthyAccountClearsConversationAffinity(t *testing.T) {
	pool, err := newClientPool("https://claude.ai", []config.Account{
		{ID: "account-a", SessionKey: "session-a"},
		{ID: "account-b", SessionKey: "session-b"},
	})
	if err != nil {
		t.Fatalf("newClientPool: %v", err)
	}

	lease, err := pool.acquire("", "", false, "conversation-1")
	if err != nil {
		t.Fatalf("initial acquire: %v", err)
	}
	accountID := lease.accountID
	lease.release()
	if !pool.setAccountHealthy(accountID, false) {
		t.Fatalf("expected %s health state to be updated", accountID)
	}
	if _, ok := pool.affinity.Load("conversation-1"); ok {
		t.Fatal("unhealthy account must clear conversation affinity")
	}

	refreshed, err := pool.acquire("", "", false, "conversation-1")
	if err != nil {
		t.Fatalf("refreshed acquire: %v", err)
	}
	defer refreshed.release()
	if refreshed.accountID == accountID {
		t.Fatalf("conversation remained on unhealthy account %q", accountID)
	}
}

func TestClientPoolCooldownExpiresAutomatically(t *testing.T) {
	pool, err := newClientPool("https://claude.ai", []config.Account{
		{ID: "account-a", SessionKey: "session-a"},
	})
	if err != nil {
		t.Fatalf("newClientPool: %v", err)
	}
	account := pool.accounts[0]
	account.cooldownUntil.Store(time.Now().Add(time.Millisecond).UnixNano())
	if _, err := pool.acquireConfigured(); err == nil {
		t.Fatal("expected acquisition to fail while account is cooling down")
	}
	time.Sleep(2 * time.Millisecond)
	lease, err := pool.acquireConfigured()
	if err != nil {
		t.Fatalf("account did not recover automatically: %v", err)
	}
	lease.release()
}

func TestClientPoolAccountCooldownsReturnsOnlyActiveDeadlines(t *testing.T) {
	pool, err := newClientPool("https://claude.ai", []config.Account{
		{ID: "account-a", SessionKey: "session-a"},
		{ID: "account-b", SessionKey: "session-b"},
	})
	if err != nil {
		t.Fatalf("newClientPool: %v", err)
	}

	now := time.Now()
	activeDeadline := now.Add(5 * time.Minute)
	pool.accounts[0].cooldownUntil.Store(activeDeadline.UnixNano())
	pool.accounts[1].cooldownUntil.Store(now.Add(-time.Minute).UnixNano())

	cooldowns := pool.accountCooldowns(now)
	if len(cooldowns) != 1 {
		t.Fatalf("cooldown count = %d, want 1", len(cooldowns))
	}
	deadline, ok := cooldowns["account-a"]
	if !ok {
		t.Fatal("active cooldown for account-a was not returned")
	}
	if !deadline.Equal(activeDeadline.UTC()) {
		t.Fatalf("cooldown deadline = %v, want %v", deadline, activeDeadline.UTC())
	}
	if _, ok := cooldowns["account-b"]; ok {
		t.Fatal("expired cooldown for account-b must not be returned")
	}
}

func TestConversationStoreScopesAccountsAndSerializesTurns(t *testing.T) {
	store := newConversationStore()
	accountA, _, releaseA := store.acquire("account-a", "conversation-1")
	accountA.ClaudeConversationID = "upstream-a"
	releaseA()

	accountB, createdB, releaseB := store.acquire("account-b", "conversation-1")
	if !createdB {
		t.Fatal("same client conversation ID must create independent state for another account")
	}
	accountB.ClaudeConversationID = "upstream-b"
	releaseB()

	locked, _, releaseLocked := store.acquire("account-a", "conversation-1")
	if locked.ClaudeConversationID != "upstream-a" {
		t.Fatalf("unexpected account A mapping: %q", locked.ClaudeConversationID)
	}

	acquired := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _, release := store.acquire("account-a", "conversation-1")
		close(acquired)
		release()
	}()

	select {
	case <-acquired:
		t.Fatal("same conversation was acquired concurrently")
	default:
	}
	releaseLocked()
	wg.Wait()
}
