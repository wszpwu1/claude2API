package admin

import (
	"sync"
	"testing"
	"time"
)

func TestMetricsSnapshotUsesCompletedRequestsForRates(t *testing.T) {
	metrics := NewMetrics()

	finishSuccess := metrics.Begin()
	finishFailure := metrics.Begin()
	_ = metrics.Begin()

	finishSuccess(true)
	finishFailure(false)

	snapshot := metrics.Snapshot()
	if snapshot.Requests != 3 {
		t.Fatalf("requests = %d, want 3", snapshot.Requests)
	}
	if snapshot.Successes != 1 {
		t.Fatalf("successes = %d, want 1", snapshot.Successes)
	}
	if snapshot.Failures != 1 {
		t.Fatalf("failures = %d, want 1", snapshot.Failures)
	}
	if snapshot.ActiveRequests != 1 {
		t.Fatalf("active requests = %d, want 1", snapshot.ActiveRequests)
	}
	if snapshot.SuccessRate != 50 {
		t.Fatalf("success rate = %f, want 50", snapshot.SuccessRate)
	}
}

func TestMetricsCompletionCallbackIsIdempotent(t *testing.T) {
	metrics := NewMetrics()
	finish := metrics.Begin()

	finish(true)
	finish(false)

	snapshot := metrics.Snapshot()
	if snapshot.Requests != 1 || snapshot.Successes != 1 || snapshot.Failures != 0 {
		t.Fatalf("unexpected snapshot after duplicate completion: %#v", snapshot)
	}
	if snapshot.ActiveRequests != 0 {
		t.Fatalf("active requests = %d, want 0", snapshot.ActiveRequests)
	}
}

func TestMetricsConcurrentCollection(t *testing.T) {
	metrics := NewMetrics()
	const requests = 200

	var waitGroup sync.WaitGroup
	waitGroup.Add(requests)
	for index := 0; index < requests; index++ {
		go func(success bool) {
			defer waitGroup.Done()
			finish := metrics.Begin()
			time.Sleep(time.Millisecond)
			finish(success)
		}(index%4 != 0)
	}
	waitGroup.Wait()

	snapshot := metrics.Snapshot()
	if snapshot.Requests != requests {
		t.Fatalf("requests = %d, want %d", snapshot.Requests, requests)
	}
	if snapshot.Successes != 150 || snapshot.Failures != 50 {
		t.Fatalf("successes/failures = %d/%d, want 150/50", snapshot.Successes, snapshot.Failures)
	}
	if snapshot.ActiveRequests != 0 {
		t.Fatalf("active requests = %d, want 0", snapshot.ActiveRequests)
	}
	if snapshot.SuccessRate != 75 {
		t.Fatalf("success rate = %f, want 75", snapshot.SuccessRate)
	}
	if snapshot.AverageLatency < 0 {
		t.Fatalf("average latency = %f, want non-negative", snapshot.AverageLatency)
	}
}
