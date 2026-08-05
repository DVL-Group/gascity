package processenv

import (
	"os"
	"path/filepath"
	"strings"
)

// PrependGCBinDirToPATH ensures that the directory containing the gc binary
// is the first entry in env["PATH"]. If env["PATH"] is unset, falls back to
// the calling process's PATH as the base.
//
// This protects spawned agents (which may write `gc` in shell prompts) from
// PATH collisions with unrelated binaries — notably Homebrew's `graphviz`
// package, which ships /opt/homebrew/bin/gc and breaks bare `gc` invocations
// for any agent whose PATH happens to put /opt/homebrew/bin first.
//
// gcBin is the absolute path to the gc binary (typically the value the caller
// also writes to env["GC_BIN"]). If empty or has no directory component, the
// function is a no-op.
//
// Both the CLI launch path (cmd/gc/template_resolve.go) and the API session-env
// builder (internal/api cityAnchoredSessionEnv) call this so the GC_BIN/PATH
// pair can never drift apart between the two session-launch surfaces.
func PrependGCBinDirToPATH(env map[string]string, gcBin string) {
	prependBinDirToPATH(env, gcBin)
}

// PrependBdBinDirToPATH ensures that the directory containing the scope's
// resolved bd binary is the first entry in env["PATH"].
//
// This is the agent-PATH half of Seam B (internal/beads/bdresolve.go). gc's own
// bd invocations are gated by the per-scope resolver, but an agent that types a
// bare `bd` in its shell is not — it gets whatever its PATH composition finds,
// which on a multi-bd host is exactly the ambiguity the seam exists to remove.
// Projecting the resolved binary's directory to the front makes the agent's
// bare `bd` the same binary gc verified for that scope.
//
// bdBin MUST be a path the resolver actually blessed. Callers must pass "" when
// the resolver refused: this is a bias, not a gate — it cannot stop an agent
// from invoking some other bd — so pointing it at an unverified binary would
// lend gc's endorsement to precisely the mismatch the pin forbids.
func PrependBdBinDirToPATH(env map[string]string, bdBin string) {
	prependBinDirToPATH(env, bdBin)
}

func prependBinDirToPATH(env map[string]string, bin string) {
	if bin == "" {
		return
	}
	dir := filepath.Dir(bin)
	if dir == "" || dir == "." {
		return
	}
	sep := string(os.PathListSeparator)
	base, ok := env["PATH"]
	if !ok {
		base = os.Getenv("PATH")
	}
	if base == "" {
		env["PATH"] = dir
		return
	}

	parts := strings.Split(base, sep)
	entries := []string{dir}
	for _, p := range parts {
		if p == dir {
			continue
		}
		entries = append(entries, p)
	}
	env["PATH"] = strings.Join(entries, sep)
}
