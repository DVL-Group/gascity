package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

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
// git-tracked → tracked, managed row-count → (hasRows, ok). Restored on cleanup.
func stubJSONLDeletionSeams(t *testing.T, tracked, hasRows, ok bool) {
	t.Helper()
	jsonlIsGitTrackedHook = func(string) bool { return tracked }
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

// writeManagedScope materializes a managed-origin scope with a stale JSONL
// export and returns (scopeRoot, jsonlPath, content).
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
	return scope, jsonlPath, content
}

// gitTrack makes relPath genuinely git ls-files-tracked inside repoDir using a
// real git process (no stub), so the git-tracked gate is exercised for real. It
// drives git through internal/git rather than a bare exec.Command so it adds no
// new subprocess call site to the repository resource census.
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
	if !g.IsTracked(relPath) {
		t.Fatalf("git.IsTracked(%q) = false after add; test precondition broken", relPath)
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
			stubJSONLDeletionSeams(t, c.tracked, c.hasRows, c.okDB)
			if got := jsonlDeletionAllowed("/scope", "/city"); got != c.want {
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
	stubInitAndHookDirSteps(t,
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
	stubInitAndHookDirSteps(t,
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
