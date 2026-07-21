package dashboardbff

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/shirou/gopsutil/v4/host"
	"github.com/shirou/gopsutil/v4/load"
	"github.com/shirou/gopsutil/v4/mem"
	"github.com/shirou/gopsutil/v4/process"
)

// processStart is captured once at package init and used to derive the admin
// process uptime, the Go equivalent of Node's process.uptime().
var processStart = time.Now()

// adminHealth is the dashboard backend process state, matching the admin block
// of shared/src/dashboard-health.ts SystemHealth. node_version carries the Go
// runtime version (this backend is the Go port of the former Node BFF).
type adminHealth struct {
	Pid           int    `json:"pid"`
	UptimeSec     int64  `json:"uptime_sec"`
	RssBytes      int64  `json:"rss_bytes"`
	HeapUsedBytes int64  `json:"heap_used_bytes"`
	NodeVersion   string `json:"node_version"`
}

// hostHealth is the machine-level state, matching the host block of
// shared/src/dashboard-health.ts SystemHealth.
type hostHealth struct {
	LoadAvg1      float64 `json:"load_avg_1"`
	LoadAvg5      float64 `json:"load_avg_5"`
	LoadAvg15     float64 `json:"load_avg_15"`
	TotalMemBytes int64   `json:"total_mem_bytes"`
	FreeMemBytes  int64   `json:"free_mem_bytes"`
	CPUCount      int     `json:"cpu_count"`
	UptimeSec     int64   `json:"uptime_sec"`
}

// systemHealth is the GET /api/health/system response, matching
// shared/src/dashboard-health.ts SystemHealth.
type systemHealth struct {
	Admin adminHealth `json:"admin"`
	Host  hostHealth  `json:"host"`
}

// localToolVersion is one probed tool's status, matching the union in
// shared/src/dashboard-health.ts LocalToolVersion. On success only
// {status,version,source} is emitted; on failure only {status,reason}. The
// unused arm's fields are omitted so the wire shape matches the TS union exactly.
type localToolVersion struct {
	Status  string `json:"status"`
	Version string `json:"version,omitempty"`
	Source  string `json:"source,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

// localToolVersions is the GET /api/health/local-tools response, matching
// shared/src/dashboard-health.ts LocalToolVersions.
type localToolVersions struct {
	Dolt  localToolVersion `json:"dolt"`
	Beads localToolVersion `json:"beads"`
	GC    localToolVersion `json:"gc"`
}

// versionProbeTimeout bounds each local tool version probe.
const versionProbeTimeout = 5 * time.Second

// localToolsTTL is how long a probed LocalToolVersions snapshot is reused
// before the next request re-probes. Tool versions only change at deploy
// cadence, so a short TTL keeps GET /api/health/local-tools from forking three
// subprocesses on every poll.
const localToolsTTL = 45 * time.Second

// semverRE extracts a dotted semver token from version output (SEMVER_RE in
// version-probe.ts).
var semverRE = regexp.MustCompile(`(\d+\.\d+\.\d+)`)

// registerHealth wires GET /api/health/system and GET /api/health/local-tools
// onto the plane mux.
func (p *Plane) registerHealth() {
	p.mux.HandleFunc("GET /api/health/system", func(w http.ResponseWriter, r *http.Request) {
		snapshot, err := p.healthSnapshot(r.Context())
		if err != nil {
			log.Printf("dashboard system-health sample failed: %v", err)
			writeError(w, http.StatusServiceUnavailable, "system health unavailable")
			return
		}
		writeJSON(w, http.StatusOK, snapshot)
	})
	p.mux.HandleFunc("GET /api/health/local-tools", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, p.localToolVersions(r.Context()))
	})
}

// currentSystemHealth assembles the admin and host health blocks using
// cross-platform OS samplers. A failed or impossible essential metric fails the
// snapshot so callers report it as unavailable instead of fabricating zeroes.
func currentSystemHealth(ctx context.Context) (systemHealth, error) {
	// On Windows, the first gopsutil Avg call starts a process-global sampler
	// under the supplied context. A request context would stop that singleton
	// permanently when the request ends, so always give it process lifetime.
	return currentSystemHealthWithLoadSampler(ctx, load.Avg)
}

func currentSystemHealthWithLoadSampler(
	ctx context.Context,
	loadAverage func() (*load.AvgStat, error),
) (systemHealth, error) {
	var runtimeMem runtime.MemStats
	runtime.ReadMemStats(&runtimeMem)
	pid := os.Getpid()
	const maxProcessPID = 1<<31 - 1
	if pid < 0 || pid > maxProcessPID {
		return systemHealth{}, fmt.Errorf("opening dashboard process metrics: invalid PID %d", pid)
	}
	cpuCount := runtime.NumCPU()
	if cpuCount <= 0 {
		return systemHealth{}, fmt.Errorf("reading host CPU count: invalid value %d", cpuCount)
	}

	avg, err := loadAverage()
	if err != nil {
		return systemHealth{}, fmt.Errorf("reading host load average: %w", err)
	}
	if !validLoadAverage(avg.Load1) || !validLoadAverage(avg.Load5) || !validLoadAverage(avg.Load15) {
		return systemHealth{}, fmt.Errorf("reading host load average: invalid values %.2f, %.2f, %.2f", avg.Load1, avg.Load5, avg.Load15)
	}

	virtualMemory, err := mem.VirtualMemoryWithContext(ctx)
	if err != nil {
		return systemHealth{}, fmt.Errorf("reading host memory: %w", err)
	}
	if virtualMemory.Total == 0 || virtualMemory.Available > virtualMemory.Total {
		return systemHealth{}, fmt.Errorf("reading host memory: invalid total/available values %d/%d", virtualMemory.Total, virtualMemory.Available)
	}

	uptime, err := host.UptimeWithContext(ctx)
	if err != nil {
		return systemHealth{}, fmt.Errorf("reading host uptime: %w", err)
	}
	if uptime == 0 {
		return systemHealth{}, fmt.Errorf("reading host uptime: invalid value %d", uptime)
	}

	self, err := process.NewProcessWithContext(ctx, int32(pid))
	if err != nil {
		return systemHealth{}, fmt.Errorf("opening dashboard process metrics: %w", err)
	}
	processMemory, err := self.MemoryInfoWithContext(ctx)
	if err != nil {
		return systemHealth{}, fmt.Errorf("reading dashboard process memory: %w", err)
	}
	if processMemory.RSS == 0 {
		return systemHealth{}, fmt.Errorf("reading dashboard process memory: invalid RSS %d", processMemory.RSS)
	}

	totalMemory, err := metricInt64("host total memory", virtualMemory.Total)
	if err != nil {
		return systemHealth{}, err
	}
	availableMemory, err := metricInt64("host available memory", virtualMemory.Available)
	if err != nil {
		return systemHealth{}, err
	}
	rss, err := metricInt64("dashboard process RSS", processMemory.RSS)
	if err != nil {
		return systemHealth{}, err
	}
	hostUptime, err := metricInt64("host uptime", uptime)
	if err != nil {
		return systemHealth{}, err
	}

	return systemHealth{
		Admin: adminHealth{
			Pid:           pid,
			UptimeSec:     int64(time.Since(processStart).Round(time.Second).Seconds()),
			RssBytes:      rss,
			HeapUsedBytes: int64(runtimeMem.HeapAlloc),
			NodeVersion:   runtime.Version(),
		},
		Host: hostHealth{
			LoadAvg1:      avg.Load1,
			LoadAvg5:      avg.Load5,
			LoadAvg15:     avg.Load15,
			TotalMemBytes: totalMemory,
			FreeMemBytes:  availableMemory,
			CPUCount:      cpuCount,
			UptimeSec:     hostUptime,
		},
	}, nil
}

func validLoadAverage(value float64) bool {
	return value >= 0 && !math.IsNaN(value) && !math.IsInf(value, 0)
}

func metricInt64(name string, value uint64) (int64, error) {
	const maxInt64 = uint64(1<<63 - 1)
	if value > maxInt64 {
		return 0, fmt.Errorf("reading %s: value %d overflows int64", name, value)
	}
	return int64(value), nil
}

// localToolsCache memoizes one Plane's LocalToolVersions snapshot behind a
// mutex and a TTL. The mutex also collapses concurrent refreshes so a burst of
// GETs after expiry forks one set of probes, not one set per request.
type localToolsCache struct {
	mu      sync.Mutex
	val     localToolVersions
	expires time.Time
	primed  bool
}

// localToolVersions returns the memoized tool-version snapshot, re-probing only
// when the cached value is missing or older than localToolsTTL. Repeated GETs
// within the TTL reuse the snapshot instead of forking three subprocesses each.
// The cache lives on the Plane (one per process); its mutex collapses
// concurrent refreshes so a burst of GETs after expiry forks one set of probes.
func (p *Plane) localToolVersions(ctx context.Context) localToolVersions {
	c := p.localTools
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.primed && time.Now().Before(c.expires) {
		return c.val
	}
	c.val = p.probeLocalTools(ctx)
	c.expires = time.Now().Add(localToolsTTL)
	c.primed = true
	return c.val
}

// probeLocalTools probes the dolt, beads, and gc binaries concurrently through
// the shared exec runner (so each probe obeys the concurrency semaphore, clean
// environment, and timeout). Each result is either {status:available,version,
// source} or {status:unavailable,reason}; a probe never fabricates a version.
func (p *Plane) probeLocalTools(ctx context.Context) localToolVersions {
	var (
		dolt, beads, gc localToolVersion
		done            = make(chan struct{}, 3)
	)
	go func() { dolt = p.probeSemverTool(ctx, "dolt", "version"); done <- struct{}{} }()
	go func() { beads = p.probeSemverTool(ctx, "bd", "version"); done <- struct{}{} }()
	go func() { gc = p.probeGCVersion(ctx); done <- struct{}{} }()
	for i := 0; i < 3; i++ {
		<-done
	}
	return localToolVersions{Dolt: dolt, Beads: beads, GC: gc}
}

// probeSemverTool runs "<cmd> <sub>" and extracts a semver token from stdout.
// source is the resolved binary path. A LookPath miss, exec failure, non-zero
// exit, or unrecognizable version surfaces as unavailable with a reason —
// never a fabricated version (probeVersion in version-probe.ts).
func (p *Plane) probeSemverTool(ctx context.Context, cmd, sub string) localToolVersion {
	path, err := exec.LookPath(cmd)
	if err != nil {
		return unavailable(cmd + " not found on PATH")
	}
	stdout, code, err := p.runProbe(ctx, cmd, sub)
	if err != nil {
		return unavailable(cmd + " " + sub + " probe failed: " + err.Error())
	}
	if code != 0 {
		return unavailable(cmd + " " + sub + " exited " + strconv.Itoa(code))
	}
	m := semverRE.FindStringSubmatch(stdout)
	if m == nil {
		return unavailable(cmd + " " + sub + " output had no recognizable version")
	}
	return localToolVersion{Status: "available", Version: m[1], Source: path}
}

// gcVersionJSON is the shape of `gc version --json` output we read from.
type gcVersionJSON struct {
	Version string `json:"version"`
}

// probeGCVersion runs `gc version --json` and reads the version field verbatim
// so a local `dev` build surfaces as "dev" rather than collapsing to "no
// recognizable version" (probeGcVersionJson in version-probe.ts).
func (p *Plane) probeGCVersion(ctx context.Context) localToolVersion {
	path, err := exec.LookPath("gc")
	if err != nil {
		return unavailable("gc not found on PATH")
	}
	stdout, code, err := p.runProbe(ctx, "gc", "version", "--json")
	if err != nil {
		return unavailable("gc version probe failed: " + err.Error())
	}
	if code != 0 {
		return unavailable("gc version exited " + strconv.Itoa(code))
	}
	var parsed gcVersionJSON
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &parsed); err != nil || parsed.Version == "" {
		return unavailable("gc version --json output had no version field")
	}
	return localToolVersion{Status: "available", Version: parsed.Version, Source: path}
}

// runProbe runs a short, shell-free version command through the shared exec
// runner so the probe obeys the maxConcurrent semaphore, the clean environment,
// and a bounded timeout. It returns stdout, the exit code, and a spawn/timeout
// error (a non-zero exit is reported in code, not as an error).
func (p *Plane) runProbe(ctx context.Context, cmd string, args ...string) (stdout string, code int, err error) {
	res, err := p.exec.run(ctx, cmd, args, versionProbeTimeout, maxBytes)
	if err != nil {
		return "", -1, err
	}
	return res.stdout, res.exitCode, nil
}

// unavailable builds an unavailable LocalToolVersion with the given reason. The
// reason forwards subprocess/error text, so it is sanitized before it reaches
// the browser, per the "all subprocess output is sanitized" contract.
func unavailable(reason string) localToolVersion {
	return localToolVersion{Status: "unavailable", Reason: sanitizeTerminalOutput(reason)}
}
