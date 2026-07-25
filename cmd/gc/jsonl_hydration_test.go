package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Seam A completion — adversarial tests (dac-y7mg.4). They attack the eager
// import: it must fire ONLY into a provably empty store, never into a populated
// or unprovable one, must fail closed without touching the JSONL or reaching the
// reaping config-sync, and must sit strictly between `bd init` and normalize in
// the shared funnel. Every "must not" here is a zero-call assertion on the
// import seam, not an end-state guess.

type hydrationImportCall struct {
	cityPath  string
	scopeRoot string
	jsonlPath string
}

// stubHydrationImportWith replaces the pinned `bd import` with fn and records
// every call, so a test can prove both the arguments and the call COUNT.
func stubHydrationImportWith(t *testing.T, fn func(call hydrationImportCall) ([]byte, error)) *[]hydrationImportCall {
	t.Helper()
	calls := &[]hydrationImportCall{}
	orig := hydrationBdImport
	hydrationBdImport = func(cityPath, scopeRoot, jsonlPath string) ([]byte, error) {
		call := hydrationImportCall{cityPath: cityPath, scopeRoot: scopeRoot, jsonlPath: jsonlPath}
		*calls = append(*calls, call)
		return fn(call)
	}
	t.Cleanup(func() { hydrationBdImport = orig })
	return calls
}

func stubHydrationImport(t *testing.T, out []byte, err error) *[]hydrationImportCall {
	t.Helper()
	return stubHydrationImportWith(t, func(hydrationImportCall) ([]byte, error) { return out, err })
}

func captureHydrationLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	orig := hydrationLogWriter
	hydrationLogWriter = buf
	t.Cleanup(func() { hydrationLogWriter = orig })
	return buf
}

// hydrationScope returns an INITIALIZED bd-contract city scope carrying a
// surviving issues.jsonl, plus the file's path and exact bytes. metadata.json is
// present because hydration refuses to write to a scope gc never initialized.
//
// Deliberately NO t.Setenv: internal/testenv scrubs GC_BEADS and GC_DOLT at
// test-binary init, so the bd-store-contract gate resolves to its "bd" default
// and the deferred-init gate to false without touching the process environment
// (stub hydrationDeferredInit for the deferred branch). cmd/gc's untagged
// environment census is a ratchet — no new Setenv call site may be added here.
func hydrationScope(t *testing.T) (scopeRoot, jsonlPath string, content []byte) {
	t.Helper()
	scopeRoot = t.TempDir()
	beadsDir := filepath.Join(scopeRoot, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"),
		[]byte(`{"database":"elt","backend":"dolt","dolt_database":"elt"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	jsonlPath = filepath.Join(beadsDir, "issues.jsonl")
	content = []byte(`{"_type":"issue","id":"elt-1","title":"hydrate me"}` + "\n" +
		`{"_type":"issue","id":"elt-2","title":"and me"}` + "\n")
	if err := os.WriteFile(jsonlPath, content, 0o644); err != nil {
		t.Fatal(err)
	}
	return scopeRoot, jsonlPath, content
}

// The load-bearing decision table. bd import is an upsert, so importing into a
// store that already has rows would rewrite live data from a possibly-stale
// mirror; an unprovable row-count must be treated as "possibly populated", never
// as empty.
func TestHydrateSurvivingJSONLImportsOnlyIntoProvablyEmptyStore(t *testing.T) {
	cases := []struct {
		name         string
		hasRows, ok  bool
		wantImports  int
		wantWarnLine bool
	}{
		{"row-count 0 → import", false, true, 1, false},
		{"rows present → never import (no upsert over live rows)", true, true, 0, false},
		{"row-count unprovable → never import blind, warn", false, false, 0, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			scope, jsonlPath, content := hydrationScope(t)
			stubManagedRows(t, c.hasRows, c.ok)
			calls := stubHydrationImport(t, []byte("Imported 2 issues from issues.jsonl\n"), nil)
			logs := captureHydrationLog(t)

			if err := hydrateScopeFromSurvivingJSONL(scope, scope); err != nil {
				t.Fatalf("hydrateScopeFromSurvivingJSONL: %v", err)
			}

			if len(*calls) != c.wantImports {
				t.Fatalf("bd import called %d times, want %d", len(*calls), c.wantImports)
			}
			if c.wantImports > 0 && (*calls)[0].jsonlPath != jsonlPath {
				t.Fatalf("imported %q, want the scope's surviving export %q", (*calls)[0].jsonlPath, jsonlPath)
			}
			if got := strings.Contains(logs.String(), "unprovable"); got != c.wantWarnLine {
				t.Fatalf("unprovable-warning emitted = %v, want %v (log=%q)", got, c.wantWarnLine, logs.String())
			}
			requireFileEquals(t, jsonlPath, content) // hydration never mutates the mirror
		})
	}
}

// Cost + blast-radius invariant: a scope with no surviving export (the steady
// state for every canonical scope) must not even OPEN the store. A row-count
// probe forks bd, so a regression here would put a subprocess on every init of
// every scope.
func TestHydrateSurvivingJSONLNoExportNeverProbesOrImports(t *testing.T) {
	for _, c := range []struct {
		name  string
		write func(t *testing.T, jsonlPath string)
	}{
		{"absent", func(t *testing.T, jsonlPath string) {
			t.Helper()
			if err := os.Remove(jsonlPath); err != nil {
				t.Fatal(err)
			}
		}},
		{"empty file", func(t *testing.T, jsonlPath string) {
			t.Helper()
			if err := os.WriteFile(jsonlPath, nil, 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{"a directory, not a file", func(t *testing.T, jsonlPath string) {
			t.Helper()
			if err := os.Remove(jsonlPath); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(jsonlPath, 0o755); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			scope, jsonlPath, _ := hydrationScope(t)
			c.write(t, jsonlPath)

			probes := 0
			scopeHasManagedRowsHook = func(_, _ string) (bool, bool) { probes++; return false, true }
			t.Cleanup(func() { scopeHasManagedRowsHook = nil })
			calls := stubHydrationImport(t, nil, nil)

			if err := hydrateScopeFromSurvivingJSONL(scope, scope); err != nil {
				t.Fatalf("hydrateScopeFromSurvivingJSONL: %v", err)
			}
			if probes != 0 {
				t.Fatalf("row-count probe ran %d times with no surviving export; want 0 (must cost one stat)", probes)
			}
			if len(*calls) != 0 {
				t.Fatalf("bd import called %d times with no surviving export; want 0", len(*calls))
			}
		})
	}
}

// A scope with no canonical metadata.json was never initialized by gc, so bd
// would resolve the server's DEFAULT database: the row-count probe would report
// a foreign database as empty and the import would land in it. Refuse — and do
// not even fork the probe.
func TestHydrateSurvivingJSONLRefusesUninitializedScope(t *testing.T) {
	for _, c := range []struct {
		name     string
		metadata string
		remove   bool
	}{
		{name: "absent metadata.json", remove: true},
		{name: "metadata.json names no database", metadata: `{"backend":"dolt"}`},
		{name: "unparseable metadata.json", metadata: `{not json`},
	} {
		t.Run(c.name, func(t *testing.T) {
			scope, _, _ := hydrationScope(t)
			metaPath := filepath.Join(scope, ".beads", "metadata.json")
			if c.remove {
				if err := os.Remove(metaPath); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(metaPath, []byte(c.metadata), 0o644); err != nil {
				t.Fatal(err)
			}

			probes := 0
			scopeHasManagedRowsHook = func(_, _ string) (bool, bool) { probes++; return false, true }
			t.Cleanup(func() { scopeHasManagedRowsHook = nil })
			calls := stubHydrationImport(t, nil, nil)
			logs := captureHydrationLog(t)

			if err := hydrateScopeFromSurvivingJSONL(scope, scope); err != nil {
				t.Fatalf("hydrateScopeFromSurvivingJSONL: %v", err)
			}
			if probes != 0 || len(*calls) != 0 {
				t.Fatalf("uninitialized scope probed %d times and imported %d times; want 0 and 0", probes, len(*calls))
			}
			if !strings.Contains(logs.String(), "not initialized") {
				t.Fatalf("log = %q, want it to name the uninitialized store", logs.String())
			}
		})
	}
}

// Deferred init (GC_DOLT=skip) only seeds canonical scope files — there is no
// store to import into, so the eager import must stay out of that path entirely.
func TestHydrateSurvivingJSONLSkipsDeferredInit(t *testing.T) {
	scope, _, _ := hydrationScope(t)
	origDeferred := hydrationDeferredInit
	hydrationDeferredInit = func() bool { return true }
	t.Cleanup(func() { hydrationDeferredInit = origDeferred })
	probes := 0
	scopeHasManagedRowsHook = func(_, _ string) (bool, bool) { probes++; return false, true }
	t.Cleanup(func() { scopeHasManagedRowsHook = nil })
	calls := stubHydrationImport(t, nil, nil)

	if err := hydrateScopeFromSurvivingJSONL(scope, scope); err != nil {
		t.Fatalf("hydrateScopeFromSurvivingJSONL: %v", err)
	}
	if probes != 0 || len(*calls) != 0 {
		t.Fatalf("deferred init probed %d times and imported %d times; want 0 and 0", probes, len(*calls))
	}
}

// Fail closed: a failing import surfaces as an error (never a partial-success
// claim), and the JSONL is byte-identical afterwards — the retry has the same
// inputs the first attempt had.
func TestHydrateSurvivingJSONLFailsClosedOnImportError(t *testing.T) {
	scope, jsonlPath, content := hydrationScope(t)
	stubManagedRows(t, false, true)
	wantErr := errors.New("bd import: exit status 1: failed to parse JSONL line")
	stubHydrationImport(t, []byte("partial output"), wantErr)
	captureHydrationLog(t)

	err := hydrateScopeFromSurvivingJSONL(scope, scope)

	if !errors.Is(err, wantErr) {
		t.Fatalf("hydrate err = %v, want it to wrap %v", err, wantErr)
	}
	requireFileEquals(t, jsonlPath, content)
}

// Idempotence: the second pass sees the rows the first pass imported and does
// nothing — re-adopt / re-materialize never re-imports and never deletes.
func TestHydrateSurvivingJSONLIdempotentAfterFirstImport(t *testing.T) {
	scope, jsonlPath, content := hydrationScope(t)
	imported := false
	scopeHasManagedRowsHook = func(_, _ string) (bool, bool) { return imported, true }
	t.Cleanup(func() { scopeHasManagedRowsHook = nil })
	calls := stubHydrationImportWith(t, func(hydrationImportCall) ([]byte, error) {
		imported = true // the store now holds the rows
		return []byte("Imported 2 issues from issues.jsonl\n"), nil
	})
	captureHydrationLog(t)

	for i := 0; i < 3; i++ {
		if err := hydrateScopeFromSurvivingJSONL(scope, scope); err != nil {
			t.Fatalf("pass %d: %v", i, err)
		}
		requireFileEquals(t, jsonlPath, content)
	}
	if len(*calls) != 1 {
		t.Fatalf("bd import ran %d times across 3 passes, want exactly 1", len(*calls))
	}
}

// The operator must see that hydration happened; a silent 89-row import into a
// store the user believes is empty is its own defect.
func TestHydrateSurvivingJSONLLogsImportSummary(t *testing.T) {
	scope, _, _ := hydrationScope(t)
	stubManagedRows(t, false, true)
	stubHydrationImport(t, []byte("\nImported 89 issues from issues.jsonl\n"), nil)
	logs := captureHydrationLog(t)

	if err := hydrateScopeFromSurvivingJSONL(scope, scope); err != nil {
		t.Fatalf("hydrateScopeFromSurvivingJSONL: %v", err)
	}
	if !strings.Contains(logs.String(), "Imported 89 issues") {
		t.Fatalf("hydration log = %q, want bd's import summary", logs.String())
	}
}

// stubInitAndHookDirHydrate swaps only the eager-import seam of initAndHookDir.
func stubInitAndHookDirHydrate(t *testing.T, fn func(cityPath, dir string) error) {
	t.Helper()
	orig := initAndHookDirHydrateJSONL
	initAndHookDirHydrateJSONL = fn
	t.Cleanup(func() { initAndHookDirHydrateJSONL = orig })
}

// Position in the funnel, asserted against the REAL hydrate function (only its
// probe/import seams are stubbed): the import must run after `bd init` created
// the database and strictly before the config-sync that may reap the JSONL.
func TestInitAndHookDirImportsBetweenInitAndNormalize(t *testing.T) {
	city, _, _ := hydrationScope(t)
	stubManagedRows(t, false /*hasRows*/, true /*ok*/)
	captureHydrationLog(t)

	var order []string
	stubHydrationImportWith(t, func(hydrationImportCall) ([]byte, error) {
		order = append(order, "import")
		return nil, nil
	})
	stubInitAndHookDirSteps(t,
		func(_, _, _, _ string) error { order = append(order, "init"); return nil },
		func(_, _, _, _ string) error { order = append(order, "normalize"); return nil },
	)

	if err := initAndHookDir(city, city, "gc"); err != nil {
		t.Fatalf("initAndHookDir: %v", err)
	}
	if want := []string{"init", "import", "normalize"}; !reflect.DeepEqual(order, want) {
		t.Fatalf("step order = %v, want %v (import strictly before normalize/reap)", order, want)
	}
}

// Fail-closed at the funnel level: an import failure aborts initAndHookDir with
// the reaping config-sync never reached — a zero-call invariant — and the JSONL
// still on disk for the retry.
func TestInitAndHookDirFailsClosedOnImportError(t *testing.T) {
	city, jsonlPath, content := hydrationScope(t)
	stubManagedRows(t, false, true)
	captureHydrationLog(t)
	wantErr := errors.New("bd import: connection refused")
	stubHydrationImport(t, nil, wantErr)

	normalizeCalls := 0
	stubInitAndHookDirSteps(t,
		func(_, _, _, _ string) error { return nil },
		func(_, _, _, _ string) error { normalizeCalls++; return nil },
	)

	err := initAndHookDir(city, city, "gc")

	if !errors.Is(err, wantErr) {
		t.Fatalf("initAndHookDir err = %v, want it to wrap %v", err, wantErr)
	}
	if normalizeCalls != 0 {
		t.Fatalf("normalize/reap ran %d times after a failed import; want 0 (fail-closed)", normalizeCalls)
	}
	requireFileEquals(t, jsonlPath, content)
}

// The eager import is wired into the ONE funnel every initialization path shares
// (adopt, materialize, init, controller boot), so no caller can be initialized
// without it. Proven by seam substitution rather than by re-running each caller.
func TestInitAndHookDirAlwaysRunsTheHydrationSeam(t *testing.T) {
	city, _, _ := hydrationScope(t)
	hydrateCalls := 0
	stubInitAndHookDirHydrate(t, func(_, dir string) error {
		hydrateCalls++
		if dir != city {
			t.Fatalf("hydrate called for %q, want the initialized scope %q", dir, city)
		}
		return nil
	})
	stubInitAndHookDirSteps(t,
		func(_, _, _, _ string) error { return nil },
		func(_, _, _, _ string) error { return nil },
	)

	if err := initAndHookDir(city, city, "gc"); err != nil {
		t.Fatalf("initAndHookDir: %v", err)
	}
	if hydrateCalls != 1 {
		t.Fatalf("hydration seam ran %d times, want exactly 1", hydrateCalls)
	}
}
