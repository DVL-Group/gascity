package main

import (
	"io"
	"path/filepath"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/git"
)

// Seam A — lossless `gc rig add --adopt` (dac-y7mg.1 / spike 002 §4).
//
// gc suppresses bd's issues.jsonl auto-export (BD_EXPORT_AUTO=false) and reaps
// any on-disk export so a stale copy cannot trigger bd's re-import-on-write
// stall (sa-41j3kp). Two deleters implement that reaping:
//
//   - removeStaleBdExportJSONL (beads_provider_lifecycle.go) — on canonical
//     config sync during init/adopt, and
//   - reapStaleBdExportJSONL (bd_env.go) — on every managed bead-store open.
//
// Both shared the false premise "issues.jsonl is always a regenerable export of
// a populated Dolt DB." That is wrong for a freshly-cloned Beads v1.1 repo,
// whose git-tracked issues.jsonl is the ONLY copy of its issues until the
// managed Dolt has been hydrated from it. Reaping that file before hydration
// silently destroys the data — the dac-75f3 repro turned 89 issues into 0.
//
// jsonlDeletionAllowed is the single gate both deleters consult before removing
// a scope's issues.jsonl.

// jsonlRelPath is the scope-relative path of the bd JSONL export, in git
// pathspec form (forward slashes) as required by `git ls-files`.
const jsonlRelPath = ".beads/issues.jsonl"

// jsonlExportPath returns the absolute path of a scope's bd JSONL export.
func jsonlExportPath(scopeRoot string) string {
	return filepath.Join(scopeRoot, ".beads", "issues.jsonl")
}

// jsonlDeletionAllowed reports whether scopeRoot/.beads/issues.jsonl may be
// safely removed. Deletion is permitted ONLY when BOTH hold:
//
//   - the file is NOT git ls-files-tracked — a tracked JSONL is a committed
//     source-of-truth mirror that must survive registration byte-for-byte; and
//   - the scope's managed Dolt store contains at least one issue row — so the
//     JSONL is a redundant export, not the last durable copy.
//
// It fails closed: any inability to prove row-count > 0 (store unopenable, list
// error, database absent) preserves the file. The cheap git-tracked check runs
// first, so the common tracked-mirror case never pays for a store open.
func jsonlDeletionAllowed(scopeRoot, cityPath string) bool {
	if scopeRoot == "" {
		return false
	}
	if jsonlPathIsGitTracked(scopeRoot) {
		return false
	}
	hasRows, ok := scopeManagedStoreHasRows(scopeRoot, cityPath)
	return ok && hasRows
}

// jsonlIsGitTrackedHook / scopeHasManagedRowsHook are nil-by-default test seams.
// They are plain vars with NO initializer, so they add no package-init
// dependency: the real probe below reaches jsonlDeletionAllowed through the
// bead-store-open chain, so binding the real closures to initialized vars would
// form an initialization cycle. Tests assign these to drive either branch of
// the gate without a live git repo or Dolt server.
var (
	jsonlIsGitTrackedHook   func(scopeRoot string) bool
	scopeHasManagedRowsHook func(scopeRoot, cityPath string) (hasRows, ok bool)
)

// jsonlPathIsGitTracked reports whether scopeRoot/.beads/issues.jsonl is tracked
// in git. A non-repo, untracked, ignored, or absent path is false.
func jsonlPathIsGitTracked(scopeRoot string) bool {
	if jsonlIsGitTrackedHook != nil {
		return jsonlIsGitTrackedHook(scopeRoot)
	}
	return git.New(scopeRoot).IsTracked(jsonlRelPath)
}

// scopeManagedStoreHasRows reports (hasRows, ok) for the scope's managed bead
// store. ok=false means the row-count could not be determined and the caller
// must treat the store as possibly empty (fail closed — preserve the JSONL).
//
// It constructs the store WITHOUT the reap wrapper: bdStoreForCity /
// bdStoreForRig call the reapers, which call this function, so reusing them
// would recurse unboundedly. The rig-aware command runner resolves both city
// and rig scopes; a misresolved endpoint surfaces as a list error → ok=false →
// deletion denied, never data loss.
func scopeManagedStoreHasRows(scopeRoot, cityPath string) (hasRows, ok bool) {
	if scopeHasManagedRowsHook != nil {
		return scopeHasManagedRowsHook(scopeRoot, cityPath)
	}
	cfg, err := loadCityConfig(cityPath, io.Discard)
	if err != nil {
		cfg = nil
	}
	store := beads.NewBdStoreWithPrefix(
		scopeRoot,
		bdCommandRunnerForRig(cityPath, cfg, scopeRoot),
		issuePrefixForScope(scopeRoot, cityPath, cfg),
		bdStoreOptionsForConfig(cfg)...,
	)
	list, err := store.List(beads.ListQuery{AllowScan: true, Limit: 1})
	if err != nil {
		return false, false
	}
	return len(list) > 0, true
}
