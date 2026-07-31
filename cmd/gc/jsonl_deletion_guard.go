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
// safely removed. Deletion is permitted ONLY when ALL of these hold:
//
//   - git PROVES the file is not tracked — a tracked JSONL is a committed
//     source-of-truth mirror that must survive registration byte-for-byte, and
//     an unprovable answer counts as tracked (see below);
//   - the scope carries the canonical .beads/metadata.json that pins WHICH
//     database bd addresses for it; and
//   - that pinned store already holds at least one ISSUE, open or closed — so
//     the JSONL is a redundant export, not the last durable copy. Wisps and
//     templates do not count; see managedStoreRowProbeQuery.
//
// Every check fails closed, in both halves:
//
//   - Row count: any inability to prove row-count > 0 (store unopenable, list
//     error, database absent) preserves the file.
//   - Git: an UNKNOWN answer (git absent, safe.directory refusal, corrupt
//     index, unreadable .git) is treated as tracked. Only a definitive negative
//     — pathspec matched nothing, or no repository above the scope at all —
//     clears this gate.
//   - Metadata: this is the SAME precondition hydrateScopeFromSurvivingJSONL
//     enforces, and for the same verified reason. A scope with no canonical
//     metadata.json has no pinned database, so bd resolves the SERVER'S DEFAULT
//     database instead. The row-count probe then reports that foreign
//     database's contents, and a non-empty one would authorize deleting this
//     scope's only copy of its issues on the strength of somebody else's rows.
//     An earlier revision of this comment claimed a misresolved endpoint
//     "surfaces as a list error → deletion denied, never data loss"; the
//     hydration seam disproved that — misresolution succeeds and answers about
//     the wrong database. Without this check the deletion gate was the weaker
//     of the two siblings.
//
// Order is cheapest-first: the git-tracked check, then a single metadata.json
// read, so the common tracked-mirror case never pays for a store open.
func jsonlDeletionAllowed(scopeRoot, cityPath string) bool {
	if scopeRoot == "" {
		return false
	}
	tracked, err := jsonlPathIsGitTracked(scopeRoot)
	if err != nil || tracked {
		return false
	}
	if !canonicalScopeStoreInitialized(scopeRoot) {
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
//
// Note for test authors: stubbing scopeHasManagedRowsHook makes the gate blind
// to the QUERY the real probe issues. That is exactly how the closed/ephemeral
// filtering defect below survived a full unit suite, so the query shape has its
// own hook-free coverage against a real BdStore runner.
var (
	jsonlIsGitTrackedHook   func(scopeRoot string) (tracked bool, err error)
	scopeHasManagedRowsHook func(scopeRoot, cityPath string) (hasRows, ok bool)
)

// jsonlPathIsGitTracked reports whether scopeRoot/.beads/issues.jsonl is tracked
// in git. A definitively untracked, ignored, or absent path — and a scope with
// no repository above it — is (false, nil). An error means git could not
// answer; callers must treat that as possibly tracked.
func jsonlPathIsGitTracked(scopeRoot string) (bool, error) {
	if jsonlIsGitTrackedHook != nil {
		return jsonlIsGitTrackedHook(scopeRoot)
	}
	return git.New(scopeRoot).IsTracked(jsonlRelPath)
}

// scopeManagedStoreHasRows reports (hasRows, ok) for the scope's managed bead
// store. The question it answers is "does this store already hold ISSUES" —
// open or closed, excluding wisps and templates; see managedStoreRowProbeQuery
// for why that is the predicate and not a bare row count. ok=false means it
// could not be determined and the caller must treat the store as possibly
// empty (fail closed — preserve the JSONL, skip the import).
//
// It constructs the store WITHOUT the reap wrapper: bdStoreForCity /
// bdStoreForRig call the reapers, which call this function, so reusing them
// would recurse unboundedly. Callers are responsible for establishing that the
// scope has a canonical metadata.json first; a scope without one resolves to
// the server's DEFAULT database and this probe then reports on that foreign
// database rather than failing (both call sites enforce that precondition).
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
	list, err := store.List(managedStoreRowProbeQuery())
	if err != nil {
		return false, false
	}
	return len(list) > 0, true
}

// managedStoreRowProbeQuery is the ListQuery both Seam A gates use to ask the
// one question that decides whether a scope's issues.jsonl is redundant:
//
//	does this store already hold the ISSUES the JSONL would supply?
//
// IncludeClosed is load-bearing. The zero ListQuery is a WORKING query, not a
// census: BdStore.listViaBDList appends `--all` only when IncludeClosed is set
// or Status=="closed", and ListQuery's client-side Matches drops closed rows
// again. A store holding only closed issues — a finished rig — therefore probed
// as row-count 0, which made both gates draw the exact inverse of the truth:
//
//   - hydrateScopeFromSurvivingJSONL would `bd import` a stale mirror over a
//     store full of live closed rows, violating its own "never import into a
//     populated store" invariant, on every controller boot, every
//     `gc rig add --adopt`, and every `gc beads materialize` — exiting 0 and
//     reporting "materialized … OK" while doing it.
//   - jsonlDeletionAllowed would deny deleting a genuinely redundant export.
//
// Limit 1 is retained: existence is the whole question.
//
// TierMode stays TierIssues (the zero value), DELIBERATELY. Widening to
// TierBoth would count rows that are not issues and cannot stand in for the
// export's contents:
//
//   - Ephemeral wisps. These are controller-owned session slots, not issues.
//     Counting them would let an untracked issues.jsonl beside a wisps-only
//     store read as "redundant" and be deleted.
//   - Templates. bdListShouldIncludeTemplates adds --include-templates for
//     every non-message TierBoth query, and template rows are real rows written
//     by `bd cook` when formulas/molecules are installed. (`bd init` seeds none
//     and no migration inserts any, so a fresh adopt is unaffected — but any
//     scope that has cooked a formula is.) A store whose only rows are cooked
//     templates would read as populated: hydration would skip forever and
//     deletion would be authorized.
//
// "At least one row of any kind" is simply not the same predicate as "the
// issues export is redundant", and this gate sits on a data-loss boundary.
// Restricting to the issues tier costs nothing against the defect above: a
// scope whose issues really are in the store still answers true, via
// IncludeClosed.
func managedStoreRowProbeQuery() beads.ListQuery {
	return beads.ListQuery{
		AllowScan:     true,
		Limit:         1,
		IncludeClosed: true,
		TierMode:      beads.TierIssues,
	}
}
