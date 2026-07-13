package collector

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ncabatoff/process-exporter/config"
	"github.com/ncabatoff/process-exporter/proc"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// testCgroupCollector adapts a cgroupCollector plus a fixed set of groups into a
// prometheus.Collector so we can exercise it with testutil.CollectAndCompare
// without needing a real /proc.
type testCgroupCollector struct {
	cc     *cgroupCollector
	groups proc.GroupByName
}

func (t *testCgroupCollector) Describe(ch chan<- *prometheus.Desc) { t.cc.describe(ch) }

func (t *testCgroupCollector) Collect(ch chan<- prometheus.Metric) {
	for gname, g := range t.groups {
		t.cc.collectGroup(ch, gname, g)
	}
	t.cc.collectSummary(ch)
}

// writeFixtureCgroupfs writes a memory.pressure file for one unit and returns
// the cgroupfs root.
func writeFixtureCgroupfs(t *testing.T, cgroupPath, memoryPressure string) string {
	t.Helper()
	root := t.TempDir()
	unit := filepath.Join(root, strings.Trim(cgroupPath, "/"))
	if err := os.MkdirAll(unit, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(unit, "memory.pressure"), []byte(memoryPressure), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func newTestCollector(t *testing.T, root string, cfg config.CgroupConfig, groups proc.GroupByName) *testCgroupCollector {
	t.Helper()
	cc, err := newCgroupCollector(CgroupCollectorOption{
		CgroupFSPath: root,
		PSI:          true,
		Config:       cfg,
	})
	if err != nil {
		t.Fatalf("newCgroupCollector: %v", err)
	}
	if cc == nil {
		t.Fatal("expected non-nil collector when PSI enabled")
	}
	return &testCgroupCollector{cc: cc, groups: groups}
}

func TestCgroupCollectorPSITotals(t *testing.T) {
	const path = "/runtime.slice/nma.service"
	// total in microseconds: 2_000_000us = 2s (some), 1_500_000us = 1.5s (full).
	root := writeFixtureCgroupfs(t, path,
		"some avg10=1.00 avg60=2.00 avg300=3.00 total=2000000\n"+
			"full avg10=0.50 avg60=0.60 avg300=0.70 total=1500000\n")

	tc := newTestCollector(t, root, config.CgroupConfig{}, proc.GroupByName{
		"nma": proc.Group{CgroupV2Path: path},
	})

	// Default (no averages): only the *_seconds_total counters, in seconds.
	const want = `
# HELP namedprocess_namegroup_cgroup_pressure_memory_stalled_seconds_total Total time all procs in this group's cgroup were stalled on memory (PSI full).
# TYPE namedprocess_namegroup_cgroup_pressure_memory_stalled_seconds_total counter
namedprocess_namegroup_cgroup_pressure_memory_stalled_seconds_total{groupname="nma"} 1.5
# HELP namedprocess_namegroup_cgroup_pressure_memory_waiting_seconds_total Total time some proc in this group's cgroup was stalled on memory (PSI some).
# TYPE namedprocess_namegroup_cgroup_pressure_memory_waiting_seconds_total counter
namedprocess_namegroup_cgroup_pressure_memory_waiting_seconds_total{groupname="nma"} 2
`
	if err := testutil.CollectAndCompare(tc, strings.NewReader(want),
		"namedprocess_namegroup_cgroup_pressure_memory_waiting_seconds_total",
		"namedprocess_namegroup_cgroup_pressure_memory_stalled_seconds_total",
	); err != nil {
		t.Error(err)
	}
}

func TestCgroupCollectorAveragesOptIn(t *testing.T) {
	const path = "/runtime.slice/nma.service"
	root := writeFixtureCgroupfs(t, path,
		"some avg10=1.00 avg60=2.00 avg300=3.00 total=2000000\n")

	// windows names only avg10 (no "total"): exactly one percent series, and no
	// *_seconds_total counter. This is the per-window allowlist honored.
	tc := newTestCollector(t, root, config.CgroupConfig{PSIWindows: []string{"avg10"}}, proc.GroupByName{
		"nma": proc.Group{CgroupV2Path: path},
	})

	const want = `
# HELP namedprocess_namegroup_cgroup_pressure_percent Kernel-computed PSI average, percent of wall time stalled (0-100), over the labelled window.
# TYPE namedprocess_namegroup_cgroup_pressure_percent gauge
namedprocess_namegroup_cgroup_pressure_percent{groupname="nma",kind="some",resource="memory",window="avg10"} 1
`
	if err := testutil.CollectAndCompare(tc, strings.NewReader(want),
		"namedprocess_namegroup_cgroup_pressure_percent",
	); err != nil {
		t.Error(err)
	}

	// The counter must NOT be emitted when "total" is absent from windows.
	if n := testutil.CollectAndCount(tc,
		"namedprocess_namegroup_cgroup_pressure_memory_waiting_seconds_total"); n != 0 {
		t.Errorf("expected no memory-waiting counter when 'total' not in windows, got %d", n)
	}
}

// TestCgroupCollectorWindowAllowlist verifies each window is gated
// independently: [total, avg60] emits the counter and only the avg60 percent.
func TestCgroupCollectorWindowAllowlist(t *testing.T) {
	const path = "/runtime.slice/nma.service"
	root := writeFixtureCgroupfs(t, path,
		"some avg10=1.00 avg60=2.00 avg300=3.00 total=2000000\n")

	tc := newTestCollector(t, root, config.CgroupConfig{PSIWindows: []string{"total", "avg60"}}, proc.GroupByName{
		"nma": proc.Group{CgroupV2Path: path},
	})

	const want = `
# HELP namedprocess_namegroup_cgroup_pressure_percent Kernel-computed PSI average, percent of wall time stalled (0-100), over the labelled window.
# TYPE namedprocess_namegroup_cgroup_pressure_percent gauge
namedprocess_namegroup_cgroup_pressure_percent{groupname="nma",kind="some",resource="memory",window="avg60"} 2
`
	if err := testutil.CollectAndCompare(tc, strings.NewReader(want),
		"namedprocess_namegroup_cgroup_pressure_percent",
	); err != nil {
		t.Error(err)
	}
	// avg10 and avg300 must be absent; the counter must be present (total set).
	if n := testutil.CollectAndCount(tc, "namedprocess_namegroup_cgroup_pressure_percent"); n != 1 {
		t.Errorf("expected exactly 1 percent series (avg60 only), got %d", n)
	}
	if n := testutil.CollectAndCount(tc,
		"namedprocess_namegroup_cgroup_pressure_memory_waiting_seconds_total"); n != 1 {
		t.Errorf("expected the memory-waiting counter (total in windows), got %d", n)
	}
}

// TestCgroupCollectorRootPathSkipped verifies a group resolving to the root
// cgroup ("/") emits nothing rather than mislabeling host-root metrics.
func TestCgroupCollectorRootPathSkipped(t *testing.T) {
	// Fixture has a real unit, but the group points at "/", so the root file
	// (if any) must never be read/emitted for this group.
	root := writeFixtureCgroupfs(t, "/runtime.slice/nma.service",
		"some avg10=1.00 avg60=2.00 avg300=3.00 total=2000000\n")

	tc := newTestCollector(t, root, config.CgroupConfig{}, proc.GroupByName{
		"rootish": proc.Group{CgroupV2Path: "/"},
	})

	if n := testutil.CollectAndCount(tc,
		"namedprocess_namegroup_cgroup_pressure_memory_waiting_seconds_total"); n != 0 {
		t.Errorf("expected no series for a group resolving to root cgroup, got %d", n)
	}
}

func TestCgroupCollectorSkipAndCount(t *testing.T) {
	const path = "/runtime.slice/nma.service"
	root := writeFixtureCgroupfs(t, path, "some avg10=0 avg60=0 avg300=0 total=1000000\n")

	// One clean group and one conflicting group (procs span multiple cgroups).
	tc := newTestCollector(t, root, config.CgroupConfig{}, proc.GroupByName{
		"nma":      proc.Group{CgroupV2Path: path},
		"confused": proc.Group{CgroupV2Path: path, CgroupV2Conflict: true},
	})

	const want = `
# HELP namedprocess_scrape_cgroup_skipped incremented each time a group's cgroup metrics are skipped because its procs span multiple cgroups
# TYPE namedprocess_scrape_cgroup_skipped counter
namedprocess_scrape_cgroup_skipped 1
`
	if err := testutil.CollectAndCompare(tc, strings.NewReader(want),
		"namedprocess_scrape_cgroup_skipped",
	); err != nil {
		t.Error(err)
	}

	// The conflicting group must emit no PSI metrics; only the clean group does.
	n := testutil.CollectAndCount(tc, "namedprocess_namegroup_cgroup_pressure_memory_waiting_seconds_total")
	if n != 1 {
		t.Errorf("expected exactly 1 memory-waiting series (clean group only), got %d", n)
	}
}

func TestNewCgroupCollectorDisabled(t *testing.T) {
	cc, err := newCgroupCollector(CgroupCollectorOption{PSI: false})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cc != nil {
		t.Error("expected nil collector when no family enabled")
	}
}

// writeStatFixture writes memory.current, memory.stat, cpu.stat and
// pids.current for one unit and returns the cgroupfs root.
func writeStatFixture(t *testing.T, cgroupPath string) string {
	t.Helper()
	root := t.TempDir()
	unit := filepath.Join(root, strings.Trim(cgroupPath, "/"))
	if err := os.MkdirAll(unit, 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"memory.current":      "104857600\n",
		"memory.swap.current": "20971520\n",
		"memory.stat":         "anon 1048576\nfile 2097152\nkernel 524288\nslab 262144\nsock 131072\nunused_field 999\n",
		"cpu.stat":            "usage_usec 9000000\nuser_usec 6000000\nsystem_usec 3000000\nnr_periods 0\n",
		"pids.current":        "17\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(unit, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestCgroupCollectorMemory(t *testing.T) {
	const path = "/runtime.slice/nma.service"
	root := writeStatFixture(t, path)

	cc, err := newCgroupCollector(CgroupCollectorOption{CgroupFSPath: root, Memory: true})
	if err != nil {
		t.Fatalf("newCgroupCollector: %v", err)
	}
	tc := &testCgroupCollector{cc: cc, groups: proc.GroupByName{"nma": proc.Group{CgroupV2Path: path}}}

	// memory.current, memory.swap.current, plus the curated default stat fields
	// (anon/file/kernel/slab/sock). unused_field is present in the file but not
	// allowlisted, so must not appear.
	const want = `
# HELP namedprocess_namegroup_cgroup_memory_current_bytes Total memory currently in use by this group's cgroup (memory.current).
# TYPE namedprocess_namegroup_cgroup_memory_current_bytes gauge
namedprocess_namegroup_cgroup_memory_current_bytes{groupname="nma"} 1.048576e+08
# HELP namedprocess_namegroup_cgroup_memory_swap_current_bytes Swap currently in use by this group's cgroup (memory.swap.current); the whole-cgroup analog of per-process memory_bytes{memtype="swapped"}.
# TYPE namedprocess_namegroup_cgroup_memory_swap_current_bytes gauge
namedprocess_namegroup_cgroup_memory_swap_current_bytes{groupname="nma"} 2.097152e+07
# HELP namedprocess_namegroup_cgroup_memory_stat_bytes Selected memory.stat fields for this group's cgroup.
# TYPE namedprocess_namegroup_cgroup_memory_stat_bytes gauge
namedprocess_namegroup_cgroup_memory_stat_bytes{field="anon",groupname="nma"} 1.048576e+06
namedprocess_namegroup_cgroup_memory_stat_bytes{field="file",groupname="nma"} 2.097152e+06
namedprocess_namegroup_cgroup_memory_stat_bytes{field="kernel",groupname="nma"} 524288
namedprocess_namegroup_cgroup_memory_stat_bytes{field="slab",groupname="nma"} 262144
namedprocess_namegroup_cgroup_memory_stat_bytes{field="sock",groupname="nma"} 131072
`
	if err := testutil.CollectAndCompare(tc, strings.NewReader(want),
		"namedprocess_namegroup_cgroup_memory_current_bytes",
		"namedprocess_namegroup_cgroup_memory_swap_current_bytes",
		"namedprocess_namegroup_cgroup_memory_stat_bytes",
	); err != nil {
		t.Error(err)
	}
}

// TestCgroupCollectorMemorySwapAbsent verifies the swap metric is simply omitted
// (no panic, no zero) when memory.swap.current is missing — e.g. swap accounting
// disabled — while memory.current is still emitted.
func TestCgroupCollectorMemorySwapAbsent(t *testing.T) {
	const path = "/runtime.slice/nma.service"
	// Fixture with memory.current but no memory.swap.current.
	root := t.TempDir()
	unit := filepath.Join(root, strings.Trim(path, "/"))
	if err := os.MkdirAll(unit, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(unit, "memory.current"), []byte("104857600\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cc, err := newCgroupCollector(CgroupCollectorOption{CgroupFSPath: root, Memory: true})
	if err != nil {
		t.Fatalf("newCgroupCollector: %v", err)
	}
	tc := &testCgroupCollector{cc: cc, groups: proc.GroupByName{"nma": proc.Group{CgroupV2Path: path}}}

	if n := testutil.CollectAndCount(tc, "namedprocess_namegroup_cgroup_memory_swap_current_bytes"); n != 0 {
		t.Errorf("expected no swap series when memory.swap.current absent, got %d", n)
	}
	if n := testutil.CollectAndCount(tc, "namedprocess_namegroup_cgroup_memory_current_bytes"); n != 1 {
		t.Errorf("expected memory.current still emitted, got %d", n)
	}
}

func TestCgroupCollectorCPUAndPids(t *testing.T) {
	const path = "/runtime.slice/nma.service"
	root := writeStatFixture(t, path)

	cc, err := newCgroupCollector(CgroupCollectorOption{CgroupFSPath: root, CPU: true, Pids: true})
	if err != nil {
		t.Fatalf("newCgroupCollector: %v", err)
	}
	tc := &testCgroupCollector{cc: cc, groups: proc.GroupByName{"nma": proc.Group{CgroupV2Path: path}}}

	// user_usec 6000000 -> 6s, system_usec 3000000 -> 3s; pids.current 17.
	const want = `
# HELP namedprocess_namegroup_cgroup_cpu_seconds_total CPU time consumed by this group's cgroup (cpu.stat), by mode.
# TYPE namedprocess_namegroup_cgroup_cpu_seconds_total counter
namedprocess_namegroup_cgroup_cpu_seconds_total{groupname="nma",mode="system"} 3
namedprocess_namegroup_cgroup_cpu_seconds_total{groupname="nma",mode="user"} 6
# HELP namedprocess_namegroup_cgroup_pids_current Number of processes/threads currently in this group's cgroup (pids.current).
# TYPE namedprocess_namegroup_cgroup_pids_current gauge
namedprocess_namegroup_cgroup_pids_current{groupname="nma"} 17
`
	if err := testutil.CollectAndCompare(tc, strings.NewReader(want),
		"namedprocess_namegroup_cgroup_cpu_seconds_total",
		"namedprocess_namegroup_cgroup_pids_current",
	); err != nil {
		t.Error(err)
	}
}

func TestCgroupCollectorMemoryStatAllowlist(t *testing.T) {
	const path = "/runtime.slice/nma.service"
	root := writeStatFixture(t, path)

	// Config names only "anon" -> only that field emitted.
	cc, err := newCgroupCollector(CgroupCollectorOption{
		CgroupFSPath: root, Memory: true,
		Config: config.CgroupConfig{MemoryStatFields: []string{"anon"}},
	})
	if err != nil {
		t.Fatalf("newCgroupCollector: %v", err)
	}
	tc := &testCgroupCollector{cc: cc, groups: proc.GroupByName{"nma": proc.Group{CgroupV2Path: path}}}

	n := testutil.CollectAndCount(tc, "namedprocess_namegroup_cgroup_memory_stat_bytes")
	if n != 1 {
		t.Errorf("expected exactly 1 memory.stat series (anon only), got %d", n)
	}
}
