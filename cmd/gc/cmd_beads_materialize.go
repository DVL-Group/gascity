package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/spf13/cobra"
)

// gc beads materialize — lab-safe, supervisor-free completion of the deferred
// managed bead-store initialization (the Seam A hydrate-before-normalize
// funnel) for the city scope and/or named adopted rigs.
//
// Motivation (dac-y7mg.3): fork-main gc defers ALL managed-store creation —
// even the city hq — to supervisor boot, and `gc start` is not lab-safe
// (machine-wide supervisor registration, provider readiness, no agentless
// mode). The spike-002 exact-N rehearsal needs to hydrate an adopted rig's
// git-tracked issues.jsonl into managed Dolt between `gc rig add --adopt` and a
// cold-restart export, WITHOUT booting a supervisor. This command runs exactly
// the store half of startBeadsLifecycle and nothing else:
//
//	ensure the city's managed Dolt server is running (reuse a healthy one, else
//	  start it via the managed-start path in dolt_start_managed.go — never a
//	  bare dolt) → initAndHookDir per selected scope (the same hydrate→normalize
//	  funnel the supervisor runs, with all Seam A invariants intact).
//
// It makes ZERO supervisor / session / AI-provider calls: no
// registerCityWithSupervisor, no api.ProbeProviders provider-readiness probe,
// no session lifecycle. It is safe to run under `env -i` with a lab
// HOME/GC_HOME. A git-tracked or not-yet-hydrated issues.jsonl is preserved
// (Seam A), and re-running is idempotent.

type beadsMaterializeOptions struct {
	rigs []string
	all  bool
}

// materializeScope is one bead-store scope to hydrate: its directory, issue
// prefix, and a human label for progress/error reporting.
type materializeScope struct {
	dir    string
	prefix string
	label  string
}

// Command seams. Tests bind these to spies so the scope plan, ordering, and
// partial-failure handling can be asserted without a live Dolt. Production
// binds them to the real managed-Dolt ensure and the shared Seam A init
// funnel. Reassigning these outside tests is a bug.
var (
	beadsMaterializeEnsureDolt = ensureManagedDoltForMaterialize
	beadsMaterializeInitScope  = initAndHookDir
)

func newBeadsMaterializeCmd(stdout, stderr io.Writer) *cobra.Command {
	var opts beadsMaterializeOptions
	cmd := &cobra.Command{
		Use:   "materialize",
		Short: "Materialize deferred managed bead stores without booting a supervisor",
		Long: `Complete the deferred managed bead-store initialization for the city
scope and/or named adopted rigs, exactly as the supervisor would at boot, but
without registering a supervisor, checking AI-provider readiness, or starting
any agent session.

For each selected scope this runs the Seam A hydrate-before-normalize funnel
(the same initAndHookDir the supervisor runs), after ensuring the city's
managed Dolt server is running — reusing a healthy server, or starting it via
the managed-start path (never a bare dolt). A git-tracked or not-yet-hydrated
.beads/issues.jsonl is preserved, and re-running is idempotent.

Scope selection:
  (no flag)    materialize the city scope (hq)
  --rig NAME   materialize the named adopted rig (repeatable)
  --all        materialize the city scope and every gc-managed rig

Safe under env -i with a lab HOME/GC_HOME. Exits non-zero (naming the scopes
that already completed) on any partial materialization.`,
		Example: `  gc beads materialize
  gc beads materialize --rig elt-pipeline
  gc beads materialize --all`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if cmdBeadsMaterialize(opts, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().StringArrayVar(&opts.rigs, "rig", nil, "materialize this adopted rig by name (repeatable)")
	cmd.Flags().BoolVar(&opts.all, "all", false, "materialize the city scope and every gc-managed rig")
	return cmd
}

func cmdBeadsMaterialize(opts beadsMaterializeOptions, stdout, stderr io.Writer) int {
	const name = "gc beads materialize"

	if opts.all && len(opts.rigs) > 0 {
		fmt.Fprintf(stderr, "%s: --all and --rig are mutually exclusive\n", name) //nolint:errcheck // best-effort stderr
		return 1
	}

	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", name, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if !cityUsesBdStoreContract(cityPath) {
		fmt.Fprintf(stderr, "%s: only supported for bd-backed (managed Dolt) beads providers\n", name) //nolint:errcheck // best-effort stderr
		return 1
	}
	cfg, err := loadCityConfig(cityPath, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "%s: loading config: %v\n", name, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	resolveRigPaths(cityPath, cfg.Rigs)

	scopes, err := planMaterializeScopes(cityPath, cfg, opts)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", name, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if len(scopes) == 0 {
		fmt.Fprintf(stderr, "%s: no gc-managed Dolt scopes to materialize\n", name) //nolint:errcheck // best-effort stderr
		return 1
	}

	// "requires or starts": reuse a healthy managed Dolt server, else start one
	// via the managed-start path (dolt_start_managed.go) — never a bare dolt.
	// No supervisor/session/provider machinery is involved.
	if err := beadsMaterializeEnsureDolt(cityPath); err != nil {
		fmt.Fprintf(stderr, "%s: ensuring managed Dolt server: %v\n", name, err) //nolint:errcheck // best-effort stderr
		return 1
	}

	done := make([]string, 0, len(scopes))
	for _, sc := range scopes {
		if err := beadsMaterializeInitScope(cityPath, sc.dir, sc.prefix); err != nil {
			fmt.Fprintf(stderr, "%s: materializing %s: %v\n", name, sc.label, err) //nolint:errcheck // best-effort stderr
			// Partial materialization is a hard failure: earlier scopes may now
			// be hydrated while this one is not. Name what completed so the
			// operator knows the managed store is in a mixed state, and stop —
			// do not attempt the remaining scopes.
			if len(done) > 0 {
				fmt.Fprintf(stderr, "%s: PARTIAL — materialized %d of %d scope(s) before failure: %s; the managed store is incomplete\n",
					name, len(done), len(scopes), strings.Join(done, ", ")) //nolint:errcheck // best-effort stderr
			} else {
				fmt.Fprintf(stderr, "%s: no scopes materialized\n", name) //nolint:errcheck // best-effort stderr
			}
			return 1
		}
		done = append(done, sc.label)
		fmt.Fprintf(stdout, "materialized %s\n", sc.label) //nolint:errcheck // best-effort stdout
	}
	fmt.Fprintf(stdout, "%s: OK — materialized %d scope(s): %s\n", name, len(done), strings.Join(done, ", ")) //nolint:errcheck // best-effort stdout
	return 0
}

// planMaterializeScopes resolves the ordered set of scopes to hydrate. Named
// rigs (--rig) must exist and be gc-managed; an unknown or non-managed name is
// a hard error so a typo never silently materializes nothing. The default
// (no flag) is the city scope; --all is the city scope plus every gc-managed
// rig in config order. Non-managed (external/postgres) scopes are excluded
// from --all silently because they have nothing to materialize locally.
func planMaterializeScopes(cityPath string, cfg *config.City, opts beadsMaterializeOptions) ([]materializeScope, error) {
	if len(opts.rigs) > 0 {
		scopes := make([]materializeScope, 0, len(opts.rigs))
		for _, want := range opts.rigs {
			want = strings.TrimSpace(want)
			if want == "" {
				return nil, fmt.Errorf("empty --rig name")
			}
			rig := findRigByName(want, cfg.Rigs)
			if rig == nil {
				return nil, fmt.Errorf("no rig named %q in this city", want)
			}
			if strings.TrimSpace(rig.Path) == "" {
				return nil, fmt.Errorf("rig %q has no resolved path", want)
			}
			if !rigUsesManagedBdStoreContract(cityPath, *rig) {
				return nil, fmt.Errorf("rig %q does not use a gc-managed Dolt store; materialize only applies to gc-managed scopes", want)
			}
			scopes = append(scopes, materializeScope{
				dir:    rig.Path,
				prefix: rig.EffectivePrefix(),
				label:  fmt.Sprintf("rig %q", rig.Name),
			})
		}
		return scopes, nil
	}

	scopes := make([]materializeScope, 0, 1+len(cfg.Rigs))
	if scopeUsesManagedBdStoreContract(cityPath, cityPath) {
		scopes = append(scopes, materializeScope{
			dir:    cityPath,
			prefix: config.EffectiveHQPrefix(cfg),
			label:  "city (hq)",
		})
	}
	if opts.all {
		for i := range cfg.Rigs {
			rig := cfg.Rigs[i]
			if strings.TrimSpace(rig.Path) == "" || !rigUsesManagedBdStoreContract(cityPath, rig) {
				continue
			}
			scopes = append(scopes, materializeScope{
				dir:    rig.Path,
				prefix: rig.EffectivePrefix(),
				label:  fmt.Sprintf("rig %q", rig.Name),
			})
		}
	}
	return scopes, nil
}

// ensureManagedDoltForMaterialize brings the city's managed Dolt server up
// without any supervisor/session/AI-provider machinery. It first materializes
// the builtin runtime assets (the gc-beads-bd provider script) so the managed
// lifecycle is runnable ahead of a supervisor boot, then runs the provider's
// managed "start" op — the exact path startBeadsLifecycle uses at boot. That op
// creates and initializes the managed Dolt data dir on a cold (never-started)
// city, reuses a healthy already-running server on a warm one, and routes the
// actual spawn through the managed-start path in dolt_start_managed.go (never a
// bare dolt). It is a beads-store-provider call — the same class of call
// initAndHookDir makes to run `bd init` — not a session/AI-provider or
// supervisor call, so it stays safe under env -i with a lab HOME/GC_HOME.
func ensureManagedDoltForMaterialize(cityPath string) error {
	if err := EnsureBuiltinRuntimeAssets(cityPath, os.Stderr); err != nil {
		return fmt.Errorf("materialize managed provider assets: %w", err)
	}
	if err := ensureBeadsProvider(cityPath); err != nil {
		return fmt.Errorf("start managed Dolt server: %w", err)
	}
	return nil
}
