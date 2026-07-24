package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/api"
)

// Unit coverage for `gc beads materialize`. These stub the managed-Dolt ensure
// and the per-scope initAndHookDir funnel (via the command seams) so the scope
// plan, ordering, ensure-before-init sequencing, partial-failure handling, and
// the "no supervisor/session/provider" contract are asserted deterministically
// without a live Dolt. Real hydration / Seam A survival is covered by the
// integration file.

type materializeInitCall struct {
	cityPath string
	dir      string
	prefix   string
}

type materializeSpies struct {
	order     []string
	ensureErr error
	initErr   func(call materializeInitCall, n int) error
	initCalls []materializeInitCall
}

func (s *materializeSpies) ensureCount() int {
	n := 0
	for _, o := range s.order {
		if o == "ensure" {
			n++
		}
	}
	return n
}

// installMaterializeSpies swaps the command seams for recording stubs, restored
// on cleanup. No real Dolt or bd is touched.
func installMaterializeSpies(t *testing.T) *materializeSpies {
	t.Helper()
	s := &materializeSpies{}
	prevEnsure := beadsMaterializeEnsureDolt
	prevInit := beadsMaterializeInitScope
	beadsMaterializeEnsureDolt = func(_ string) error {
		s.order = append(s.order, "ensure")
		return s.ensureErr
	}
	beadsMaterializeInitScope = func(cityPath, dir, prefix string) error {
		s.order = append(s.order, "init")
		call := materializeInitCall{cityPath: cityPath, dir: dir, prefix: prefix}
		s.initCalls = append(s.initCalls, call)
		if s.initErr != nil {
			return s.initErr(call, len(s.initCalls))
		}
		return nil
	}
	t.Cleanup(func() {
		beadsMaterializeEnsureDolt = prevEnsure
		beadsMaterializeInitScope = prevInit
	})
	return s
}

type unitRig struct {
	name   string
	prefix string
}

// writeMaterializeUnitCity writes a minimal managed-bd city.toml (provider =
// "bd") with optional inherited rigs, points resolveCity at it via GC_CITY, and
// clears the --city/--rig flag globals so the env tier is reached. The rigs
// inherit the city's managed "bd" contract from the default provider, so no
// per-rig .beads files are required for the store-contract predicates.
func writeMaterializeUnitCity(t *testing.T, rigs ...unitRig) string {
	t.Helper()
	cityPath := t.TempDir()

	var b strings.Builder
	b.WriteString("[workspace]\nname = \"unit\"\nprefix = \"gc\"\n\n[beads]\nprovider = \"bd\"\n")
	for _, r := range rigs {
		fmt.Fprintf(&b, "\n[[rigs]]\nname = %q\npath = %q\nprefix = %q\n", r.name, r.name, r.prefix)
	}
	writeMaterializeUnitCityRaw(t, cityPath, b.String())
	for _, r := range rigs {
		if err := os.MkdirAll(filepath.Join(cityPath, r.name, ".beads"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return cityPath
}

func writeMaterializeUnitCityRaw(t *testing.T, cityPath, cityToml string) {
	t.Helper()
	// Point resolveCity() at the fixture through the --city flag global rather
	// than GC_CITY: the flag is resolved before any env var, so these unit tests
	// touch zero process-environment state (keeps them off the cmd/gc env-census
	// ratchet). GC_BEADS is deliberately NOT forced — the city.toml [beads]
	// provider must stay the authoritative signal the bd-store guard reads, and
	// under the mandated env -i proof lane ambient GC_* is stripped anyway.
	prevCity, prevRig := cityFlag, rigFlag
	cityFlag, rigFlag = cityPath, ""
	t.Cleanup(func() { cityFlag, rigFlag = prevCity, prevRig })

	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestBeadsMaterialize_DefaultScopeIsCityAndEnsuresDoltFirst(t *testing.T) {
	cityPath := writeMaterializeUnitCity(t)
	spies := installMaterializeSpies(t)

	if code := cmdBeadsMaterialize(beadsMaterializeOptions{}, os.Stderr, os.Stderr); code != 0 {
		t.Fatalf("cmdBeadsMaterialize() = %d, want 0", code)
	}
	if spies.ensureCount() != 1 {
		t.Fatalf("ensure-dolt called %d times, want 1", spies.ensureCount())
	}
	if len(spies.initCalls) != 1 {
		t.Fatalf("initAndHookDir called %d times, want 1 (city only)", len(spies.initCalls))
	}
	got := spies.initCalls[0]
	if got.cityPath != cityPath || got.dir != cityPath || got.prefix != "gc" {
		t.Fatalf("city init call = %+v, want {cityPath:%s dir:%s prefix:gc}", got, cityPath, cityPath)
	}
	if len(spies.order) == 0 || spies.order[0] != "ensure" {
		t.Fatalf("call order = %v, want managed Dolt ensured before any init", spies.order)
	}
}

func TestBeadsMaterialize_RigByNameScopesToThatRig(t *testing.T) {
	cityPath := writeMaterializeUnitCity(t, unitRig{name: "backend", prefix: "ba"})
	spies := installMaterializeSpies(t)

	if code := cmdBeadsMaterialize(beadsMaterializeOptions{rigs: []string{"backend"}}, os.Stderr, os.Stderr); code != 0 {
		t.Fatalf("cmdBeadsMaterialize(--rig backend) = %d, want 0", code)
	}
	if len(spies.initCalls) != 1 {
		t.Fatalf("initAndHookDir called %d times, want 1 (rig only, city not included)", len(spies.initCalls))
	}
	got := spies.initCalls[0]
	wantDir := filepath.Join(cityPath, "backend")
	if got.dir != wantDir || got.prefix != "ba" {
		t.Fatalf("rig init call = %+v, want {dir:%s prefix:ba}", got, wantDir)
	}
}

func TestBeadsMaterialize_UnknownRigFailsClosedWithoutEnsuringDolt(t *testing.T) {
	writeMaterializeUnitCity(t, unitRig{name: "backend", prefix: "ba"})
	spies := installMaterializeSpies(t)

	if code := cmdBeadsMaterialize(beadsMaterializeOptions{rigs: []string{"nope"}}, os.Stderr, os.Stderr); code != 1 {
		t.Fatalf("cmdBeadsMaterialize(--rig nope) = %d, want 1", code)
	}
	if spies.ensureCount() != 0 || len(spies.initCalls) != 0 {
		t.Fatalf("a bad --rig name must not start Dolt or init any scope; ensure=%d init=%d", spies.ensureCount(), len(spies.initCalls))
	}
}

func TestBeadsMaterialize_AllScopesCityThenEveryRigInOrder(t *testing.T) {
	cityPath := writeMaterializeUnitCity(t, unitRig{name: "backend", prefix: "ba"}, unitRig{name: "worker", prefix: "wo"})
	spies := installMaterializeSpies(t)

	if code := cmdBeadsMaterialize(beadsMaterializeOptions{all: true}, os.Stderr, os.Stderr); code != 0 {
		t.Fatalf("cmdBeadsMaterialize(--all) = %d, want 0", code)
	}
	if len(spies.initCalls) != 3 {
		t.Fatalf("initAndHookDir called %d times, want 3 (city + 2 rigs)", len(spies.initCalls))
	}
	wantDirs := []string{cityPath, filepath.Join(cityPath, "backend"), filepath.Join(cityPath, "worker")}
	for i, want := range wantDirs {
		if spies.initCalls[i].dir != want {
			t.Fatalf("init call %d dir = %s, want %s (city-first, then config order)", i, spies.initCalls[i].dir, want)
		}
	}
}

func TestBeadsMaterialize_AllAndRigAreMutuallyExclusive(t *testing.T) {
	writeMaterializeUnitCity(t, unitRig{name: "backend", prefix: "ba"})
	spies := installMaterializeSpies(t)

	if code := cmdBeadsMaterialize(beadsMaterializeOptions{all: true, rigs: []string{"backend"}}, os.Stderr, os.Stderr); code != 1 {
		t.Fatalf("cmdBeadsMaterialize(--all --rig) = %d, want 1", code)
	}
	if spies.ensureCount() != 0 {
		t.Fatalf("mutually-exclusive flag error must not start Dolt; ensure=%d", spies.ensureCount())
	}
}

func TestBeadsMaterialize_PartialFailureStopsAndExitsNonZero(t *testing.T) {
	writeMaterializeUnitCity(t, unitRig{name: "backend", prefix: "ba"}, unitRig{name: "worker", prefix: "wo"})
	spies := installMaterializeSpies(t)
	// Fail the second scope (the "backend" rig); the city scope precedes it.
	spies.initErr = func(_ materializeInitCall, n int) error {
		if n == 2 {
			return fmt.Errorf("boom")
		}
		return nil
	}

	var stderr strings.Builder
	if code := cmdBeadsMaterialize(beadsMaterializeOptions{all: true}, os.Stderr, &stderr); code != 1 {
		t.Fatalf("cmdBeadsMaterialize(--all) with a mid-run failure = %d, want 1", code)
	}
	// The third scope ("worker") must NOT be attempted after the failure.
	if len(spies.initCalls) != 2 {
		t.Fatalf("initAndHookDir called %d times, want 2 (stop at first failure)", len(spies.initCalls))
	}
	if !strings.Contains(stderr.String(), "PARTIAL") {
		t.Fatalf("stderr must flag partial materialization, got:\n%s", stderr.String())
	}
}

func TestBeadsMaterialize_EnsureDoltFailureSkipsAllInit(t *testing.T) {
	writeMaterializeUnitCity(t)
	spies := installMaterializeSpies(t)
	spies.ensureErr = fmt.Errorf("no dolt")

	if code := cmdBeadsMaterialize(beadsMaterializeOptions{}, os.Stderr, os.Stderr); code != 1 {
		t.Fatalf("cmdBeadsMaterialize() with ensure failure = %d, want 1", code)
	}
	if len(spies.initCalls) != 0 {
		t.Fatalf("no scope may be initialized when the managed Dolt server cannot be brought up; init=%d", len(spies.initCalls))
	}
}

func TestBeadsMaterialize_RejectsNonBdCity(t *testing.T) {
	cityPath := t.TempDir()
	writeMaterializeUnitCityRaw(t, cityPath, "[workspace]\nname = \"filecity\"\n\n[beads]\nprovider = \"file\"\n")
	spies := installMaterializeSpies(t)

	if code := cmdBeadsMaterialize(beadsMaterializeOptions{}, os.Stderr, os.Stderr); code != 1 {
		t.Fatalf("cmdBeadsMaterialize() on a file-backed city = %d, want 1", code)
	}
	if spies.ensureCount() != 0 {
		t.Fatalf("a non-bd city must be rejected before any Dolt work; ensure=%d", spies.ensureCount())
	}
}

// TestBeadsMaterialize_HydrationFailureLeavesTrackedJSONLUntouched asserts the
// command's failure contract: when a scope's init (hydration) fails, the
// command exits non-zero and never itself removes the scope's tracked
// issues.jsonl. (The complementary Seam A guarantee — that initAndHookDir's own
// fail-closed path leaves the JSONL byte-identical on a real hydration error —
// is covered by the integration test and the existing Seam A guard tests.)
func TestBeadsMaterialize_HydrationFailureLeavesTrackedJSONLUntouched(t *testing.T) {
	cityPath := writeMaterializeUnitCity(t)
	jsonlPath := jsonlExportPath(cityPath)
	if err := os.MkdirAll(filepath.Dir(jsonlPath), 0o755); err != nil {
		t.Fatal(err)
	}
	content := []byte(`{"_type":"issue","id":"gc-1","title":"keep me"}` + "\n")
	if err := os.WriteFile(jsonlPath, content, 0o644); err != nil {
		t.Fatal(err)
	}
	gitTrack(t, cityPath, jsonlRelPath)

	spies := installMaterializeSpies(t)
	spies.initErr = func(_ materializeInitCall, _ int) error { return fmt.Errorf("hydration failed") }

	if code := cmdBeadsMaterialize(beadsMaterializeOptions{}, os.Stderr, os.Stderr); code != 1 {
		t.Fatalf("cmdBeadsMaterialize() with hydration failure = %d, want 1", code)
	}
	requireFileEquals(t, jsonlPath, content)
}

// TestBeadsMaterialize_MakesNoProviderReadinessCall pins the "no
// supervisor/session/provider" contract at the seam that would otherwise reach
// the AI-provider readiness probe: initProbeProvidersReadiness (api.ProbeProviders).
// A full materialize run must never trigger it.
func TestBeadsMaterialize_MakesNoProviderReadinessCall(t *testing.T) {
	writeMaterializeUnitCity(t, unitRig{name: "backend", prefix: "ba"})
	installMaterializeSpies(t)

	prevProbe := initProbeProvidersReadiness
	initProbeProvidersReadiness = func(_ context.Context, names []string, _ bool) (map[string]api.ReadinessItem, error) {
		t.Fatalf("materialize must not probe AI-provider readiness, but it probed %v", names)
		return nil, nil
	}
	t.Cleanup(func() { initProbeProvidersReadiness = prevProbe })

	if code := cmdBeadsMaterialize(beadsMaterializeOptions{all: true}, os.Stderr, os.Stderr); code != 0 {
		t.Fatalf("cmdBeadsMaterialize(--all) = %d, want 0", code)
	}
}
