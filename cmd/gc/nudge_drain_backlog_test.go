package main

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// installSlowNudgeStoreSeam swaps openNudgeBeadStore for the duration of the
// test so claimDueQueuedNudgesForTarget/Matching -- which always open their
// own store rather than accepting one as a parameter, unlike
// enqueueQueuedNudgeWithStore -- drive the given fake instead of a real
// backing store. Mirrors installCountingNudgeStoreSeam's use of the same seam.
func installSlowNudgeStoreSeam(t *testing.T, store beads.Store) {
	t.Helper()
	prev := openNudgeBeadStore
	openNudgeBeadStore = func(string) beads.NudgesStore {
		return beads.NudgesStore{Store: store}
	}
	t.Cleanup(func() { openNudgeBeadStore = prev })
}

// timeQueuedNudgeClaim seeds a dead backlog, then times a claim pass against
// the given deadline. Reuses backlogSlowStore/seedDeadBacklog from
// sling_nudge_backlog_test.go so the drain path is measured the same way the
// already-fixed enqueue path is.
func timeQueuedNudgeClaim(t *testing.T, backlog int, latency time.Duration, deadline time.Time) (time.Duration, int64) {
	t.Helper()
	cityPath := t.TempDir()
	seedDeadBacklog(t, cityPath, backlog)
	store := &backlogSlowStore{latency: latency}
	installSlowNudgeStoreSeam(t, store)
	start := time.Now()
	if _, err := claimDueQueuedNudgesForTarget(cityPath, nudgeTarget{}, start, deadline); err != nil {
		t.Fatalf("claimDueQueuedNudgesForTarget (backlog=%d): %v", backlog, err)
	}
	return time.Since(start), atomic.LoadInt64(&store.ops)
}

// The gc nudge drain hook (UserPromptSubmit, foreground on every prompt) must
// be bounded regardless of nudge-queue backlog. Before the deadline argument
// was threaded through claimDueQueuedNudgesMatching, it hardcoded
// noMaintenanceDeadline() internally, so a large dead backlog turned a
// sub-second drain into a multi-second (real-world: multi-minute) stall.
func TestNudgeDrainClaimBoundedByBacklog(t *testing.T) {
	const latency = 20 * time.Millisecond
	small, _ := timeQueuedNudgeClaim(t, 40, latency, time.Now().Add(nudgeForegroundMaintenanceBudget))
	big, bigOps := timeQueuedNudgeClaim(t, 160, latency, time.Now().Add(nudgeForegroundMaintenanceBudget))
	t.Logf("drain claim backlog=40 -> %v ; backlog=160 -> %v", small.Round(time.Millisecond), big.Round(time.Millisecond))
	if marginal := big - small; marginal > 2*time.Second {
		t.Fatalf("drain claim grows with backlog: +120 items added %v (>2s). "+
			"Thread the caller's deadline into claimDueQueuedNudgesMatching.", marginal.Round(time.Millisecond))
	}
	if bigOps >= 160 {
		t.Fatalf("slow store ops = %d, want fewer than dead backlog 160 to prove the maintenance budget cut in", bigOps)
	}
}

// The poller (gc nudge poll) is the path that fully retires stale entries so
// the foreground hook never has to -- it must keep draining the full backlog
// every pass. Prove noMaintenanceDeadline() is not silently downgraded to the
// foreground budget now that claimDueQueuedNudgesMatching takes a deadline
// from its caller instead of hardcoding one.
func TestNudgePollerClaimDrainsFullBacklogRegardlessOfBudget(t *testing.T) {
	const backlog = 50
	const latency = 20 * time.Millisecond
	elapsed, ops := timeQueuedNudgeClaim(t, backlog, latency, noMaintenanceDeadline())
	t.Logf("poller claim backlog=%d -> %v, ops=%d", backlog, elapsed.Round(time.Millisecond), ops)
	if ops < int64(backlog) {
		t.Fatalf("slow store ops = %d, want at least backlog %d: the poller must fully drain regardless of the foreground budget", ops, backlog)
	}
}
