// Seam B (spike 002 §4, dac-y7mg.2): the per-scope fail-closed bd CLI resolver.
//
// Every gc-managed bd invocation goes through the Beads CommandRunner
// (execCommandRunnerWithEnv). Before Seam B that runner handed the bare name
// "bd" to os/exec, which resolved it against the ambient PATH of whatever
// process happened to be calling — a shell-dependent composition. On a host
// carrying more than one bd (the normal state of this fleet: bd itself prints
// "Warning: multiple 'bd' binaries found in PATH") that means gc could not say
// which binary wrote to a store, and a bd whose CLI version disagrees with the
// store's schema corrupts on write rather than refusing.
//
// This file closes that. For each scope, before every invocation, the resolver:
//
//  1. resolves an EXACT ABSOLUTE bd binary for that scope, and
//  2. verifies the binary's version and the store's schema against the pin in
//     the scope's .beads/identity.toml, and
//  3. refuses to invoke bd at all when either disagrees.
//
// A global PATH swap is explicitly insufficient and this design says why in
// code: enforcement is keyed on the scope directory, the decision is recorded
// per scope, and the verified absolute path — not the name "bd" — is what
// reaches os/exec. Swapping PATH cannot satisfy a pin, and cannot make a
// mismatched binary reachable.
//
// # Fail-closed contract
//
// The enforcement trigger is the pin, not the file:
//
//   - Pin present, everything agrees      -> invoke, using the absolute path.
//   - Pin present, version disagrees      -> REFUSE. bd is never invoked.
//   - Pin present, schema disagrees       -> REFUSE. bd is never invoked.
//   - Pin present, either unprovable      -> REFUSE. Being unable to prove the
//     binary or the schema is the corruption case this seam exists to stop;
//     "could not check" must never read as "checked and fine".
//   - identity.toml malformed/unreadable  -> REFUSE. A file that will not parse
//     may well carry a pin, so it cannot be treated as unpinned.
//   - No pin (absent file, or no [bd])    -> INERT. Resolution still upgrades
//     the name to an absolute path when it can, but nothing is enforced and
//     nothing new can fail. Unpinned scopes behave exactly as before Seam B.
//
// The inert default is deliberate. Pinning is an opt-in a scope takes at
// registration; making an absent pin fatal would brick every already-registered
// scope in the fleet on upgrade, which is a bigger outage than the one this
// guards against. Enforcement is dark until a scope is pinned, and total once
// it is.

package beads

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/fsys"
)

// ErrBdResolve is the sentinel wrapping every fail-closed resolver refusal.
// Callers distinguishing "gc refused to run bd" from "bd ran and failed" match
// on this with errors.Is.
var ErrBdResolve = errors.New("bd resolver")

// bdResolveProbeTimeout bounds each verification probe. It is well under
// bdCommandTimeout: a probe that hangs must not consume the caller's whole
// budget before the refusal is reported, and an unresponsive probe is itself a
// fail-closed outcome.
const bdResolveProbeTimeout = 30 * time.Second

// bdVersionRE extracts a semver token from `bd version` / `bd context` output.
// bd prints "bd version 1.1.0 (8e4e59d39: HEAD@8e4e59d39f34)"; only the semver
// core is compared, so build metadata never causes a false mismatch.
var bdVersionRE = regexp.MustCompile(`\b(\d+\.\d+\.\d+)\b`)

// bdResolution is a completed, cached verification for one scope.
type bdResolution struct {
	// path is the absolute bd binary this scope must use.
	path string
	// version is the semver reported by the binary, when probed.
	version string
	// schema is the store schema version bd reported, when probed.
	schema int
	// err is the fail-closed refusal for this scope, if any. When err is
	// non-nil the other fields are diagnostic only and path must not be run.
	err error
}

// bdResolveCacheKey identifies a verification result. Including the binary's
// identity (path, size, mtime) means the cache self-invalidates on the two
// remediations an operator actually performs — swapping which bd is found, or
// replacing the binary in place — without a TTL that would re-probe on every
// call. The pin is included so editing identity.toml re-verifies too.
type bdResolveCacheKey struct {
	scope   string
	binPath string
	binSize int64
	binMod  int64
	pin     contract.BDPin
}

var bdResolveCache sync.Map // map[bdResolveCacheKey]*bdResolution

// bdResolveLookPath is exec.LookPath, indirected for tests.
var bdResolveLookPath = exec.LookPath

// bdResolveIdentityFS reads .beads/identity.toml. Indirected for tests.
var bdResolveIdentityFS fsys.FS = fsys.OSFS{}

// resolveBdCommand returns the absolute bd binary that scopeDir must use, after
// verifying it against the scope's pin.
//
// It is called by execCommandRunnerWithEnv for every `bd` invocation. A
// non-nil error means gc must NOT invoke bd: the caller returns it without
// executing anything, which is what makes the contract fail-closed rather than
// fail-noisy.
//
// env is the caller's environment overrides for the scope; probes run with the
// same overrides so the schema they read is the schema the real invocation
// would use.
func resolveBdCommand(ctx context.Context, scopeDir string, env map[string]string) (string, error) {
	pin, pinned, err := contract.ReadBDPin(bdResolveIdentityFS, scopeDir)
	if err != nil {
		// Unparseable identity.toml. It may carry a pin; refusing is the only
		// answer that cannot silently skip enforcement.
		return "", fmt.Errorf("%w: scope %s: identity is unreadable, refusing to invoke bd: %w", ErrBdResolve, scopeDir, err)
	}

	path, lookErr := bdResolveLookPath("bd")
	if lookErr != nil {
		if !pinned {
			// Inert: preserve pre-Seam-B behavior exactly. os/exec will
			// produce its own "executable file not found" for the bare name.
			return "", nil
		}
		return "", fmt.Errorf("%w: scope %s: pinned to bd %s but no bd is resolvable: %w",
			ErrBdResolve, scopeDir, describeBDPin(pin), lookErr)
	}
	if abs, absErr := filepath.Abs(path); absErr == nil {
		path = abs
	}
	if !pinned {
		// Inert, but still upgrade to the absolute path: it removes the
		// child's own PATH from the decision without enforcing anything.
		return path, nil
	}

	key := bdResolveCacheKeyFor(scopeDir, path, pin)
	if cached, ok := bdResolveCache.Load(key); ok {
		return bdResolutionResult(cached.(*bdResolution))
	}

	res := verifyBdForScope(ctx, scopeDir, path, pin, env)
	actual, _ := bdResolveCache.LoadOrStore(key, res)
	return bdResolutionResult(actual.(*bdResolution))
}

// bdResolutionResult unpacks a verification into the (path, error) pair callers
// see. A refusal yields an EMPTY path deliberately: res.path stays populated for
// diagnostics, but handing it back would let a caller that ignores the error
// run — or bless onto an agent's PATH — the very binary the pin rejected.
func bdResolutionResult(res *bdResolution) (string, error) {
	if res.err != nil {
		return "", res.err
	}
	return res.path, nil
}

// ResolveBdBinaryForScope returns the verified absolute bd binary a scope must
// use, or "" when none may be blessed.
//
// It is the agent-PATH half of Seam B. Session launch projects the returned
// path's directory onto the spawned agent's PATH, so an agent typing a bare
// `bd` reaches the same binary gc's own calls resolve to, rather than whatever
// its shell's PATH composition happens to find first.
//
// A refusal — pin mismatch, unprovable version or schema, unreadable identity —
// returns ("", err). Callers MUST NOT fall back to the ambient PATH on error:
// there is no blessed binary in that state, and prepending a directory anyway
// would hand the agent a mismatched bd wearing gc's endorsement. Projecting
// nothing leaves the agent no worse off than before Seam B, and gc's own
// invocations still refuse loudly through resolveBdCommand.
//
// An UNPINNED scope also returns ("", nil): there is nothing to bless, so
// nothing is projected and the agent's PATH is left exactly as it was before
// Seam B. Only a scope that opted into a pin gets its PATH rearranged — the
// same dark-until-pinned discipline resolveBdCommand applies to enforcement.
//
// Session launch is not a safe place to run the store-touching schema probe, so
// callers get whatever the cached per-scope verification already proved; a cold
// cache performs the same verification the first gc-mediated call would.
func ResolveBdBinaryForScope(ctx context.Context, scopeRoot string, env map[string]string) (string, error) {
	if strings.TrimSpace(scopeRoot) == "" {
		return "", nil
	}
	_, pinned, err := contract.ReadBDPin(bdResolveIdentityFS, scopeRoot)
	if err != nil {
		return "", fmt.Errorf("%w: scope %s: identity is unreadable, refusing to bless a bd binary: %w", ErrBdResolve, scopeRoot, err)
	}
	if !pinned {
		return "", nil
	}
	return resolveBdCommand(ctx, scopeRoot, env)
}

func bdResolveCacheKeyFor(scopeDir, binPath string, pin contract.BDPin) bdResolveCacheKey {
	key := bdResolveCacheKey{
		scope:   filepath.Clean(scopeDir),
		binPath: binPath,
		pin:     pin,
	}
	if info, err := os.Stat(binPath); err == nil {
		key.binSize = info.Size()
		key.binMod = info.ModTime().UnixNano()
	}
	return key
}

// verifyBdForScope probes the binary and compares both halves of the pin.
//
// A version pin alone is verified with `bd version`, which does not touch the
// store. A schema pin requires `bd context --json`, which reports the bd
// version and the store schema together — so a scope pinning both costs one
// probe, not two.
func verifyBdForScope(ctx context.Context, scopeDir, path string, pin contract.BDPin, env map[string]string) *bdResolution {
	res := &bdResolution{path: path}

	if pin.SchemaVersion != 0 {
		version, schema, err := probeBdContext(ctx, scopeDir, path, env)
		if err != nil {
			res.err = fmt.Errorf("%w: scope %s: cannot prove store schema for %s, refusing to invoke bd (pin %s): %w",
				ErrBdResolve, scopeDir, path, describeBDPin(pin), err)
			return res
		}
		res.version, res.schema = version, schema
		if schema != pin.SchemaVersion {
			res.err = fmt.Errorf("%w: scope %s: store schema is %d but identity.toml pins schema %d; %s would corrupt this store, refusing to invoke it",
				ErrBdResolve, scopeDir, schema, pin.SchemaVersion, path)
			return res
		}
	}

	if pin.ExpectedVersion == "" {
		return res
	}

	version := res.version
	if version == "" {
		probed, err := probeBdVersion(ctx, scopeDir, path, env)
		if err != nil {
			res.err = fmt.Errorf("%w: scope %s: cannot prove the version of %s, refusing to invoke it (pin %s): %w",
				ErrBdResolve, scopeDir, path, describeBDPin(pin), err)
			return res
		}
		version = probed
		res.version = probed
	}
	if version != pin.ExpectedVersion {
		res.err = fmt.Errorf("%w: scope %s: %s is bd %s but identity.toml pins bd %s, refusing to invoke it",
			ErrBdResolve, scopeDir, path, version, pin.ExpectedVersion)
		return res
	}
	return res
}

// probeBdVersion runs `<path> version` and returns the semver it reports.
func probeBdVersion(ctx context.Context, scopeDir, path string, env map[string]string) (string, error) {
	out, err := runBdProbe(ctx, scopeDir, path, env, "version")
	if err != nil {
		return "", err
	}
	m := bdVersionRE.FindSubmatch(out)
	if m == nil {
		return "", fmt.Errorf("bd version output carried no recognizable version")
	}
	return string(m[1]), nil
}

// probeBdContext runs `<path> context --json` and returns the bd version and
// store schema version it reports. A zero or absent schema_version is an error,
// not a pass: the pin cannot be checked against a value bd did not supply.
func probeBdContext(ctx context.Context, scopeDir, path string, env map[string]string) (string, int, error) {
	out, err := runBdProbe(ctx, scopeDir, path, env, "context", "--json")
	if err != nil {
		return "", 0, err
	}
	var raw struct {
		BDVersion     string `json:"bd_version"`
		SchemaVersion int    `json:"schema_version"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return "", 0, fmt.Errorf("parse bd context --json: %w", err)
	}
	if raw.SchemaVersion <= 0 {
		return "", 0, fmt.Errorf("bd context reported no schema version")
	}
	version := ""
	if m := bdVersionRE.FindStringSubmatch(raw.BDVersion); m != nil {
		version = m[1]
	}
	return version, raw.SchemaVersion, nil
}

// runBdProbe executes one verification probe directly against the absolute
// binary.
//
// It deliberately does NOT route back through execCommandRunnerWithEnv: that
// runner calls the resolver, so a probe taking the normal path would recurse.
// Probing the already-resolved absolute path is also the stronger check — it
// verifies the exact file that the real invocation will run, not whatever the
// name "bd" resolves to a moment later.
func runBdProbe(ctx context.Context, scopeDir, path string, env map[string]string, args ...string) ([]byte, error) {
	probeCtx, cancel := context.WithTimeout(ctx, bdResolveProbeTimeout)
	defer cancel()

	cmd := exec.CommandContext(probeCtx, path, args...)
	cmd.WaitDelay = 2 * time.Second
	prepareCommandForTimeout(cmd)
	cmd.Dir = scopeDir
	cmd.Cancel = func() error { return killCommandTree(cmd) }
	cmd.Env = execEnvFor("bd", processEnvSnapshotExcludingNativeDoltOpen(), env)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if probeCtx.Err() != nil {
			return nil, fmt.Errorf("probe %s %s timed out after %s", path, strings.Join(args, " "), bdResolveProbeTimeout)
		}
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return nil, fmt.Errorf("probe %s %s: %w: %s", path, strings.Join(args, " "), err, msg)
		}
		return nil, fmt.Errorf("probe %s %s: %w", path, strings.Join(args, " "), err)
	}
	return out, nil
}

// describeBDPin renders a pin for operator-facing error text.
func describeBDPin(pin contract.BDPin) string {
	parts := make([]string, 0, 2)
	if pin.ExpectedVersion != "" {
		parts = append(parts, "version "+pin.ExpectedVersion)
	}
	if pin.SchemaVersion != 0 {
		parts = append(parts, fmt.Sprintf("schema %d", pin.SchemaVersion))
	}
	if len(parts) == 0 {
		return "(unpinned)"
	}
	return strings.Join(parts, ", ")
}
