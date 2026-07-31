package contract

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

func writeIdentityBody(t *testing.T, body string) (fsys.FS, string) {
	t.Helper()
	root := t.TempDir()
	fs := fsys.OSFS{}
	if err := fs.MkdirAll(filepath.Join(root, ".beads"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := fs.WriteFile(ProjectIdentityPath(root), []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	return fs, root
}

// TestUnpinnedWriteIsByteIdenticalToLegacyFormat is a churn guard, not a
// formatting preference. WriteProjectIdentity is called on paths that run
// against every registered scope; if Seam B changed its output, every scope in
// the fleet would show a spurious identity.toml diff on the next write.
func TestUnpinnedWriteIsByteIdenticalToLegacyFormat(t *testing.T) {
	fs := fsys.OSFS{}
	root := t.TempDir()
	if err := WriteProjectIdentity(fs, root, "proj-1"); err != nil {
		t.Fatalf("WriteProjectIdentity: %v", err)
	}
	got, err := fs.ReadFile(ProjectIdentityPath(root))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	want := "# .beads/identity.toml — canonical, git-tracked.\n" +
		"# Edited only at scope creation or by deliberate human/`gc` migration.\n" +
		"\n[project]\nid = \"proj-1\"\n"
	if string(got) != want {
		t.Fatalf("unpinned identity body changed.\n got: %q\nwant: %q", got, want)
	}
	if _, pinned, err := ReadBDPin(fs, root); err != nil || pinned {
		t.Fatalf("ReadBDPin on unpinned write = (pinned=%v, err=%v), want (false, nil)", pinned, err)
	}
}

// TestPinnedIdentityRemainsReadableByProjectIDReader closes the cross-reader
// break Seam B could have introduced: identity.toml is parsed strictly, so a
// [bd] section that only ReadBDPin knew about would make every pinned scope
// unreadable to ReadProjectIdentity — which is on the registration path.
func TestPinnedIdentityRemainsReadableByProjectIDReader(t *testing.T) {
	fs := fsys.OSFS{}
	root := t.TempDir()
	pin := BDPin{ExpectedVersion: "1.1.0", SchemaVersion: 53}
	if err := WriteProjectIdentityWithBDPin(fs, root, "proj-2", pin); err != nil {
		t.Fatalf("WriteProjectIdentityWithBDPin: %v", err)
	}

	id, ok, err := ReadProjectIdentity(fs, root)
	if err != nil {
		t.Fatalf("ReadProjectIdentity on a pinned file: %v", err)
	}
	if !ok || id != "proj-2" {
		t.Fatalf("ReadProjectIdentity = (%q, %v), want (\"proj-2\", true)", id, ok)
	}

	gotPin, pinned, err := ReadBDPin(fs, root)
	if err != nil || !pinned {
		t.Fatalf("ReadBDPin = (pinned=%v, err=%v), want (true, nil)", pinned, err)
	}
	if gotPin != pin {
		t.Fatalf("pin round-trip = %+v, want %+v", gotPin, pin)
	}
}

// TestVersionOnlyPinDoesNotImplyASchemaPin guards a silent over-enforcement:
// emitting schema_version = 0 for a version-only pin would be read back as a
// pin the operator never wrote, and the resolver would start demanding a schema
// probe for a scope that only asked for a version check.
func TestVersionOnlyPinDoesNotImplyASchemaPin(t *testing.T) {
	fs := fsys.OSFS{}
	root := t.TempDir()
	if err := WriteProjectIdentityWithBDPin(fs, root, "proj-3", BDPin{ExpectedVersion: "1.1.0"}); err != nil {
		t.Fatalf("write: %v", err)
	}
	body, err := fs.ReadFile(ProjectIdentityPath(root))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if strings.Contains(string(body), "schema_version") {
		t.Fatalf("version-only pin emitted a schema_version key:\n%s", body)
	}
	pin, pinned, err := ReadBDPin(fs, root)
	if err != nil || !pinned {
		t.Fatalf("ReadBDPin = (pinned=%v, err=%v), want (true, nil)", pinned, err)
	}
	if pin.SchemaVersion != 0 {
		t.Fatalf("SchemaVersion = %d, want 0", pin.SchemaVersion)
	}
}

// TestReadBDPinRejectsTypos proves a misspelled pin key fails loudly. A
// silently-ignored typo would leave the operator believing a scope is pinned
// while the resolver treats it as unpinned — enforcement lost, no signal.
func TestReadBDPinRejectsTypos(t *testing.T) {
	fs, root := writeIdentityBody(t, "[project]\nid = \"p\"\n\n[bd]\nexpectd_version = \"1.1.0\"\n")
	if _, pinned, err := ReadBDPin(fs, root); err == nil {
		t.Fatalf("typo accepted (pinned=%v); a misspelled pin must not read as unpinned", pinned)
	}
}

// TestReadBDPinDistinguishesUnreadableFromUnpinned is the fail-closed hinge:
// a parse failure must surface as an error, never as (false, nil), because the
// resolver reads "not pinned" as "nothing to enforce".
func TestReadBDPinDistinguishesUnreadableFromUnpinned(t *testing.T) {
	fs, root := writeIdentityBody(t, "this is not toml {{{\n")
	pin, pinned, err := ReadBDPin(fs, root)
	if err == nil {
		t.Fatal("malformed identity.toml parsed without error")
	}
	if pinned || !pin.IsZero() {
		t.Fatalf("malformed read returned pin=%+v pinned=%v, want zero/false", pin, pinned)
	}

	// Absent file is the genuinely-unpinned case and must NOT be an error.
	if _, pinned, err := ReadBDPin(fsys.OSFS{}, t.TempDir()); err != nil || pinned {
		t.Fatalf("absent identity = (pinned=%v, err=%v), want (false, nil)", pinned, err)
	}
}

// TestReadBDPinNormalizesVersionPrefix keeps the pin comparable to what bd
// reports: bd prints "1.1.0" while operators habitually write "v1.1.0", and a
// literal comparison of those two would refuse a correctly-matched binary.
func TestReadBDPinNormalizesVersionPrefix(t *testing.T) {
	fs, root := writeIdentityBody(t, "[project]\nid = \"p\"\n\n[bd]\nexpected_version = \"v1.1.0\"\n")
	pin, pinned, err := ReadBDPin(fs, root)
	if err != nil || !pinned {
		t.Fatalf("ReadBDPin = (pinned=%v, err=%v), want (true, nil)", pinned, err)
	}
	if pin.ExpectedVersion != "1.1.0" {
		t.Fatalf("ExpectedVersion = %q, want %q", pin.ExpectedVersion, "1.1.0")
	}
}

// TestWriteRejectsCorruptingPinValues checks the writer cannot be used to
// smuggle TOML-breaking content into the file, which would make the scope
// unreadable — and therefore, per the fail-closed contract, unusable.
func TestWriteRejectsCorruptingPinValues(t *testing.T) {
	fs := fsys.OSFS{}
	for _, tc := range []struct {
		name string
		pin  BDPin
	}{
		{"quote in version", BDPin{ExpectedVersion: "1.1.0\" evil = \""}},
		{"newline in version", BDPin{ExpectedVersion: "1.1.0\nid = \"x\""}},
		{"negative schema", BDPin{SchemaVersion: -1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := WriteProjectIdentityWithBDPin(fs, t.TempDir(), "proj", tc.pin); err == nil {
				t.Fatal("writer accepted a corrupting pin value")
			}
		})
	}
}
