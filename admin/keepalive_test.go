package admin

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestKeepAliveWorkerChecksEnabledAccountsAndUpdatesStatus(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "admin.json"), "test-password")
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if err := store.Update(func(state *PersistentState) error {
		state.Accounts = []Account{
			{ID: "healthy", Enabled: true, Status: "unknown"},
			{ID: "unhealthy", Enabled: true, Status: "unknown"},
			{ID: "disabled", Enabled: false, Status: "unknown"},
		}
		return nil
	}); err != nil {
		t.Fatalf("seed accounts: %v", err)
	}

	checked := make(map[string]int)
	worker := NewKeepAliveWorker(store, func(_ context.Context, accountID string) error {
		checked[accountID]++
		if accountID == "unhealthy" {
			return errors.New("upstream unavailable")
		}
		return nil
	})
	worker.checkAccounts(context.Background(), KeepAliveConfig{Enabled: true, TimeoutSeconds: 1})

	if checked["healthy"] != 1 || checked["unhealthy"] != 1 {
		t.Fatalf("enabled account checks = %#v, want each enabled account once", checked)
	}
	if checked["disabled"] != 0 {
		t.Fatal("disabled account must not be checked")
	}

	accounts := store.Snapshot().Accounts
	if accounts[0].Status != "healthy" || accounts[0].LastCheckedAt.IsZero() {
		t.Fatalf("unexpected healthy account state: %#v", accounts[0])
	}
	if accounts[1].Status != "unhealthy" || accounts[1].StatusMessage != "upstream unavailable" || accounts[1].LastCheckedAt.IsZero() {
		t.Fatalf("unexpected unhealthy account state: %#v", accounts[1])
	}
	if accounts[2].Status != "unknown" || !accounts[2].LastCheckedAt.IsZero() {
		t.Fatalf("disabled account was modified: %#v", accounts[2])
	}
}

func TestKeepAliveWorkerSetConfigAppliesRuntimeConfiguration(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "admin.json"), "test-password")
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	worker := NewKeepAliveWorker(store, nil)
	config := KeepAliveConfig{Enabled: true, IntervalMinutes: 3, TimeoutSeconds: 20}
	if err := worker.SetConfig(config); err != nil {
		t.Fatalf("SetConfig: %v", err)
	}
	if actual := worker.currentConfig(); actual != config {
		t.Fatalf("runtime config = %#v, want %#v", actual, config)
	}
	select {
	case <-worker.wake:
	default:
		t.Fatal("configuration update did not wake worker")
	}
}

func TestKeepAliveWorkerStopIsSafeBeforeStart(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "admin.json"), "test-password")
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	worker := NewKeepAliveWorker(store, nil)

	stopped := make(chan struct{})
	go func() {
		worker.Stop()
		close(stopped)
	}()

	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("Stop blocked when called before Start")
	}
}
