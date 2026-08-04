package main

import (
	"io"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// closeGateFixture builds a city-scoped session bead in a live store plus one
// work bead assigned to it, and returns the store, the session's front-door
// Info, and the work bead's ID. Work is seeded verbatim through the store so
// the guard's own live query is what decides — no pre-computed snapshot.
func closeGateFixture(t *testing.T, workStatus string) (beads.Store, *config.City, sessionpkg.Info, string) {
	t.Helper()
	cfg := &config.City{
		Agents: []config.Agent{{Name: "auditor", Scope: "city"}},
	}
	store := beads.NewMemStore()
	sessionBead, err := store.Create(beads.Bead{
		Type:   sessionBeadType,
		Status: "open",
		Metadata: map[string]string{
			"template":     "auditor",
			"session_name": "auditor-session",
		},
	})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}
	work, err := store.Create(beads.Bead{
		Type:     "task",
		Status:   "open",
		Assignee: sessionBead.ID,
	})
	if err != nil {
		t.Fatalf("create work bead: %v", err)
	}
	if workStatus != "open" {
		if err := store.Update(work.ID, beads.UpdateOpts{Status: &workStatus}); err != nil {
			t.Fatalf("set work status %q: %v", workStatus, err)
		}
	}
	info, err := sessionFrontDoor(store).Get(sessionBead.ID)
	if err != nil {
		t.Fatalf("front-door Get(%q): %v", sessionBead.ID, err)
	}
	return store, cfg, info, work.ID
}

// blockWork adds a real open blocker and a "blocks" dependency edge, so the
// work bead is genuinely absent from the store's Ready projection rather than
// merely labeled blocked. Returns the blocker's ID.
func blockWork(t *testing.T, store beads.Store, workID string) string {
	t.Helper()
	blocker, err := store.Create(beads.Bead{Type: "task", Status: "open"})
	if err != nil {
		t.Fatalf("create blocker: %v", err)
	}
	if err := store.DepAdd(workID, blocker.ID, "blocks"); err != nil {
		t.Fatalf("DepAdd(%q -> %q): %v", workID, blocker.ID, err)
	}
	// Prove the edge actually took: the work bead must not be Ready. Without
	// this the whole test could pass vacuously against a store that ignores deps.
	ready, err := store.Ready(beads.ReadyQuery{Assignee: ""})
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	for _, b := range ready {
		if b.ID == workID {
			t.Fatalf("fixture is vacuous: %q is still Ready after DepAdd on open blocker %q", workID, blocker.ID)
		}
	}
	return blocker.ID
}

// TestCloseGateReleasesSessionHeldOnlyByBlockedWork pins the fix for the
// ready-blind sleep gate.
//
// A blocked open bead assigned to a session is work that session provably
// cannot claim: the wake gate already declines to wake for it. When the close
// gate treated mere openness as "assigned", the two gates disagreed about the
// same bead and the session could neither start it nor be released — it sat
// awake and the runtime relaunched the agent on a ~20s cycle until the model
// provider cut it off (two full 5-hour quota windows, 658 sessions, zero work).
//
// The close must also not orphan the bead: closeBead's release cascade has to
// hand the work back unassigned so it is routable once it unblocks.
func TestCloseGateReleasesSessionHeldOnlyByBlockedWork(t *testing.T) {
	store, cfg, info, workID := closeGateFixture(t, "open")
	blockerID := blockWork(t, store, workID)

	if !closeSessionBeadIfReachableStoreUnassigned("", cfg, store, nil, info, "drained", time.Now().UTC(), io.Discard) {
		t.Fatal("close gate refused to release a session whose only assigned work is BLOCKED; the session is stranded awake with work it can never start")
	}

	closed, err := store.Get(info.ID)
	if err != nil {
		t.Fatalf("Get session bead: %v", err)
	}
	if closed.Status != "closed" {
		t.Fatalf("session bead status = %q, want closed", closed.Status)
	}

	// The release cascade must have handed the work back, or we traded a
	// stranded session for a bead assigned to a dead one.
	work, err := store.Get(workID)
	if err != nil {
		t.Fatalf("Get work bead: %v", err)
	}
	if work.Assignee == info.ID {
		t.Fatalf("work %q is still assigned to the closed session %q: release cascade did not run", workID, info.ID)
	}
	if work.Status == "closed" {
		t.Fatalf("work %q was closed by the session release; the blocked bead must survive", workID)
	}

	// The gate edge itself must be untouched — releasing a session is not a
	// license to unblock its work.
	deps, err := store.DepList(workID, "")
	if err != nil {
		t.Fatalf("DepList: %v", err)
	}
	found := false
	for _, d := range deps {
		if d.DependsOnID == blockerID {
			found = true
		}
	}
	if !found {
		t.Fatalf("blocking edge %q -> %q was dropped by the close", workID, blockerID)
	}
}

// TestCloseGateStillRefusesForInProgressWork is the orphan protection this
// guard exists for. Claimed work means an agent is mid-task; closing the
// session would strand the bead in_progress behind a dead assignee. The fix
// must narrow the gate to unstartable work only, never disarm it.
func TestCloseGateStillRefusesForInProgressWork(t *testing.T) {
	store, cfg, info, workID := closeGateFixture(t, "in_progress")

	if closeSessionBeadIfReachableStoreUnassigned("", cfg, store, nil, info, "drained", time.Now().UTC(), io.Discard) {
		t.Fatalf("close gate released a session still holding in_progress work %q", workID)
	}
	live, err := store.Get(info.ID)
	if err != nil {
		t.Fatalf("Get session bead: %v", err)
	}
	if live.Status == "closed" {
		t.Fatal("session bead was closed despite in_progress assigned work")
	}
}

// TestCloseGateStillRefusesForReadyOpenWork covers the other half of the
// invariant: open work that IS startable (unblocked, undeferred) is exactly
// what the session was woken for, so it must keep the session open. Passing
// this while the blocked-work test also passes is what proves the gate was
// narrowed rather than removed.
func TestCloseGateStillRefusesForReadyOpenWork(t *testing.T) {
	store, cfg, info, workID := closeGateFixture(t, "open")

	// No blocker: the bead is genuinely Ready. Assert that before relying on it.
	ready, err := store.Ready(beads.ReadyQuery{})
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	isReady := false
	for _, b := range ready {
		if b.ID == workID {
			isReady = true
		}
	}
	if !isReady {
		t.Fatalf("fixture is vacuous: work %q is not Ready, so this test cannot detect an over-broad gate", workID)
	}

	if closeSessionBeadIfReachableStoreUnassigned("", cfg, store, nil, info, "drained", time.Now().UTC(), io.Discard) {
		t.Fatalf("close gate released a session holding READY open work %q", workID)
	}
}
