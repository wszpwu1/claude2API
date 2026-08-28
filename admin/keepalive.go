package admin

import (
	"context"
	"sync"
	"time"
)

// KeepAliveWorker periodically checks all enabled managed accounts and records
// their latest health status. Configuration changes wake the worker immediately.
type KeepAliveWorker struct {
	store     *Store
	check     func(context.Context, string) error
	mu        sync.RWMutex
	config    KeepAliveConfig
	wake      chan struct{}
	stop      chan struct{}
	done      chan struct{}
	startOnce sync.Once
	stopOnce  sync.Once
}

// NewKeepAliveWorker creates an account keep-alive worker using the persisted
// configuration. Call Start to begin periodic checks.
func NewKeepAliveWorker(store *Store, check func(context.Context, string) error) *KeepAliveWorker {
	return &KeepAliveWorker{
		store:  store,
		check:  check,
		config: store.Snapshot().KeepAlive,
		wake:   make(chan struct{}, 1),
		stop:   make(chan struct{}),
		done:   make(chan struct{}),
	}
}

// Start runs the worker until Stop is called or the parent context is canceled.
// Repeated calls are ignored so only one worker goroutine can own done.
func (w *KeepAliveWorker) Start(ctx context.Context) {
	w.startOnce.Do(func() { go w.run(ctx) })
}

// Stop terminates the worker and waits for it to exit. Starting the worker here
// makes Stop safe even when the caller has not called Start explicitly.
func (w *KeepAliveWorker) Stop() {
	w.startOnce.Do(func() { go w.run(context.Background()) })
	w.stopOnce.Do(func() { close(w.stop) })
	<-w.done
}

// SetConfig applies keep-alive configuration immediately and wakes the worker
// so the new interval or enabled state takes effect without a restart.
func (w *KeepAliveWorker) SetConfig(config KeepAliveConfig) error {
	w.mu.Lock()
	w.config = config
	w.mu.Unlock()
	select {
	case w.wake <- struct{}{}:
	default:
	}
	return nil
}

func (w *KeepAliveWorker) currentConfig() KeepAliveConfig {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.config
}

func (w *KeepAliveWorker) run(ctx context.Context) {
	defer close(w.done)
	for {
		config := w.currentConfig()
		interval := time.Duration(config.IntervalMinutes) * time.Minute
		if interval <= 0 {
			interval = 10 * time.Minute
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return
		case <-w.stop:
			if !timer.Stop() {
				<-timer.C
			}
			return
		case <-w.wake:
			if !timer.Stop() {
				<-timer.C
			}
			continue
		case <-timer.C:
			if config.Enabled {
				w.checkAccounts(ctx, config)
			}
		}
	}
}

func (w *KeepAliveWorker) checkAccounts(parent context.Context, config KeepAliveConfig) {
	if w.check == nil {
		return
	}
	accounts := w.store.Snapshot().Accounts
	for _, account := range accounts {
		if !account.Enabled {
			continue
		}
		timeout := time.Duration(config.TimeoutSeconds) * time.Second
		if timeout <= 0 {
			timeout = 15 * time.Second
		}
		ctx, cancel := context.WithTimeout(parent, timeout)
		err := w.check(ctx, account.ID)
		cancel()

		status := "healthy"
		message := ""
		if err != nil {
			status = "unhealthy"
			message = err.Error()
		}
		now := time.Now().UTC()
		_ = w.store.Update(func(state *PersistentState) error {
			for index := range state.Accounts {
				if state.Accounts[index].ID == account.ID {
					state.Accounts[index].Status = status
					state.Accounts[index].StatusMessage = message
					state.Accounts[index].LastCheckedAt = now
					state.Accounts[index].UpdatedAt = now
					break
				}
			}
			return nil
		})
	}
}
