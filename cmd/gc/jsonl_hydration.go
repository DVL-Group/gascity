package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/fsys"
)

// Seam A completion — eager hydration of a surviving issues.jsonl (dac-y7mg.4 /
// spike 002 §4).
//
// Seam A (dac-y7mg.1) stopped gc from DELETING a repo's git-tracked
// .beads/issues.jsonl before it could be loaded, and reordered initAndHookDir so
// `bd init` runs before the reaping config-sync. That fix rested on one premise:
// "bd init auto-imports a surviving issues.jsonl into the empty managed
// database." Under bd 1.0.5 it did. Under bd 1.1.0 it does NOT, and nothing else
// in the funnel picks up the slack:
//
//   - `bd init` no longer auto-imports (verified: 0 rows after init beside an
//     89-row JSONL);
//   - the gc-beads-bd provider script's op_init contains no import step;
//   - `gc beads materialize` reports OK against a 0-row store;
//   - bd's write-time auto-import fires in EMBEDDED mode only — every gc-managed
//     store is server mode, so it never fires there.
//
// Net effect before this seam: adopting a Beads v1.1 repo "succeeded" with the
// JSONL preserved but the managed store EMPTY — the issues invisible to `gc bd`
// until someone ran `bd import` by hand. Seam A made the wipe non-destructive;
// it did not make registration lossless.
//
// hydrateScopeFromSurvivingJSONL closes that gap at the one funnel every
// initialization path already shares (initAndHookDir, therefore `gc rig add
// --adopt`, `gc beads materialize`, `gc init`, and controller boot): when a
// scope still has an issues.jsonl AND its store is provably row-count-0, run the
// same pinned bd the store itself uses to import the file.
//
// Invariants, in the order the code enforces them:
//
//   - Never import into a scope gc has not initialized. No canonical
//     .beads/metadata.json means no pinned database for this scope, and bd would
//     resolve a default one — the row-count probe would then report a FOREIGN
//     database as empty and the import would land in it.
//   - Never import into a populated store. Row-count > 0 means the store — not
//     the file — is the live copy; bd import is an upsert and would silently
//     rewrite live rows from a stale mirror.
//   - Never import blind. An unprovable row-count (store unopenable, database
//     absent) is NOT treated as empty: gc warns and skips rather than risk the
//     upsert above. The scope keeps its JSONL (jsonlDeletionAllowed fails closed
//     on the same signal), so a later re-run still hydrates.
//   - Fail closed on import error. The error aborts initAndHookDir before the
//     reaping config-sync ever runs, with the JSONL untouched and no
//     partial-success claim. The store stays row-count-0, so the retry re-enters
//     this path unchanged.
//   - Idempotent. A second pass sees rows > 0 and does nothing.
//   - Cheap. One os.Stat guards everything; scopes with no surviving export
//     (the steady state, since gc reaps regenerable ones) never open a store.
//
// Residual risk 1, stated plainly: this step inherits gc's existing per-scope bd
// resolution — it adds none of its own. If that resolution silently degrades (bd
// reached without server coordinates falls back to an embedded on-disk store),
// the import lands in the fallback store rather than the managed one, exactly as
// any other gc-issued bd write would. The metadata precondition above removes
// the one case observed in this lane (an uninitialized scope resolving the
// server's default database); pinning resolution fail-closed for every call site
// is Seam B (dac-y7mg.2), deliberately not built here.
//
// Residual risk 2, stated plainly: bd import is not transactional. An import that
// fails PART way leaves rows > 0 with the JSONL still on disk. The funcs's own
// retry is then a no-op (rows exist), and if that JSONL is untracked the Seam A
// reapers are free to delete it on a later pass — the unimported remainder would
// be lost. It is bounded: a git-tracked JSONL (the adopt case this seam exists
// for, and what Beads v1.1 repos ship) is never deletable at all, and the abort
// surfaces the failure to the operator with the file still in place. Making the
// window zero needs transactional import in bd itself, not here.

var (
	// hydrationBdImport runs the pinned `bd import` for a scope. It is a var so
	// unit tests can drive the funnel — including the fail-closed path — without
	// a real bd binary or Dolt server. Override only in tests that do not call
	// t.Parallel while the hook is changed.
	hydrationBdImport = execBdImportForScope
	// hydrationDeferredInit reports whether store init was deferred to a later
	// pass (GC_DOLT=skip). Indirected so tests can drive the deferred branch
	// without mutating the process environment — cmd/gc's untagged environment
	// census is a ratchet that no new call site may grow.
	hydrationDeferredInit = gcDoltSkip
	// hydrationLogWriter receives the one-line hydration summary and the
	// skipped-a-surviving-export warnings. Tests capture it.
	hydrationLogWriter io.Writer = os.Stderr
)

// hydrateScopeFromSurvivingJSONL imports scopeRoot/.beads/issues.jsonl into the
// scope's managed bead store when — and only when — the file survives on disk,
// the scope is an initialized bd-contract store, and that store is provably
// empty. Returns an error only when an attempted import fails; every "not
// applicable" condition is a nil no-op.
func hydrateScopeFromSurvivingJSONL(cityPath, scopeRoot string) error {
	scopeRoot = strings.TrimSpace(scopeRoot)
	if scopeRoot == "" {
		return nil
	}
	jsonlPath := jsonlExportPath(scopeRoot)
	info, err := os.Stat(jsonlPath)
	if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
		// No surviving export (or an empty/irregular one): nothing to hydrate.
		// This is the steady state for every canonical scope, so it must stay a
		// single stat — no config load, no store open.
		return nil
	}
	if hydrationDeferredInit() {
		// Deferred init (GC_DOLT=skip): initBeadsForDir only seeded canonical
		// scope files, there is no store to import into yet. The real init at
		// controller boot re-enters this funnel and hydrates then.
		return nil
	}
	if !cityUsesBdStoreContract(cityPath) {
		// file/legacy providers do not have a bd-addressable managed store.
		return nil
	}
	if !canonicalScopeStoreInitialized(scopeRoot) {
		// The scope carries no canonical metadata.json, so `bd init` never
		// created a store here (an exec provider that was never materialized
		// leaves initBeadsForDir a no-op). bd would then resolve the server's
		// DEFAULT database instead of this scope's — the row-count probe reports
		// that foreign database as "empty" and an import would land in it.
		// Verified: probing a scope with no metadata.json makes bd warn
		// "no beads configuration found … using default database name beads".
		fmt.Fprintf(hydrationLogWriter, //nolint:errcheck // best-effort stderr
			"beads: %s: no canonical .beads/metadata.json (store not initialized); skipped importing surviving %s\n",
			scopeRoot, jsonlRelPath)
		return nil
	}
	hasRows, ok := scopeManagedStoreHasRows(scopeRoot, cityPath)
	if !ok {
		fmt.Fprintf(hydrationLogWriter, //nolint:errcheck // best-effort stderr
			"beads: %s: managed store row-count unprovable; skipped importing surviving %s (re-run once the store is reachable)\n",
			scopeRoot, jsonlRelPath)
		return nil
	}
	if hasRows {
		// The store is the live copy; the JSONL is a mirror. Importing here
		// would upsert stale rows over live ones.
		return nil
	}
	out, importErr := hydrationBdImport(cityPath, scopeRoot, jsonlPath)
	if importErr != nil {
		return fmt.Errorf("hydrating bead scope %s from %s: %w", scopeRoot, jsonlRelPath, importErr)
	}
	if summary := bdImportSummaryLine(out); summary != "" {
		fmt.Fprintf(hydrationLogWriter, "beads: %s: %s\n", scopeRoot, summary) //nolint:errcheck // best-effort stderr
	}
	return nil
}

// canonicalScopeStoreInitialized reports whether scopeRoot carries the canonical
// .beads/metadata.json that gc writes when it initializes a scope's store, and
// that the file names a database. That file is what pins WHICH database bd
// addresses for this scope; without it bd falls back to a default, so it is the
// precondition for any write gc makes on the scope's behalf. Unreadable or
// unparseable metadata is treated as uninitialized (fail closed).
func canonicalScopeStoreInitialized(scopeRoot string) bool {
	state, ok, err := contract.LoadMetadataState(fsys.OSFS{}, scopeMetadataJSONPath(scopeRoot))
	if err != nil || !ok {
		return false
	}
	return strings.TrimSpace(state.Database) != "" || strings.TrimSpace(state.DoltDatabase) != ""
}

// execBdImportForScope invokes `bd import <jsonlPath>` through the SAME
// rig-aware command runner the scope's store uses (bdCommandRunnerForRig, also
// used by the Seam A row-count probe). That inherits the resolved server
// coordinates, BEADS_DIR pinning, export/auto-backup suppression, and the
// managed transport retry — and adds no new bd resolution policy: pinning the bd
// CLI per scope is Seam B (dac-y7mg.2), deliberately not built here.
func execBdImportForScope(cityPath, scopeRoot, jsonlPath string) ([]byte, error) {
	cfg, err := loadCityConfig(cityPath, io.Discard)
	if err != nil {
		cfg = nil
	}
	return bdCommandRunnerForRig(cityPath, cfg, scopeRoot)(scopeRoot, "bd", "import", jsonlPath)
}

// bdImportSummaryLine extracts bd import's human summary ("Imported N issues
// from …") for the operator-facing log line. Empty when bd said nothing.
func bdImportSummaryLine(out []byte) string {
	for _, line := range bytes.Split(out, []byte("\n")) {
		if trimmed := strings.TrimSpace(string(line)); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
