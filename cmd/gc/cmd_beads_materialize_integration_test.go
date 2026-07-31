//go:build integration

package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
)

// Adversarial behavioral coverage for `gc beads materialize` against a REAL
// managed Dolt server and real bd (dac-y7mg.3). Behind the `integration` build
// tag (like native_dolt_rebind_integration_test.go) so the heavy real-Dolt
// env/process harness stays out of the untagged cmd/gc resource census; run
// with `go test -tags integration -run TestBeadsMaterialize_ ./cmd/gc` on a box
// with dolt+bd on PATH. They attack the four load-bearing claims: a fresh
// --no-start city is materialized (hq created) without a supervisor; a rig with
// a git-tracked issues.jsonl keeps its exact row count and a byte-identical
// JSONL across a re-materialize (the dac-75f3 89→0 wipe must NOT recur);
// re-running is idempotent with zero deletions; and no AI-provider readiness
// probe is ever reached.

// setupColdManagedBdCity builds a managed-bd city exactly like
// setupFreshManagedBdWaitTestCity, but STOPS before starting Dolt or hydrating
// any scope — the "gc init --no-start" cold state. Bringing Dolt up and
// hydrating is the job of `gc beads materialize`, which is what the tests
// exercise. Returns (cityPath, rigPath) where rigPath is the scaffold's
// "frontend" rig (prefix "fe").
func setupColdManagedBdCity(t *testing.T) (string, string) {
	t.Helper()
	// The `integration` build tag is the gate; no skipSlowCmdGCTest needed.
	configureIsolatedRuntimeEnv(t)

	bdPath := waitTestRealBDPath(t)
	doltPath, err := exec.LookPath("dolt")
	if err != nil {
		t.Skip("dolt not installed")
	}

	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "")

	homeDir := filepath.Join(shortSocketTempDir(t, "gc-mat-home-"), "home")
	if err := writeWaitTestDoltIdentity(homeDir); err != nil {
		t.Fatalf("writeWaitTestDoltIdentity: %v", err)
	}
	t.Setenv("HOME", homeDir)
	t.Setenv("DOLT_ROOT_PATH", homeDir)
	t.Setenv("PATH", strings.Join([]string{filepath.Dir(bdPath), filepath.Dir(doltPath), os.Getenv("PATH")}, string(os.PathListSeparator)))

	reexecGC := reexecGCTestBinaryForTests(t)
	oldResolve := resolveProviderLifecycleGCBinary
	resolveProviderLifecycleGCBinary = func() string { return reexecGC }
	t.Cleanup(func() { resolveProviderLifecycleGCBinary = oldResolve })

	prevCityFlag, prevRigFlag := cityFlag, rigFlag
	cityFlag, rigFlag = "", ""
	t.Cleanup(func() { cityFlag, rigFlag = prevCityFlag, prevRigFlag })

	cityPath := shortSocketTempDir(t, "gc-mat-city-")
	rigPath, err := writeManagedBdWaitTestCityScaffold(cityPath)
	if err != nil {
		t.Fatalf("writeManagedBdWaitTestCityScaffold: %v", err)
	}
	t.Setenv("GC_CITY", cityPath)
	t.Setenv("GC_CITY_PATH", cityPath)
	materializeBuiltinPacksForTest(t, cityPath)
	// Dolt is deliberately NOT started here — materialize must start it.
	t.Cleanup(func() { _ = shutdownBeadsProvider(cityPath) })
	return cityPath, rigPath
}

// failIfProviderReadinessProbed installs a fatal spy over the AI-provider
// readiness probe for the duration of the test, proving materialize reaches it
// zero times during a full real run.
func failIfProviderReadinessProbed(t *testing.T) {
	t.Helper()
	prev := initProbeProvidersReadiness
	initProbeProvidersReadiness = func(_ context.Context, names []string, _ bool) (map[string]api.ReadinessItem, error) {
		t.Fatalf("materialize probed AI-provider readiness (%v); it must make zero supervisor/session/provider calls", names)
		return nil, nil
	}
	t.Cleanup(func() { initProbeProvidersReadiness = prev })
}

func TestBeadsMaterialize_FreshColdCityCreatesHQWithoutSupervisor(t *testing.T) {
	cityPath, _ := setupColdManagedBdCity(t)
	failIfProviderReadinessProbed(t)

	// Precondition: no managed Dolt is running for this cold city yet.
	if port := currentResolvableManagedDoltPort(cityPath); port != "" {
		t.Fatalf("precondition: managed Dolt already running on port %s for a cold city", port)
	}

	if code := cmdBeadsMaterialize(beadsMaterializeOptions{}, os.Stderr, os.Stderr); code != 0 {
		t.Fatalf("cmdBeadsMaterialize() on a fresh cold city = %d, want 0", code)
	}

	// materialize must have started the managed Dolt server itself.
	if port := currentResolvableManagedDoltPort(cityPath); port == "" {
		t.Fatal("materialize did not bring up a managed Dolt server")
	}
	// hq must exist and be a working store: create + read back a bead.
	store, err := openStoreAtForCity(cityPath, cityPath)
	if err != nil {
		t.Fatalf("openStoreAtForCity(hq): %v", err)
	}
	created, err := store.Create(beads.Bead{Title: "materialize probe", Type: "task"})
	if err != nil {
		t.Fatalf("hq store not writable after materialize: %v", err)
	}
	list, err := store.List(beads.ListQuery{AllowScan: true})
	if err != nil {
		t.Fatalf("hq store not readable after materialize: %v", err)
	}
	if !containsBeadID(list, created.ID) {
		t.Fatalf("hq store missing the bead just created (%s); rows=%d", created.ID, len(list))
	}
}

func TestBeadsMaterialize_RigTrackedJSONLSurvivesReRunByteIdentical(t *testing.T) {
	// A live managed city with hq already hydrated; Dolt is running, so
	// materialize will REUSE it (idempotent ensure) rather than start a new one.
	cityPath := setupFreshManagedBdWaitTestCity(t)
	rigPath := filepath.Join(cityPath, "frontend")

	// Materialize the (still-deferred) frontend rig into a fresh managed store.
	if code := cmdBeadsMaterialize(beadsMaterializeOptions{rigs: []string{"frontend"}}, os.Stderr, os.Stderr); code != 0 {
		t.Fatalf("cmdBeadsMaterialize(--rig frontend) = %d, want 0", code)
	}

	// Seed the rig's managed store with a known number of issues.
	const wantRows = 5
	rigStore, err := openStoreAtForCity(rigPath, cityPath)
	if err != nil {
		t.Fatalf("openStoreAtForCity(rig): %v", err)
	}
	for i := 0; i < wantRows; i++ {
		if _, err := rigStore.Create(beads.Bead{Title: "seed", Type: "task"}); err != nil {
			t.Fatalf("seed rig issue %d: %v", i, err)
		}
	}
	if got := countRows(t, rigPath, cityPath); got != wantRows {
		t.Fatalf("rig row count after seeding = %d, want %d", got, wantRows)
	}

	// Plant a git-tracked issues.jsonl — the "freshly-adopted, tracked mirror"
	// state. Its content is inert here (bd only auto-imports into an EMPTY db,
	// and the store already has rows), so this isolates the survival guarantee:
	// a re-materialize must never reap a git-tracked export (Seam A).
	jsonlPath := jsonlExportPath(rigPath)
	trackedContent := []byte(`{"_type":"issue","id":"fe-1","title":"tracked mirror"}` + "\n" +
		`{"_type":"issue","id":"fe-2","title":"tracked mirror"}` + "\n")
	if err := os.WriteFile(jsonlPath, trackedContent, 0o644); err != nil {
		t.Fatal(err)
	}
	gitTrack(t, rigPath, jsonlRelPath)

	// Both Seam A deleters consult jsonlDeletionAllowed; a git-tracked export
	// must gate them off. Asserting the shared gate is a direct "zero-call on
	// both JSONL deleters" for this scope.
	if jsonlDeletionAllowed(rigPath, cityPath) {
		t.Fatal("jsonlDeletionAllowed = true for a git-tracked export; both Seam A deleters would be free to delete it")
	}

	// Re-run: idempotent, and it must NOT wipe rows (the dac-75f3 89→0 regression)
	// nor reap the tracked JSONL.
	if code := cmdBeadsMaterialize(beadsMaterializeOptions{rigs: []string{"frontend"}}, os.Stderr, os.Stderr); code != 0 {
		t.Fatalf("re-run cmdBeadsMaterialize(--rig frontend) = %d, want 0", code)
	}

	if got := countRows(t, rigPath, cityPath); got != wantRows {
		t.Fatalf("rig row count after re-materialize = %d, want %d (rows were wiped/altered)", got, wantRows)
	}
	requireFileEquals(t, jsonlPath, trackedContent) // byte-identical survival
	if jsonlDeletionAllowed(rigPath, cityPath) {
		t.Fatal("tracked export became deletable after re-materialize")
	}
}

func containsBeadID(list []beads.Bead, id string) bool {
	for _, b := range list {
		if b.ID == id {
			return true
		}
	}
	return false
}

// censusQuery lists EVERY row a scope's store holds, in every tier.
//
// beads.ListQuery{AllowScan: true} is a working query, not a census: bd is
// invoked without --all (so closed rows never come back) and the client-side
// filter additionally drops closed and ephemeral rows. Counting with it made
// these tests pass for the wrong reason — they seed open rows, so the filtering
// was invisible, and an assertion like "row count after a failed import = 0"
// would have held even if the import had landed a hundred CLOSED rows. Same
// defect as the Seam A row-count probe (see managedStoreRowProbeQuery).
func censusQuery() beads.ListQuery {
	return beads.ListQuery{AllowScan: true, IncludeClosed: true, TierMode: beads.TierBoth}
}

func countRows(t *testing.T, storePath, cityPath string) int {
	t.Helper()
	store, err := openStoreAtForCity(storePath, cityPath)
	if err != nil {
		t.Fatalf("openStoreAtForCity(%s): %v", storePath, err)
	}
	list, err := store.List(censusQuery())
	if err != nil {
		t.Fatalf("List(%s): %v", storePath, err)
	}
	return len(list)
}
