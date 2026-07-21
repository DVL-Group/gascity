package dashboardbff

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHealthSystemReturnsUnavailableWhenSamplingFails(t *testing.T) {
	p := New(Deps{})
	p.healthSnapshot = func(context.Context) (systemHealth, error) {
		return systemHealth{}, errors.New("host metrics unavailable")
	}

	rec := httptest.NewRecorder()
	p.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/health/system", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
	var got apiErrorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode error response: %v", err)
	}
	if got.Error != "system health unavailable" {
		t.Errorf("error = %q, want %q", got.Error, "system health unavailable")
	}
}

// TestLocalToolVersionsMemoized verifies the MEDIUM finding fix: repeated calls
// within the TTL reuse the cached snapshot instead of re-probing. The cache is
// seeded by a real probe, then overwritten with a sentinel and given a future
// expiry; the next call must return the sentinel (proving no re-probe), and a
// past expiry must force a re-probe (sentinel replaced).
func TestLocalToolVersionsMemoized(t *testing.T) {
	p := New(Deps{})
	ctx := context.Background()

	_ = p.localToolVersions(ctx) // prime the cache
	c := p.localTools

	sentinel := localToolVersions{Dolt: localToolVersion{Status: "available", Version: "sentinel"}}
	c.mu.Lock()
	c.val = sentinel
	c.expires = time.Now().Add(time.Hour)
	c.mu.Unlock()

	if got := p.localToolVersions(ctx); got.Dolt.Version != "sentinel" {
		t.Errorf("cached call re-probed: dolt version = %q, want sentinel", got.Dolt.Version)
	}

	// Expire the entry: the next call must re-probe and overwrite the sentinel.
	c.mu.Lock()
	c.expires = time.Now().Add(-time.Minute)
	c.mu.Unlock()
	if got := p.localToolVersions(ctx); got.Dolt.Version == "sentinel" {
		t.Error("expired cache was not re-probed: still returning sentinel")
	}
}

// TestLocalToolsCachePerPlane confirms each Plane gets its own cache entry, so
// one Plane's snapshot never leaks into another's.
func TestLocalToolsCachePerPlane(t *testing.T) {
	p1, p2 := New(Deps{}), New(Deps{})
	if p1.localTools == p2.localTools {
		t.Error("distinct planes share a localToolsCache")
	}
	if p1.localTools == nil {
		t.Error("plane localTools cache not initialized")
	}
}

// TestUnavailableSanitizesReason verifies the NIT fix: subprocess/error text in
// an unavailable reason is run through sanitizeTerminalOutput before it reaches
// the wire, stripping control and escape bytes.
func TestUnavailableSanitizesReason(t *testing.T) {
	tv := unavailable("boom\x07 \x1b]0;title\x07here\x00")
	if tv.Status != "unavailable" {
		t.Fatalf("status = %q, want unavailable", tv.Status)
	}
	if strings.ContainsAny(tv.Reason, "\x07\x00\x1b") {
		t.Errorf("reason not sanitized: %q", tv.Reason)
	}
	if !strings.Contains(tv.Reason, "boom") || !strings.Contains(tv.Reason, "here") {
		t.Errorf("sanitizer dropped legible text: %q", tv.Reason)
	}
}
