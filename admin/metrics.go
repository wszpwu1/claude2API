package admin

import (
	"sync/atomic"
	"time"
)

// Metrics records process-wide request activity for the management dashboard.
type Metrics struct {
	requests       atomic.Int64
	successes      atomic.Int64
	failures       atomic.Int64
	activeRequests atomic.Int64
	latencyTotalMS atomic.Int64
}

// NewMetrics creates an empty metrics collector.
func NewMetrics() *Metrics {
	return &Metrics{}
}

// Begin starts timing one request and returns a completion callback.
func (m *Metrics) Begin() func(success bool) {
	startedAt := time.Now()
	m.requests.Add(1)
	m.activeRequests.Add(1)

	var completed atomic.Bool
	return func(success bool) {
		if !completed.CompareAndSwap(false, true) {
			return
		}
		m.activeRequests.Add(-1)
		m.latencyTotalMS.Add(time.Since(startedAt).Milliseconds())
		if success {
			m.successes.Add(1)
		} else {
			m.failures.Add(1)
		}
	}
}

// Snapshot returns a consistent-enough lock-free view for dashboard display.
func (m *Metrics) Snapshot() MetricsSnapshot {
	requests := m.requests.Load()
	successes := m.successes.Load()
	failures := m.failures.Load()
	completed := successes + failures

	var averageLatency float64
	var successRate float64
	if completed > 0 {
		averageLatency = float64(m.latencyTotalMS.Load()) / float64(completed)
		successRate = float64(successes) / float64(completed) * 100
	}

	return MetricsSnapshot{
		Requests:       requests,
		Successes:      successes,
		Failures:       failures,
		ActiveRequests: m.activeRequests.Load(),
		AverageLatency: averageLatency,
		SuccessRate:    successRate,
	}
}
