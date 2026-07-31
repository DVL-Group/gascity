//go:build !windows

package beads

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These tests attack the Seam B fail-closed contract with real subprocesses and
// real PATH composition. A fake `bd` records every invocation it receives, so
// the central assertion is not "an error was returned" but "bd was never run" —
// the zero-invocation invariant is the whole point of refusing BEFORE exec.

// fakeBd writes an executable named "bd" into its own directory and returns
// that directory (for PATH) and the invocation-log path.
//
// The script appends its argv to the log on EVERY call, then answers `version`
// and `context --json` from the version/schema it was built with. Any other
// subcommand exits 0 with no output — so if a test sees such a call in the log,
// the resolver let a real invocation through.
func fakeBd(t *testing.T, version string, schema int) (binDir, logPath string) {
	t.Helper()
	binDir = t.TempDir()
	logPath = filepath.Join(binDir, "invocations.log")
	script := fmt.Sprintf(`#!/bin/sh
echo "$@" >> %q
case "$1" in
  version) echo "bd version %s (deadbeef: HEAD@deadbeef)" ;;
  context) printf '{"backend":"dolt","bd_version":"%s","schema_version":%d}\n' ;;
esac
exit 0
`, logPath, version, version, schema)
	path := filepath.Join(binDir, "bd")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake bd: %v", err)
	}
	return binDir, logPath
}

// brokenBd writes a `bd` that logs its argv and then fails every probe, so the
// resolver can prove nothing about it.
func brokenBd(t *testing.T) (binDir, logPath string) {
	t.Helper()
	binDir = t.TempDir()
	logPath = filepath.Join(binDir, "invocations.log")
	script := fmt.Sprintf(`#!/bin/sh
echo "$@" >> %q
echo "bd exploded" >&2
exit 1
`, logPath)
	if err := os.WriteFile(filepath.Join(binDir, "bd"), []byte(script), 0o755); err != nil {
		t.Fatalf("write broken bd: %v", err)
	}
	return binDir, logPath
}

// scope creates a scope root whose .beads/identity.toml carries body.
// A body of "" omits the file entirely.
func scope(t *testing.T, body string) string {
	t.Helper()
	root := t.TempDir()
	if body == "" {
		return root
	}
	if err := os.MkdirAll(filepath.Join(root, ".beads"), 0o755); err != nil {
		t.Fatalf("mkdir .beads: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, ".beads", "identity.toml"), []byte(body), 0o644); err != nil {
		t.Fatalf("write identity.toml: %v", err)
	}
	return root
}

func pinnedIdentity(version string, schema int) string {
	body := "[project]\nid = \"proj\"\n\n[bd]\n"
	if version != "" {
		body += fmt.Sprintf("expected_version = %q\n", version)
	}
	if schema != 0 {
		body += fmt.Sprintf("schema_version = %d\n", schema)
	}
	return body
}

// invocations returns the argv lines the fake bd recorded.
func invocations(t *testing.T, logPath string) []string {
	t.Helper()
	data, err := os.ReadFile(logPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read invocation log: %v", err)
	}
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if strings.TrimSpace(line) != "" {
			out = append(out, line)
		}
	}
	return out
}

// assertNoRealInvocation fails when the log contains anything other than the
// resolver's own verification probes. This is the zero-invocation invariant:
// refusing after running bd once would already have permitted the corruption.
func assertNoRealInvocation(t *testing.T, logPath string) {
	t.Helper()
	for _, got := range invocations(t, logPath) {
		if got == "version" || strings.HasPrefix(got, "context ") || got == "context" {
			continue
		}
		t.Fatalf("bd was invoked for real work despite a refusal: %q", got)
	}
}

func runBd(t *testing.T, scopeRoot string, args ...string) ([]byte, error) {
	t.Helper()
	return ExecCommandRunner()(scopeRoot, "bd", args...)
}

// TestVersionMismatchRefusesBeforeInvoking is the headline fail-closed case:
// the binary on PATH is bd 1.0.5, the scope pins 1.1.0, and the mismatched
// binary must never receive the caller's command.
func TestVersionMismatchRefusesBeforeInvoking(t *testing.T) {
	binDir, logPath := fakeBd(t, "1.0.5", 53)
	t.Setenv("PATH", binDir)
	root := scope(t, pinnedIdentity("1.1.0", 0))

	_, err := runBd(t, root, "list", "--all")
	if err == nil {
		t.Fatal("resolver permitted a version-mismatched bd")
	}
	if !errors.Is(err, ErrBdResolve) {
		t.Fatalf("error = %v, want ErrBdResolve", err)
	}
	if !strings.Contains(err.Error(), "1.0.5") || !strings.Contains(err.Error(), "1.1.0") {
		t.Fatalf("error must name both versions, got: %v", err)
	}
	assertNoRealInvocation(t, logPath)
}

// TestSchemaMismatchRefusesBeforeInvoking covers the corruption vector
// directly: a bd whose CLI version is fine but whose store reports a different
// schema than the scope pinned would write a mismatched schema on the next
// mutation.
func TestSchemaMismatchRefusesBeforeInvoking(t *testing.T) {
	binDir, logPath := fakeBd(t, "1.1.0", 42)
	t.Setenv("PATH", binDir)
	root := scope(t, pinnedIdentity("1.1.0", 53))

	_, err := runBd(t, root, "create", "boom")
	if err == nil {
		t.Fatal("resolver permitted a schema-mismatched bd")
	}
	if !errors.Is(err, ErrBdResolve) {
		t.Fatalf("error = %v, want ErrBdResolve", err)
	}
	if !strings.Contains(err.Error(), "42") || !strings.Contains(err.Error(), "53") {
		t.Fatalf("error must name both schema versions, got: %v", err)
	}
	assertNoRealInvocation(t, logPath)
}

// TestUnprovableBinaryRefuses proves "could not check" is not "checked and
// fine". The binary exists and is executable but fails every probe.
func TestUnprovableBinaryRefuses(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"version pin", pinnedIdentity("1.1.0", 0)},
		{"schema pin", pinnedIdentity("", 53)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			binDir, logPath := brokenBd(t)
			t.Setenv("PATH", binDir)
			root := scope(t, tc.body)

			_, err := runBd(t, root, "list")
			if err == nil {
				t.Fatal("resolver permitted a bd it could not verify")
			}
			if !errors.Is(err, ErrBdResolve) {
				t.Fatalf("error = %v, want ErrBdResolve", err)
			}
			assertNoRealInvocation(t, logPath)
		})
	}
}

// TestMalformedIdentityRefusesWithoutProbing asserts an unparseable
// identity.toml refuses outright — and, unlike a pin mismatch, does not even
// probe: gc cannot know whether the file carried a pin, so nothing may run.
func TestMalformedIdentityRefusesWithoutProbing(t *testing.T) {
	binDir, logPath := fakeBd(t, "1.1.0", 53)
	t.Setenv("PATH", binDir)
	root := scope(t, "[project]\nid = \"proj\"\n\n[bd]\nexpectd_version = \"1.1.0\"\n")

	_, err := runBd(t, root, "list")
	if err == nil {
		t.Fatal("resolver accepted an identity.toml with an unknown key")
	}
	if !errors.Is(err, ErrBdResolve) {
		t.Fatalf("error = %v, want ErrBdResolve", err)
	}
	if got := invocations(t, logPath); len(got) != 0 {
		t.Fatalf("malformed identity must not run bd at all, got %v", got)
	}
}

// TestUnpinnedScopesStayInert pins the fleet-safety property: scopes that never
// opted in behave exactly as they did before Seam B. If this ever fails, an
// upgrade bricks every already-registered scope.
func TestUnpinnedScopesStayInert(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"no identity file", ""},
		{"identity without [bd]", "[project]\nid = \"proj\"\n"},
		{"empty [bd] section", "[project]\nid = \"proj\"\n\n[bd]\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			binDir, logPath := fakeBd(t, "0.0.1", 1)
			t.Setenv("PATH", binDir)
			root := scope(t, tc.body)

			if _, err := runBd(t, root, "list"); err != nil {
				t.Fatalf("unpinned scope must not be gated, got: %v", err)
			}
			if got := invocations(t, logPath); len(got) != 1 || got[0] != "list" {
				t.Fatalf("expected exactly the caller's command, got %v", got)
			}
		})
	}
}

// TestMatchingPinInvokesResolvedAbsolutePath proves the happy path both runs
// and runs the RIGHT file: the command reaching os/exec is the verified
// absolute path, not the bare name re-resolved against the child's PATH.
func TestMatchingPinInvokesResolvedAbsolutePath(t *testing.T) {
	binDir, logPath := fakeBd(t, "1.1.0", 53)
	t.Setenv("PATH", binDir)
	root := scope(t, pinnedIdentity("1.1.0", 53))

	if _, err := runBd(t, root, "list"); err != nil {
		t.Fatalf("matching pin must be permitted, got: %v", err)
	}
	got := invocations(t, logPath)
	if len(got) == 0 || got[len(got)-1] != "list" {
		t.Fatalf("caller's command did not reach bd, got %v", got)
	}

	resolved, err := ResolveBdBinaryForScope(context.Background(), root, nil)
	if err != nil {
		t.Fatalf("ResolveBdBinaryForScope: %v", err)
	}
	if want := filepath.Join(binDir, "bd"); resolved != want {
		t.Fatalf("resolved = %q, want the absolute pinned binary %q", resolved, want)
	}
}

// TestPathSwapCannotSatisfyAPin is the bead's "global PATH swap is explicitly
// insufficient" invariant, stated as an attack: an operator who puts a
// perfectly good bd first on PATH still cannot make a scope pinned to a
// different version run. Enforcement is keyed on the scope, not the search
// path, so reordering PATH is not a remedy.
func TestPathSwapCannotSatisfyAPin(t *testing.T) {
	wrongDir, wrongLog := fakeBd(t, "1.0.5", 53)
	rightDir, rightLog := fakeBd(t, "1.1.0", 53)
	root := scope(t, pinnedIdentity("9.9.9", 0))

	// Whichever binary the operator promotes to the front of PATH, the pin
	// names a version neither of them is — so both orderings must refuse.
	for _, order := range [][]string{{wrongDir, rightDir}, {rightDir, wrongDir}} {
		t.Setenv("PATH", strings.Join(order, string(os.PathListSeparator)))
		_, err := runBd(t, root, "list")
		if err == nil {
			t.Fatalf("PATH order %v satisfied a pin it does not match", order)
		}
		if !errors.Is(err, ErrBdResolve) {
			t.Fatalf("error = %v, want ErrBdResolve", err)
		}
	}
	assertNoRealInvocation(t, wrongLog)
	assertNoRealInvocation(t, rightLog)
}

// TestPerScopeIsolation proves the pin does not leak across scopes: one PATH,
// one process, two scopes with different pins, and each gets its own verdict.
// A global check could not produce this result.
func TestPerScopeIsolation(t *testing.T) {
	binDir, logPath := fakeBd(t, "1.1.0", 53)
	t.Setenv("PATH", binDir)

	allowed := scope(t, pinnedIdentity("1.1.0", 53))
	refused := scope(t, pinnedIdentity("9.9.9", 53))

	if _, err := runBd(t, allowed, "list"); err != nil {
		t.Fatalf("matching scope must be permitted, got: %v", err)
	}
	if _, err := runBd(t, refused, "list"); err == nil {
		t.Fatal("mismatched scope was permitted — the pin leaked across scopes")
	}

	// Exactly one real "list" reached bd: the allowed scope's.
	lists := 0
	for _, got := range invocations(t, logPath) {
		if got == "list" {
			lists++
		}
	}
	if lists != 1 {
		t.Fatalf("expected exactly 1 permitted invocation, got %d", lists)
	}
}

// TestReplacingTheBinaryReverifies attacks the cache: a refusal must not be
// sticky once the operator actually fixes the binary. The cache key carries the
// binary's identity, so replacing it in place re-runs verification.
func TestReplacingTheBinaryReverifies(t *testing.T) {
	binDir, _ := fakeBd(t, "1.0.5", 53)
	t.Setenv("PATH", binDir)
	root := scope(t, pinnedIdentity("1.1.0", 53))

	if _, err := runBd(t, root, "list"); err == nil {
		t.Fatal("version-mismatched bd was permitted")
	}

	// The operator installs the correct bd over the wrong one.
	fixedDir, fixedLog := fakeBd(t, "1.1.0", 53)
	fixed, err := os.ReadFile(filepath.Join(fixedDir, "bd"))
	if err != nil {
		t.Fatalf("read fixed bd: %v", err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "bd"), fixed, 0o755); err != nil {
		t.Fatalf("replace bd: %v", err)
	}

	if _, err := runBd(t, root, "list"); err != nil {
		t.Fatalf("resolver stayed refused after the binary was fixed: %v", err)
	}
	// The replacement logs to the fixed fake's path, proving the new binary ran.
	if got := invocations(t, fixedLog); len(got) == 0 {
		t.Fatal("replacement binary was never invoked")
	}
}

// TestRefusalIsNotAnExecFailure guards the error contract session-launch and
// operators depend on: a Seam B refusal must be distinguishable from bd running
// and failing, or callers cannot tell "gc blocked this" from "bd errored".
func TestRefusalIsNotAnExecFailure(t *testing.T) {
	binDir, _ := fakeBd(t, "1.0.5", 53)
	t.Setenv("PATH", binDir)
	root := scope(t, pinnedIdentity("1.1.0", 0))

	_, err := runBd(t, root, "list")
	if !errors.Is(err, ErrBdResolve) {
		t.Fatalf("refusal must wrap ErrBdResolve, got %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(binDir, "bd")); statErr != nil {
		t.Fatalf("precondition: fake bd should exist, got %v", statErr)
	}
	if !strings.Contains(err.Error(), root) {
		t.Fatalf("refusal must name the scope it refused for, got %v", err)
	}
}

// TestResolverRefusalYieldsNoBlessedBinary covers the agent-PATH contract: on
// refusal ResolveBdBinaryForScope must return "" alongside the error, so a
// caller that ignores the error still cannot project an unverified binary onto
// an agent's PATH.
func TestResolverRefusalYieldsNoBlessedBinary(t *testing.T) {
	binDir, _ := fakeBd(t, "1.0.5", 53)
	t.Setenv("PATH", binDir)
	root := scope(t, pinnedIdentity("1.1.0", 0))

	resolved, err := ResolveBdBinaryForScope(context.Background(), root, nil)
	if err == nil {
		t.Fatal("expected a refusal")
	}
	if resolved != "" {
		t.Fatalf("refusal returned a blessed path %q; it must return \"\"", resolved)
	}
}

// TestUnpinnedScopeBlessesNothing is the fleet-safety counterpart to
// TestUnpinnedScopesStayInert, for the agent-PATH surface. An unpinned scope
// must project NOTHING onto a session's PATH: rearranging PATH for scopes that
// never opted in is a behavior change beyond the pin's remit, and it displaced
// the gc binary from its guaranteed first position when it was first written.
func TestUnpinnedScopeBlessesNothing(t *testing.T) {
	binDir, _ := fakeBd(t, "1.1.0", 53)
	t.Setenv("PATH", binDir)

	for _, tc := range []struct {
		name string
		body string
	}{
		{"no identity file", ""},
		{"identity without [bd]", "[project]\nid = \"proj\"\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resolved, err := ResolveBdBinaryForScope(context.Background(), scope(t, tc.body), nil)
			if err != nil {
				t.Fatalf("unpinned scope returned an error: %v", err)
			}
			if resolved != "" {
				t.Fatalf("unpinned scope blessed %q; it must bless nothing", resolved)
			}
		})
	}

	// An empty scope root (an agent with no resolvable scope) is also inert.
	if resolved, err := ResolveBdBinaryForScope(context.Background(), "", nil); err != nil || resolved != "" {
		t.Fatalf("empty scope = (%q, %v), want (\"\", nil)", resolved, err)
	}
}

// TestPinnedScopeBlessesTheVerifiedBinary is the positive half: an opted-in
// scope does get its verified binary back, so the PATH projection has something
// to work with.
func TestPinnedScopeBlessesTheVerifiedBinary(t *testing.T) {
	binDir, _ := fakeBd(t, "1.1.0", 53)
	t.Setenv("PATH", binDir)
	root := scope(t, pinnedIdentity("1.1.0", 53))

	resolved, err := ResolveBdBinaryForScope(context.Background(), root, nil)
	if err != nil {
		t.Fatalf("ResolveBdBinaryForScope: %v", err)
	}
	if want := filepath.Join(binDir, "bd"); resolved != want {
		t.Fatalf("resolved = %q, want %q", resolved, want)
	}
}
