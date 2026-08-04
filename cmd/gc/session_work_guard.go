package main

import (
	"fmt"
	"io"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// closeSessionBeadIfUnassigned closes a session bead only when the live store
// confirms no open or in-progress work is assigned to it across the primary
// store AND any attached rig stores. Use this cross-store guard for cleanup
// paths that must not orphan work in any attached store. Reconciler paths that
// close a session according to its configured agent reachability should use
// closeSessionBeadIfReachableStoreUnassigned instead.
//
// Callers must NOT pass a pre-computed work snapshot — this helper queries the
// stores itself so its decision cannot be poisoned by a stale snapshot taken
// earlier in the tick (see the PR that retired the snapshot-based variant).
// Live-query failures fail closed: the bead stays open until assignment can be
// re-verified.
func closeSessionBeadIfUnassigned(
	store beads.Store,
	rigStores map[string]beads.Store,
	cfg *config.City,
	session beads.Bead,
	reason string,
	now time.Time,
	stderr io.Writer,
) bool {
	if stderr == nil {
		stderr = io.Discard
	}
	hasAssignedWork, err := sessionHasOpenAssignedWorkForConfig(store, rigStores, session, cfg)
	if err != nil {
		fmt.Fprintf(stderr, "session work guard: checking assigned work for %s: %v\n", session.ID, err) //nolint:errcheck
		return false
	}
	if hasAssignedWork {
		return false
	}
	if isFailedCreateSessionBead(session) {
		return closeFailedCreateBead(sessionFrontDoor(store), session.ID, now, stderr)
	}
	return closeBead(store, session.ID, reason, now, stderr)
}

// closeSessionInfoIfUnassigned is the session.Info form of
// closeSessionBeadIfUnassigned: it closes the session identified by info only when
// the live cross-store query confirms no open or in-progress work is assigned to
// it. The identity/close reads route through the typed projection and the session
// front door (closeBead / closeFailedCreateBead, which funnel writes through
// sessionFrontDoor and run the extmsg/orphaned-work release cascade). Byte-
// identical to the raw form for the GCSweep close op.
func closeSessionInfoIfUnassigned(
	store beads.Store,
	rigStores map[string]beads.Store,
	cfg *config.City,
	info sessionpkg.Info,
	reason string,
	now time.Time,
	stderr io.Writer,
) bool {
	if stderr == nil {
		stderr = io.Discard
	}
	hasAssignedWork, err := sessionHasOpenAssignedWorkForConfigInfo(store, rigStores, info, cfg)
	if err != nil {
		fmt.Fprintf(stderr, "session work guard: checking assigned work for %s: %v\n", info.ID, err) //nolint:errcheck
		return false
	}
	if hasAssignedWork {
		return false
	}
	if isFailedCreateSessionInfo(info) {
		return closeFailedCreateBead(sessionFrontDoor(store), info.ID, now, stderr)
	}
	return closeBead(store, info.ID, reason, now, stderr)
}

// closeSessionBeadIfReachableStoreUnassigned closes a session bead only when
// the live store scope its configured agent can query has no STARTABLE work
// assigned to the session — in-progress work (already claimed) or ready open
// work. It returns whether the close succeeded, matching
// closeSessionBeadIfUnassigned's contract.
//
// "Startable", not "open", is the correct gate here, and the distinction is
// load-bearing. A blocked, deferred, or ready-excluded open bead assigned to a
// session is work that session provably cannot claim: the wake gate
// (sessionHasAwakeAssignedWorkForReachableStore, used at the recycle/wake sites)
// already declines to wake for it. Gating the close on mere openness made the two
// gates disagree about the same bead — too blocked to wake for, yet too assigned
// to release — which strands the session `state: awake` holding work it may never
// start. Where sleep_after_idle is off there is no timeout to break the tie, so
// the agent relaunches on a ~20s cycle until the model provider cuts it off. That
// is not hypothetical: it consumed two full 5-hour quota windows (658 sessions,
// zero work performed) before the gates were reconciled here.
//
// Releasing the session does not orphan the blocked bead. closeBead runs
// releaseWorkFromClosedSessionBead, which clears the assignee on every non-closed
// assigned bead, resets in_progress work to open, and re-routes via the closing
// session's fallback route so the work stays discoverable — the same end state an
// operator reaches by hand. In-progress and ready open work still hold the session
// open, so the orphan protection this guard exists for is fully preserved.
//
// The session parameter is a session.Info: the reachable-store gate reads the
// session through the typed front door, while the close routes through closeBead
// (which already funnels its writes through sessionFrontDoor AND runs the
// extmsg/orphaned-work release cascade Store.Close does not — so the close stays
// on closeBead, not Store.Close, to preserve that behavior).
func closeSessionBeadIfReachableStoreUnassigned(
	cityPath string,
	cfg *config.City,
	store beads.Store,
	rigStores map[string]beads.Store,
	info sessionpkg.Info,
	reason string,
	now time.Time,
	stderr io.Writer,
) bool {
	if stderr == nil {
		stderr = io.Discard
	}
	hasAssignedWork, err := sessionHasAwakeAssignedWorkForReachableStore(cityPath, cfg, store, rigStores, info)
	if err != nil {
		// Fail closed: an unverifiable assignment keeps the session bead open.
		fmt.Fprintf(stderr, "session work guard: checking reachable startable work for %s: %v\n", info.ID, err) //nolint:errcheck
		return false
	}
	if hasAssignedWork {
		return false
	}
	if isFailedCreateSessionInfo(info) {
		return closeFailedCreateBead(sessionFrontDoor(store), info.ID, now, stderr)
	}
	return closeBead(store, info.ID, reason, now, stderr)
}
