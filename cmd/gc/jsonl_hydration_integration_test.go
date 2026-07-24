//go:build integration

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// Seam A completion — REAL bd + REAL managed Dolt coverage (dac-y7mg.4). These
// are the tests the unit suite cannot fake: bd 1.1.0's actual import semantics
// against a server-mode store, driven through the real `gc beads materialize`
// funnel. They attack the three claims that decide whether registering a Beads
// v1.1 repo is lossless: an adopted scope's surviving issues.jsonl hydrates to
// the EXACT row count while the file survives byte-identical; a store that
// already has rows is never re-imported; and a failing import aborts non-zero
// with the JSONL intact and the store still empty.
//
// Behind the `integration` build tag (like cmd_beads_materialize_integration_test.go);
// run with `go test -tags integration -run TestBeadsHydration_ ./cmd/gc` on a box
// with dolt+bd on PATH. Registered in scripts/test-integration-shard.

// hydrationFixtureJSONL returns n import-ready JSONL rows for issue prefix, and
// the ids it used. 89 rows is the dac-75f3 acceptance shape (89 → 0 was the
// original wipe), so the default case here is the real one, not a toy.
func hydrationFixtureJSONL(prefix string, n int) (content []byte, ids []string) {
	var b strings.Builder
	for i := 1; i <= n; i++ {
		id := fmt.Sprintf("%s-h%d", prefix, i)
		ids = append(ids, id)
		fmt.Fprintf(&b, `{"_type":"issue","id":%q,"title":"tracked issue %d","issue_type":"task","status":"open","priority":2}`+"\n", id, i)
	}
	return []byte(b.String()), ids
}

// plantTrackedJSONL writes content to scopeRoot/.beads/issues.jsonl and makes it
// genuinely git-tracked — the state a freshly cloned Beads v1.1 repo is in when
// gc adopts it.
func plantTrackedJSONL(t *testing.T, scopeRoot string, content []byte) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(scopeRoot, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	jsonlPath := jsonlExportPath(scopeRoot)
	if err := os.WriteFile(jsonlPath, content, 0o644); err != nil {
		t.Fatal(err)
	}
	gitTrack(t, scopeRoot, jsonlRelPath)
	return jsonlPath
}

func storeHasID(t *testing.T, scopeRoot, cityPath, id string) bool {
	t.Helper()
	store, err := openStoreAtForCity(scopeRoot, cityPath)
	if err != nil {
		t.Fatalf("openStoreAtForCity(%s): %v", scopeRoot, err)
	}
	list, err := store.List(beads.ListQuery{AllowScan: true})
	if err != nil {
		t.Fatalf("List(%s): %v", scopeRoot, err)
	}
	return containsBeadID(list, id)
}

// The headline claim: a scope adopted with a git-tracked issues.jsonl and an
// empty managed server store ends up with EXACTLY the JSONL's rows in Dolt, and
// the file itself byte-identical. Before this seam the same run reported OK with
// 0 rows — bd 1.1.0 stopped auto-importing on init and nothing replaced it.
// Re-running must not duplicate or re-import (idempotent).
func TestBeadsHydration_TrackedJSONLImportsExactRowCountAndSurvives(t *testing.T) {
	cityPath := setupFreshManagedBdWaitTestCity(t)
	failIfProviderReadinessProbed(t)
	rigPath := filepath.Join(cityPath, "frontend")

	const wantRows = 89
	content, ids := hydrationFixtureJSONL("fe", wantRows)
	jsonlPath := plantTrackedJSONL(t, rigPath, content)

	// Materialize the still-deferred rig: init creates the empty store, then the
	// eager import hydrates it — all inside the shared initAndHookDir funnel.
	if code := cmdBeadsMaterialize(beadsMaterializeOptions{rigs: []string{"frontend"}}, os.Stderr, os.Stderr); code != 0 {
		t.Fatalf("cmdBeadsMaterialize(--rig frontend) = %d, want 0", code)
	}

	if got := countRows(t, rigPath, cityPath); got != wantRows {
		t.Fatalf("rig row count after materialize = %d, want %d (the surviving JSONL was not hydrated)", got, wantRows)
	}
	for _, id := range []string{ids[0], ids[len(ids)/2], ids[len(ids)-1]} {
		if !storeHasID(t, rigPath, cityPath, id) {
			t.Fatalf("issue %s from the tracked JSONL is missing from the managed store", id)
		}
	}
	requireFileEquals(t, jsonlPath, content)

	// Idempotent re-run: no re-import (the store now has rows), no duplication,
	// and the tracked mirror still survives byte-identical.
	if code := cmdBeadsMaterialize(beadsMaterializeOptions{rigs: []string{"frontend"}}, os.Stderr, os.Stderr); code != 0 {
		t.Fatalf("re-run cmdBeadsMaterialize(--rig frontend) = %d, want 0", code)
	}
	if got := countRows(t, rigPath, cityPath); got != wantRows {
		t.Fatalf("rig row count after re-materialize = %d, want %d (re-import duplicated or wiped rows)", got, wantRows)
	}
	requireFileEquals(t, jsonlPath, content)
}

// bd import is an upsert. A store that already holds rows is the live copy, so a
// stale mirror on disk must never be replayed over it — not even partially.
func TestBeadsHydration_PopulatedStoreIsNeverReimported(t *testing.T) {
	cityPath := setupFreshManagedBdWaitTestCity(t)
	failIfProviderReadinessProbed(t)
	rigPath := filepath.Join(cityPath, "frontend")

	if code := cmdBeadsMaterialize(beadsMaterializeOptions{rigs: []string{"frontend"}}, os.Stderr, os.Stderr); code != 0 {
		t.Fatalf("cmdBeadsMaterialize(--rig frontend) = %d, want 0", code)
	}
	const seeded = 3
	rigStore, err := openStoreAtForCity(rigPath, cityPath)
	if err != nil {
		t.Fatalf("openStoreAtForCity(rig): %v", err)
	}
	for i := 0; i < seeded; i++ {
		if _, err := rigStore.Create(beads.Bead{Title: "live row", Type: "task"}); err != nil {
			t.Fatalf("seed rig issue %d: %v", i, err)
		}
	}

	// A mirror whose rows are NOT in the store. If the eager import ran, these
	// ids would appear and the count would jump.
	content, ids := hydrationFixtureJSONL("fe", 5)
	jsonlPath := plantTrackedJSONL(t, rigPath, content)

	if code := cmdBeadsMaterialize(beadsMaterializeOptions{rigs: []string{"frontend"}}, os.Stderr, os.Stderr); code != 0 {
		t.Fatalf("re-run cmdBeadsMaterialize(--rig frontend) = %d, want 0", code)
	}

	if got := countRows(t, rigPath, cityPath); got != seeded {
		t.Fatalf("rig row count = %d, want %d (a populated store was re-imported from its JSONL mirror)", got, seeded)
	}
	if storeHasID(t, rigPath, cityPath, ids[0]) {
		t.Fatalf("issue %s was imported into a store that already had rows", ids[0])
	}
	requireFileEquals(t, jsonlPath, content)
}

// Fail-closed against a REAL bd failure (unparseable JSONL): materialize exits
// non-zero, claims no partial success, leaves the file untouched, and leaves the
// store empty — so the operator's retry has exactly the inputs the first attempt
// had. The import runs before the reaping config-sync, so nothing deletes the
// only copy of the data on the way out.
func TestBeadsHydration_ImportFailureFailsClosedWithJSONLIntact(t *testing.T) {
	cityPath := setupFreshManagedBdWaitTestCity(t)
	failIfProviderReadinessProbed(t)
	rigPath := filepath.Join(cityPath, "frontend")

	// The bad line is FIRST, so bd aborts before writing any row.
	content := []byte("this is not json\n" +
		`{"_type":"issue","id":"fe-h1","title":"never imported","issue_type":"task","status":"open"}` + "\n")
	jsonlPath := plantTrackedJSONL(t, rigPath, content)

	if code := cmdBeadsMaterialize(beadsMaterializeOptions{rigs: []string{"frontend"}}, os.Stderr, os.Stderr); code == 0 {
		t.Fatal("cmdBeadsMaterialize(--rig frontend) = 0 with an unimportable JSONL; want non-zero (no partial-success claim)")
	}

	requireFileEquals(t, jsonlPath, content)
	if got := countRows(t, rigPath, cityPath); got != 0 {
		t.Fatalf("rig row count after a failed import = %d, want 0 (no partial hydration)", got)
	}
	if jsonlDeletionAllowed(rigPath, cityPath) {
		t.Fatal("the JSONL became deletable after a failed import; the retry could lose the only copy")
	}
}
