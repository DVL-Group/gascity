// L1 reader/writer for project identity. The L1 layer is the
// canonical, git-tracked source of truth for a beads scope's
// project_id. This file owns reads and writes of L1; reconcile across
// L1/L2/L3 lives in EnsureProjectIdentity (a sibling bead). External
// packages must route writes through WriteProjectIdentity.

package contract

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/gastownhall/gascity/internal/fsys"
)

// ProjectIdentityPath returns the canonical L1 path for a scope.
//
// The L1 file is "<scopeRoot>/.beads/identity.toml". This helper
// centralizes the construction so callers (doctor, error messages,
// reconcile) name the file consistently and survive future scope-path
// normalization.
func ProjectIdentityPath(scopeRoot string) string {
	return filepath.Join(scopeRoot, ".beads", "identity.toml")
}

// ReadProjectIdentity reads the L1 project_id for a scope.
//
// The bool reports whether a usable id was found. Both an absent file
// and a present file with an empty (or whitespace-only) project.id
// return ("", false, nil) — callers must treat both as "L1 not yet
// populated" (legacy rig). A missing [project] section is also
// treated as not-yet-populated; only a malformed document or one
// with unknown keys is an error.
//
// Parse strictness is intentional: unknown keys at the top level or
// inside [project] are rejected with an error wrapped to include the
// file path. This catches typos before they cascade into reconcile
// mismatches.
//
// scopeRoot is the parent of the .beads/ directory (city or rig
// root). The function joins scopeRoot/.beads/identity.toml itself;
// callers should not construct the path.
func ReadProjectIdentity(fs fsys.FS, scopeRoot string) (string, bool, error) {
	path := ProjectIdentityPath(scopeRoot)
	data, err := fs.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("read identity %s: %w", path, err)
	}

	var d identityDoc
	if err := decodeIdentityDoc(path, data, &d); err != nil {
		return "", false, err
	}

	id := strings.TrimSpace(d.Project.ID)
	if id == "" {
		return "", false, nil
	}
	return id, true, nil
}

// identityDoc is the whole strict shape of .beads/identity.toml.
//
// Every section the file may legally carry must appear here: the decoder
// below rejects unknown keys, so a section that is modeled in one reader
// but not another would make a legitimate file unreadable through the
// other. The [bd] section (Seam B) is therefore declared alongside
// [project] even though ReadProjectIdentity ignores its contents.
type identityDoc struct {
	Project identityProject `toml:"project"`
	BD      identityBD      `toml:"bd"`
}

type identityProject struct {
	ID string `toml:"id"`
}

type identityBD struct {
	ExpectedVersion string `toml:"expected_version"`
	SchemaVersion   int    `toml:"schema_version"`
}

// decodeIdentityDoc parses an identity.toml body into d, rejecting unknown
// keys. Strictness is intentional (see ReadProjectIdentity): it catches typos
// before they cascade into reconcile mismatches, and — for the [bd] pin — before
// a misspelled key silently disables the fail-closed resolver.
func decodeIdentityDoc(path string, data []byte, d *identityDoc) error {
	md, err := toml.Decode(string(data), d)
	if err != nil {
		return fmt.Errorf("parse identity %s: %w", path, err)
	}
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, len(undecoded))
		for i, k := range undecoded {
			keys[i] = k.String()
		}
		return fmt.Errorf("parse identity %s: unexpected keys %v", path, keys)
	}
	return nil
}

// BDPin is a scope's expected bd CLI version and target store schema version.
//
// It is the per-scope half of the Seam B resolver contract (spike 002 §4): the
// resolver refuses to invoke bd for the scope when the on-disk binary's version
// or the store's schema disagrees with these values. A zero BDPin means the
// scope is unpinned and the resolver stays inert for it.
type BDPin struct {
	// ExpectedVersion is the exact bd CLI version required for this scope,
	// without a leading "v" (for example "1.1.0"). Empty means no version pin.
	ExpectedVersion string
	// SchemaVersion is the store schema version this scope's bd must report.
	// Zero means no schema pin.
	SchemaVersion int
}

// IsZero reports whether the pin constrains nothing.
func (p BDPin) IsZero() bool {
	return strings.TrimSpace(p.ExpectedVersion) == "" && p.SchemaVersion == 0
}

// ReadBDPin reads the L1 [bd] pin for a scope.
//
// The bool reports whether a constraining pin was found. An absent file, a file
// with no [bd] section, and a [bd] section whose fields are all zero all return
// (BDPin{}, false, nil) — every one of those means "this scope is unpinned",
// and the resolver treats them identically.
//
// A malformed or unreadable identity.toml returns an error and is NOT reported
// as unpinned. The distinction is load-bearing: callers must not read a parse
// failure as permission to skip enforcement, because a file that cannot be
// parsed may well carry a pin.
//
// scopeRoot is the parent of the .beads/ directory; the function joins the path
// itself.
func ReadBDPin(fs fsys.FS, scopeRoot string) (BDPin, bool, error) {
	path := ProjectIdentityPath(scopeRoot)
	data, err := fs.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return BDPin{}, false, nil
		}
		return BDPin{}, false, fmt.Errorf("read identity %s: %w", path, err)
	}

	var d identityDoc
	if err := decodeIdentityDoc(path, data, &d); err != nil {
		return BDPin{}, false, err
	}

	pin := BDPin{
		ExpectedVersion: strings.TrimPrefix(strings.TrimSpace(d.BD.ExpectedVersion), "v"),
		SchemaVersion:   d.BD.SchemaVersion,
	}
	if pin.SchemaVersion < 0 {
		return BDPin{}, false, fmt.Errorf("parse identity %s: bd.schema_version %d is negative", path, pin.SchemaVersion)
	}
	if pin.IsZero() {
		return BDPin{}, false, nil
	}
	return pin, true, nil
}

// identityBodyTemplate is the canonical L1 file body. The two leading
// comment lines are part of the format (designer §10) so a `git diff`
// of the file reads as documentation, not as bytes.
const identityBodyTemplate = `# .beads/identity.toml — canonical, git-tracked.
# Edited only at scope creation or by deliberate human/` + "`gc`" + ` migration.

[project]
id = "%s"
`

// forbiddenIdentityChars are characters that cannot appear in a valid
// project id without corrupting the TOML body. Newline and CR would
// break the single-line `id = "..."` field; the double quote and
// backslash would either close or escape the TOML string.
const forbiddenIdentityChars = "\n\r\"\\"

// WriteProjectIdentity writes the L1 project_id for a scope.
//
// The id is trimmed before validation and serialization. Empty,
// whitespace-only, and ids containing newline (\n), carriage return
// (\r), double quote ("), or backslash (\) are rejected with an error
// that includes the offending value — these inputs would otherwise
// corrupt the TOML body.
//
// The function creates scopeRoot/.beads/ with mode 0o755 if the
// directory does not exist (designer §7.1). The file is written via
// fsys.WriteFileIfChangedAtomic, which provides atomicity (temp file +
// rename) and idempotence (no inode churn when content already
// matches). Symlinks at the target path are replaced with regular
// files (designer §7.2 / atomic.go).
//
// Concurrency: the file-level write is safe under concurrent calls
// passing the same id — the atomic rename ensures readers never see
// partial content. The contract does not serialize callers passing
// *different* ids; that policy lives upstream in the reconciler.
func WriteProjectIdentity(fs fsys.FS, scopeRoot string, id string) error {
	return WriteProjectIdentityWithBDPin(fs, scopeRoot, id, BDPin{})
}

// WriteProjectIdentityWithBDPin writes the L1 project_id and, when pin is
// non-zero, the scope's [bd] resolver pin.
//
// A zero pin produces a file byte-identical to the pre-Seam-B format, so
// existing unpinned scopes never churn when their identity is rewritten. A
// non-zero pin appends a [bd] section carrying only the fields it constrains.
//
// The version is validated against the same character rules as the id: a value
// that would break the single-line TOML string is rejected rather than written.
// A negative schema version is rejected — ReadBDPin treats zero as "no pin", so
// a negative value could only be a caller error.
func WriteProjectIdentityWithBDPin(fs fsys.FS, scopeRoot string, id string, pin BDPin) error {
	if strings.ContainsAny(id, forbiddenIdentityChars) {
		return fmt.Errorf("write identity: id %q contains forbidden character (newline, CR, quote, or backslash)", id)
	}
	trimmed := strings.TrimSpace(id)
	if trimmed == "" {
		return fmt.Errorf("write identity: id %q is empty or whitespace-only", id)
	}
	version := strings.TrimSpace(pin.ExpectedVersion)
	if strings.ContainsAny(version, forbiddenIdentityChars) {
		return fmt.Errorf("write identity: bd expected_version %q contains forbidden character (newline, CR, quote, or backslash)", pin.ExpectedVersion)
	}
	if pin.SchemaVersion < 0 {
		return fmt.Errorf("write identity: bd schema_version %d is negative", pin.SchemaVersion)
	}

	path := ProjectIdentityPath(scopeRoot)
	dir := filepath.Dir(path)
	if err := fs.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}

	body := fmt.Sprintf(identityBodyTemplate, trimmed)
	if section := bdPinSection(BDPin{ExpectedVersion: version, SchemaVersion: pin.SchemaVersion}); section != "" {
		body += section
	}
	if err := fsys.WriteFileIfChangedAtomic(fs, path, []byte(body), 0o644); err != nil {
		return fmt.Errorf("write identity %s: %w", path, err)
	}
	return nil
}

// bdPinSection renders the [bd] section for a pin, or "" when the pin
// constrains nothing. Only constrained fields are emitted so a version-only
// pin does not imply a schema_version of 0 on a re-read.
func bdPinSection(pin BDPin) string {
	if pin.IsZero() {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n[bd]\n")
	if pin.ExpectedVersion != "" {
		fmt.Fprintf(&b, "expected_version = %q\n", pin.ExpectedVersion)
	}
	if pin.SchemaVersion != 0 {
		fmt.Fprintf(&b, "schema_version = %d\n", pin.SchemaVersion)
	}
	return b.String()
}
