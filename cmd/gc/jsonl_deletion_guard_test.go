package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/git"
)

// Seam A adversarial tests (dac-y7mg.1). These attack the lossless-adopt
// contract: prove the JSONL deleters refuse to delete a git-tracked or
// not-yet-hydrated export, that hydration runs strictly before the reaping
// config-sync, that a hydration failure never reaches the delete path, and
// that a preseeded explicit origin survives managed canonicalization.

// stubJSONLDeletionSeams overrides both Seam A gate probes for the test:
// git-tracked → (tracked, nil), managed row-count → (hasRows, ok). Restored on
// cleanup.
//
// Stubbing the row probe deliberately blinds the test to the QUERY the real
// probe issues — which is how the closed/ephemeral filtering defect survived
// this suite. TestManagedStoreRowProbeQuery* below covers that hook-free.
func stubJSONLDeletionSeams(t *testing.T, tracked, hasRows, ok bool) {
	t.Helper()
	jsonlIsGitTrackedHook = func(string) (bool, error) { return tracked, nil }
	scopeHasManagedRowsHook = func(_, _ string) (bool, bool) { return hasRows, ok }
	t.Cleanup(func() {
		jsonlIsGitTrackedHook = nil
		scopeHasManagedRowsHook = nil
	})
}

// stubManagedRows overrides only the row-count probe, leaving the real git
// ls-files check in force so tests can attack it with an actual repository.
func stubManagedRows(t *testing.T, hasRows, ok bool) {
	t.Helper()
	scopeHasManagedRowsHook = func(_, _ string) (bool, bool) { return hasRows, ok }
	t.Cleanup(func() { scopeHasManagedRowsHook = nil })
}

// countingManagedRowsStub is stubManagedRows plus a call counter, for zero-call
// invariants: a gate that rejects on a cheaper precondition must never reach
// the store at all.
func countingManagedRowsStub(t *testing.T, hasRows, ok bool) *int {
	t.Helper()
	calls := 0
	scopeHasManagedRowsHook = func(_, _ string) (bool, bool) {
		calls++
		return hasRows, ok
	}
	t.Cleanup(func() { scopeHasManagedRowsHook = nil })
	return &calls
}

// writeCanonicalMetadata gives scopeRoot the .beads/metadata.json that pins
// WHICH database bd addresses for it. Without this file bd resolves the
// server's DEFAULT database, so both Seam A gates refuse to act on the scope.
func writeCanonicalMetadata(t *testing.T, scopeRoot string) {
	t.Helper()
	beadsDir := filepath.Join(scopeRoot, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"),
		[]byte(`{"database":"beads_test_scope","backend":"dolt"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if !canonicalScopeStoreInitialized(scopeRoot) {
		t.Fatal("canonicalScopeStoreInitialized = false after writing metadata.json; test precondition broken")
	}
}

// stubInitAndHookDirSteps swaps the hydrate/normalize seams of initAndHookDir so
// a test can observe their order and count. Restored on cleanup.
func stubInitAndHookDirSteps(t *testing.T, hydrate, normalize func(cityPath, dir, prefix, doltDatabase string) error) {
	t.Helper()
	origHydrate := initAndHookDirInitBeads
	origNormalize := initAndHookDirNormalizeScopeFiles
	initAndHookDirInitBeads = hydrate
	initAndHookDirNormalizeScopeFiles = normalize
	t.Cleanup(func() {
		initAndHookDirInitBeads = origHydrate
		initAndHookDirNormalizeScopeFiles = origNormalize
	})
}

// writeManagedScope materializes a managed-origin scope that gc has already
// initialized (canonical metadata.json present) with a stale JSONL export, and
// returns (scopeRoot, jsonlPath, content). The metadata is part of the fixture
// so these tests exercise the git and row-count gates rather than
// short-circuiting on the metadata precondition.
func writeManagedScope(t *testing.T) (scope, jsonlPath string, content []byte) {
	t.Helper()
	scope = t.TempDir()
	beadsDir := filepath.Join(scope, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	jsonlPath = filepath.Join(beadsDir, "issues.jsonl")
	content = []byte(`{"_type":"issue","id":"elt-1","title":"keep me"}` + "\n")
	if err := os.WriteFile(jsonlPath, content, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(beadsDir, "config.yaml"),
		[]byte("issue_prefix: elt\ngc.endpoint_origin: managed_city\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeCanonicalMetadata(t, scope)
	return scope, jsonlPath, content
}

// gitTrack makes relPath genuinely git ls-files-tracked inside repoDir using a
// real git process (no stub), so the git-tracked gate is exercised for real. It
// drives git through internal/git rather than a bare exec.Command so it adds no
// new subprocess call site to the repository resource census.
//
//nolint:unparam // relPath is fixed today (.beads/issues.jsonl) but the helper's contract is per-path
func gitTrack(t *testing.T, repoDir, relPath string) {
	t.Helper()
	g := git.New(repoDir)
	if err := g.Init(); err != nil {
		t.Fatalf("git init: %v", err)
	}
	if err := g.Add(relPath); err != nil {
		t.Fatalf("git add %s: %v", relPath, err)
	}
	// Sanity: the real probe must now agree the path is tracked.
	tracked, err := g.IsTracked(relPath)
	if err != nil || !tracked {
		t.Fatalf("git.IsTracked(%q) = (%v, %v) after add; test precondition broken", relPath, tracked, err)
	}
}

func requireFileEquals(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("issues.jsonl missing (reaped): %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("issues.jsonl changed: got %q want %q", got, want)
	}
}

func TestJSONLDeletionAllowedTruthTable(t *testing.T) {
	cases := []struct {
		name                   string
		tracked, hasRows, okDB bool
		want                   bool
	}{
		{"git-tracked blocks even with a populated store", true, true, true, false},
		{"untracked + rows>0 permits deletion", false, true, true, true},
		{"untracked + row-count 0 blocks", false, false, true, false},
		{"untracked + unprovable row-count blocks (fail-closed)", false, false, false, false},
		{"git-tracked + empty store blocks", true, false, true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			scope := t.TempDir()
			writeCanonicalMetadata(t, scope)
			stubJSONLDeletionSeams(t, c.tracked, c.hasRows, c.okDB)
			if got := jsonlDeletionAllowed(scope, "/city"); got != c.want {
				t.Fatalf("jsonlDeletionAllowed = %v, want %v", got, c.want)
			}
		})
	}
	t.Run("empty scopeRoot blocks", func(t *testing.T) {
		stubJSONLDeletionSeams(t, false, true, true)
		if jsonlDeletionAllowed("", "/city") {
			t.Fatal("empty scopeRoot must block deletion")
		}
	})
}

// The metadata precondition, which the deletion gate previously lacked while
// its hydration sibling enforced it.
//
// A scope with no canonical .beads/metadata.json has no pinned database, so bd
// resolves the SERVER'S DEFAULT one. The row-count probe then answers about a
// FOREIGN database — and a populated foreign database would authorize deleting
// this scope's only copy of its issues. The old comment on the gate claimed a
// misresolved endpoint "surfaces as a list error → deletion denied, never data
// loss"; hydration's verified note (bd warns "no beads configuration found …
// using default database name beads" and then answers) says otherwise.
//
// Both halves are asserted: the gate must deny, and it must deny WITHOUT
// consulting the store at all — a zero-call invariant, because reaching the
// store is itself the act of asking the wrong database.
func TestJSONLDeletionDeniedWithoutCanonicalMetadata(t *testing.T) {
	scope := t.TempDir()
	if err := os.MkdirAll(filepath.Join(scope, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	jsonlIsGitTrackedHook = func(string) (bool, error) { return false, nil }
	t.Cleanup(func() { jsonlIsGitTrackedHook = nil })
	// The most dangerous configuration: the (foreign) store reports rows.
	probes := countingManagedRowsStub(t, true /*hasRows*/, true /*ok*/)

	if jsonlDeletionAllowed(scope, "/city") {
		t.Fatal("deletion allowed for a scope with no canonical metadata.json; a populated FOREIGN database authorized wiping this scope's only copy")
	}
	if *probes != 0 {
		t.Fatalf("row-count probe ran %d times on an unpinned scope; want 0 — the probe would be addressing the server's default database", *probes)
	}
}

// Neither deleter may touch the file for an unpinned scope either — the gate is
// only useful if the call sites honor it.
func TestDeletersPreserveExportOnUnpinnedScope(t *testing.T) {
	newUnpinnedScope := func(t *testing.T) (scope, jsonlPath string, content []byte) {
		t.Helper()
		scope, jsonlPath, content = writeManagedScope(t)
		if err := os.Remove(filepath.Join(scope, ".beads", "metadata.json")); err != nil {
			t.Fatal(err)
		}
		return scope, jsonlPath, content
	}
	t.Run("reapStaleBdExportJSONL", func(t *testing.T) {
		stubJSONLDeletionSeams(t, false /*tracked*/, true /*hasRows*/, true /*ok*/)
		scope, jsonlPath, content := newUnpinnedScope(t)
		reapStaleBdExportJSONL(scope, scope)
		requireFileEquals(t, jsonlPath, content)
	})
	t.Run("removeStaleBdExportJSONL", func(t *testing.T) {
		stubJSONLDeletionSeams(t, false, true, true)
		scope, jsonlPath, content := newUnpinnedScope(t)
		removeStaleBdExportJSONL(fsys.OSFS{}, scope, scope)
		requireFileEquals(t, jsonlPath, content)
	})
}

// The git half of the gate must fail CLOSED, matching the row-count half.
//
// Driven by a REAL git that genuinely cannot answer: the repo is intact and the
// file is genuinely tracked, but the index is destroyed, so `git ls-files`
// exits 128. Under the old `return err == nil` probe that collapsed to "not
// tracked" and — with a populated store — authorized deleting a committed
// source-of-truth mirror.
func TestJSONLDeletionDeniedWhenGitCannotAnswer(t *testing.T) {
	newBrokenGitScope := func(t *testing.T) (scope, jsonlPath string, content []byte) {
		t.Helper()
		scope, jsonlPath, content = writeManagedScope(t)
		gitTrack(t, scope, jsonlRelPath) // the file really IS tracked
		if err := os.WriteFile(filepath.Join(scope, ".git", "index"), []byte("garbage"), 0o644); err != nil {
			t.Fatal(err)
		}
		return scope, jsonlPath, content
	}

	t.Run("gate denies", func(t *testing.T) {
		scope, _, _ := newBrokenGitScope(t)
		stubManagedRows(t, true /*hasRows*/, true /*ok*/) // the tempting branch
		if jsonlDeletionAllowed(scope, scope) {
			t.Fatal("deletion allowed while git could not answer; the tracked-mirror half of the gate failed OPEN")
		}
	})
	t.Run("reapStaleBdExportJSONL preserves", func(t *testing.T) {
		scope, jsonlPath, content := newBrokenGitScope(t)
		stubManagedRows(t, true, true)
		reapStaleBdExportJSONL(scope, scope)
		requireFileEquals(t, jsonlPath, content)
	})
	t.Run("removeStaleBdExportJSONL preserves", func(t *testing.T) {
		scope, jsonlPath, content := newBrokenGitScope(t)
		stubManagedRows(t, true, true)
		removeStaleBdExportJSONL(fsys.OSFS{}, scope, scope)
		requireFileEquals(t, jsonlPath, content)
	})
}

// A managed scope whose issues.jsonl is git-tracked must survive reap
// byte-identical — even when the managed store reports rows. Uses a real repo.
func TestReapStaleBdExportJSONLPreservesGitTrackedExport(t *testing.T) {
	scope, jsonlPath, content := writeManagedScope(t)
	gitTrack(t, scope, jsonlRelPath)
	stubManagedRows(t, true /*hasRows*/, true /*ok*/) // prove tracked alone blocks

	reapStaleBdExportJSONL(scope, scope)

	requireFileEquals(t, jsonlPath, content)
}

// The same tracked-export invariant for the OTHER deleter (removeStaleBdExportJSONL).
func TestRemoveStaleBdExportJSONLPreservesGitTrackedExport(t *testing.T) {
	scope, jsonlPath, content := writeManagedScope(t)
	gitTrack(t, scope, jsonlRelPath)
	stubManagedRows(t, true, true)

	removeStaleBdExportJSONL(fsys.OSFS{}, scope, scope)

	requireFileEquals(t, jsonlPath, content)
}

// An untracked export backed by an EMPTY managed Dolt (row-count 0 — the
// pre-hydration / failed-hydration state) must not be deleted by either deleter.
func TestDeletersPreserveEmptyDoltExport(t *testing.T) {
	t.Run("reapStaleBdExportJSONL", func(t *testing.T) {
		stubJSONLDeletionSeams(t, false /*tracked*/, false /*hasRows*/, true /*ok*/)
		scope, jsonlPath, content := writeManagedScope(t)
		reapStaleBdExportJSONL(scope, scope)
		requireFileEquals(t, jsonlPath, content)
	})
	t.Run("removeStaleBdExportJSONL", func(t *testing.T) {
		stubJSONLDeletionSeams(t, false, false, true)
		scope, jsonlPath, content := writeManagedScope(t)
		removeStaleBdExportJSONL(fsys.OSFS{}, scope, scope)
		requireFileEquals(t, jsonlPath, content)
	})
}

// Unprovable row-count (store unopenable) must also fail closed for both deleters.
func TestDeletersFailClosedWhenRowCountUnprovable(t *testing.T) {
	t.Run("reapStaleBdExportJSONL", func(t *testing.T) {
		stubJSONLDeletionSeams(t, false, false, false /*ok=false*/)
		scope, jsonlPath, content := writeManagedScope(t)
		reapStaleBdExportJSONL(scope, scope)
		requireFileEquals(t, jsonlPath, content)
	})
	t.Run("removeStaleBdExportJSONL", func(t *testing.T) {
		stubJSONLDeletionSeams(t, false, false, false)
		scope, jsonlPath, content := writeManagedScope(t)
		removeStaleBdExportJSONL(fsys.OSFS{}, scope, scope)
		requireFileEquals(t, jsonlPath, content)
	})
}

// Re-adopt idempotency: repeatedly reaping a git-tracked, populated scope never
// wipes the export (a second adopt of an already-hydrated store is a no-op).
func TestReapIdempotentNeverRewipesTrackedExport(t *testing.T) {
	scope, jsonlPath, content := writeManagedScope(t)
	gitTrack(t, scope, jsonlRelPath)
	stubManagedRows(t, true, true)

	for i := 0; i < 3; i++ {
		reapStaleBdExportJSONL(scope, scope)
		requireFileEquals(t, jsonlPath, content)
	}
}

// initAndHookDir must run hydration (bd init) strictly before the canonical
// config-sync/reap, so a surviving JSONL is imported before anything can delete it.
func TestInitAndHookDirHydratesBeforeNormalize(t *testing.T) {
	var order []string
	stubInitAndHookDirSteps(
		t,
		func(_, _, _, _ string) error { order = append(order, "hydrate"); return nil },
		func(_, _, _, _ string) error { order = append(order, "normalize"); return nil },
	)
	city := t.TempDir()

	if err := initAndHookDir(city, city, "gc"); err != nil {
		t.Fatalf("initAndHookDir: %v", err)
	}
	if want := []string{"hydrate", "normalize"}; !reflect.DeepEqual(order, want) {
		t.Fatalf("step order = %v, want %v (hydrate strictly before normalize/reap)", order, want)
	}
}

// Fail-closed: when hydration errors, the reaping config-sync is never reached —
// a zero-call invariant, not merely an end-state check. The JSONL is untouched
// because the delete path never runs.
func TestInitAndHookDirFailsClosedOnHydrationError(t *testing.T) {
	wantErr := errors.New("bd init: hydration boom")
	var normalizeCalls int
	stubInitAndHookDirSteps(
		t,
		func(_, _, _, _ string) error { return wantErr },
		func(_, _, _, _ string) error { normalizeCalls++; return nil },
	)
	city := t.TempDir()

	err := initAndHookDir(city, city, "gc")
	if !errors.Is(err, wantErr) {
		t.Fatalf("initAndHookDir err = %v, want %v", err, wantErr)
	}
	if normalizeCalls != 0 {
		t.Fatalf("normalize/reap ran %d times after a hydration failure; want 0 (fail-closed)", normalizeCalls)
	}
}

func writeScopeConfig(t *testing.T, scopeRoot, yaml string) {
	t.Helper()
	beadsDir := filepath.Join(scopeRoot, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(beadsDir, "config.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Seam A item 5 (spike 002 §4): a preseeded endpoint origin survives init/adopt
// canonicalization ONLY when it is a *valid* explicit config. The resolver that
// the adopt path uses to compute the desired state returns a valid explicit
// origin unchanged (authoritative), so EnsureCanonicalConfig writes explicit —
// never a managed downgrade before registration. That is what makes explicit /
// JSONL-of-record coexistence durable. A stale or illegal explicit (e.g. on a
// city scope, where explicit is invalid) is deliberately NOT authoritative, so
// canonicalization must be free to normalize it away rather than preserve an
// invalid state — the exact regression a blanket "always preserve explicit"
// would cause.
func TestExplicitEndpointOriginPreservedOnlyWhenValid(t *testing.T) {
	t.Run("valid explicit rig config is preserved (authoritative)", func(t *testing.T) {
		cityRoot := t.TempDir()
		scopeRoot := filepath.Join(cityRoot, "rig")
		writeScopeConfig(t, scopeRoot,
			"issue_prefix: rig\ngc.endpoint_origin: explicit\ngc.endpoint_status: verified\ndolt.host: db.example.com\ndolt.port: 4406\ndolt.user: agent\n")

		res, err := contract.ResolveScopeConfigState(fsys.OSFS{}, cityRoot, scopeRoot, "rig")
		if err != nil {
			t.Fatalf("ResolveScopeConfigState: %v", err)
		}
		if res.Kind != contract.ScopeConfigAuthoritative || res.State.EndpointOrigin != contract.EndpointOriginExplicit {
			t.Fatalf("kind=%q origin=%q, want authoritative + explicit (valid explicit must survive registration)", res.Kind, res.State.EndpointOrigin)
		}
	})

	t.Run("illegal explicit on a city scope is rejected, not preserved", func(t *testing.T) {
		cityRoot := t.TempDir()
		writeScopeConfig(t, cityRoot,
			"issue_prefix: mc\ngc.endpoint_origin: explicit\ngc.endpoint_status: unverified\ndolt.host: db.example.com\ndolt.port: 4406\n")

		if _, err := contract.ResolveScopeConfigState(fsys.OSFS{}, cityRoot, cityRoot, "mc"); err == nil {
			t.Fatal("want rejection of an explicit city scope; got nil — canonicalization must be able to normalize invalid explicit")
		}
	})
}

// ---------------------------------------------------------------------------
// The row-count probe's QUERY — covered without the scopeHasManagedRowsHook.
//
// Every other test in this package stubs that hook, which is precisely why the
// defect below shipped: with the probe faked, the query it issues is invisible
// and any filtering bug in it is unobservable. These tests drive a REAL
// beads.BdStore whose CommandRunner emulates bd's actual list semantics, so the
// query is exercised end to end.
// ---------------------------------------------------------------------------

// bdListEmulator is a CommandRunner standing in for a bd CLI backed by a store
// whose ONLY rows are the ones given. It reproduces the two behaviors the
// defect turned on:
//
//   - `bd list` withholds closed rows unless --all is passed;
//   - `bd list` never returns ephemeral rows at all — they are reachable only
//     via `bd query "ephemeral=true"`.
//
// It records every argv it is handed.
type bdListEmulator struct {
	closedIssuesJSON string // served by `bd list` ONLY when --all is present
	openIssuesJSON   string // served by `bd list` always
	ephemeralJSON    string // served by `bd query ephemeral=true`
	calls            [][]string
}

func (e *bdListEmulator) runner() beads.CommandRunner {
	return func(_, _ string, args ...string) ([]byte, error) {
		e.calls = append(e.calls, args)
		if len(args) == 0 {
			return []byte("[]"), nil
		}
		switch args[0] {
		case "list":
			rows := e.openIssuesJSON
			if slices.Contains(args, "--all") {
				rows = joinJSONRows(rows, e.closedIssuesJSON)
			}
			return []byte("[" + rows + "]"), nil
		case "query":
			rows := e.ephemeralJSON
			if !slices.Contains(args, "--all") {
				rows = "" // closed wisps hidden the same way
			}
			return []byte("[" + rows + "]"), nil
		}
		return []byte("[]"), nil
	}
}

func (e *bdListEmulator) sawFlag(subcommand, flag string) bool {
	for _, args := range e.calls {
		if len(args) > 0 && args[0] == subcommand && slices.Contains(args, flag) {
			return true
		}
	}
	return false
}

func joinJSONRows(rows ...string) string {
	kept := make([]string, 0, len(rows))
	for _, r := range rows {
		if strings.TrimSpace(r) != "" {
			kept = append(kept, r)
		}
	}
	return strings.Join(kept, ",")
}

func (e *bdListEmulator) store(t *testing.T) *beads.BdStore {
	t.Helper()
	return beads.NewBdStoreWithPrefix(t.TempDir(), e.runner(), "zz")
}

const (
	closedIssueRow    = `{"id":"zz-1","title":"finished work","status":"closed","issue_type":"task"}`
	ephemeralWispRow  = `{"id":"zz-w1","title":"session slot","status":"open","issue_type":"session","ephemeral":true}`
	legacyProbeReason = "the pre-fix probe (ListQuery{AllowScan:true, Limit:1}) reported this store EMPTY"
)

// legacyProbeQuery is the query this guard used to issue. Kept in the test as
// the control: each case asserts the fixed query sees the rows AND that the old
// one did not, so these tests provably catch the regression rather than merely
// agreeing with the current code.
func legacyProbeQuery() beads.ListQuery {
	return beads.ListQuery{AllowScan: true, Limit: 1}
}

// A store holding only CLOSED issues — a finished rig — is populated. Reporting
// it empty made hydration `bd import` a stale mirror over live rows on every
// controller boot, every `gc rig add --adopt` and every `gc beads materialize`.
func TestManagedStoreRowProbeQuerySeesAnAllClosedStore(t *testing.T) {
	emu := &bdListEmulator{closedIssuesJSON: closedIssueRow}
	store := emu.store(t)

	legacy, err := store.List(legacyProbeQuery())
	if err != nil {
		t.Fatalf("legacy probe List: %v", err)
	}
	if len(legacy) != 0 {
		t.Fatalf("control broken: legacy probe returned %d rows, want 0 — this test can no longer detect the regression", len(legacy))
	}

	got, err := store.List(managedStoreRowProbeQuery())
	if err != nil {
		t.Fatalf("probe List: %v", err)
	}
	if len(got) == 0 {
		t.Fatalf("an all-closed store probes as EMPTY; %s and the fixed query still does", legacyProbeReason)
	}
	if !emu.sawFlag("list", "--all") {
		t.Fatal("probe issued `bd list` without --all; bd withholds closed rows without it, so the row count is a lie")
	}
}

// Same defect, the other filter: matchesTier drops ephemeral rows under the
// default TierIssues, and `bd list` does not return them at all.
func TestManagedStoreRowProbeQuerySeesAnEphemeralOnlyStore(t *testing.T) {
	emu := &bdListEmulator{ephemeralJSON: ephemeralWispRow}
	store := emu.store(t)

	legacy, err := store.List(legacyProbeQuery())
	if err != nil {
		t.Fatalf("legacy probe List: %v", err)
	}
	if len(legacy) != 0 {
		t.Fatalf("control broken: legacy probe returned %d rows, want 0", len(legacy))
	}

	got, err := store.List(managedStoreRowProbeQuery())
	if err != nil {
		t.Fatalf("probe List: %v", err)
	}
	if len(got) == 0 {
		t.Fatalf("an ephemeral-only store probes as EMPTY; %s", legacyProbeReason)
	}
	if !emu.sawFlag("query", "--all") {
		t.Fatal("probe never issued the ephemeral-tier `bd query`; wisps are unreachable through `bd list`")
	}
}

// A genuinely empty store must still probe empty — the fix must not simply
// force the answer to "populated", which would disable hydration entirely.
func TestManagedStoreRowProbeQueryStillReportsAnEmptyStoreEmpty(t *testing.T) {
	emu := &bdListEmulator{}
	got, err := emu.store(t).List(managedStoreRowProbeQuery())
	if err != nil {
		t.Fatalf("probe List: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("empty store probed as %d rows, want 0 — hydration would never run again", len(got))
	}
}

// Composition: the fixed query's verdict for an all-closed store, fed to the
// gate that consumes it, must suppress the import. This closes the loop the two
// halves would otherwise leave open (correct query + correct branch, never
// joined). The import hook is asserted zero-call: the point is that nothing
// happens.
func TestHydrationSkipsImportForAnAllClosedStore(t *testing.T) {
	emu := &bdListEmulator{closedIssuesJSON: closedIssueRow}
	rows, err := emu.store(t).List(managedStoreRowProbeQuery())
	if err != nil {
		t.Fatalf("probe List: %v", err)
	}
	hasRows := len(rows) > 0

	scope, jsonlPath, content := hydrationScope(t)
	stubManagedRows(t, hasRows, true)
	calls := stubHydrationImport(t, []byte("Imported 2 issues\n"), nil)

	if err := hydrateScopeFromSurvivingJSONL(scope, scope); err != nil {
		t.Fatalf("hydrateScopeFromSurvivingJSONL: %v", err)
	}
	if len(*calls) != 0 {
		t.Fatalf("bd import ran %d times against a store full of closed rows; want 0 — an upsert from a stale mirror over live data", len(*calls))
	}
	requireFileEquals(t, jsonlPath, content)
}
