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

	tc := newTestCollector(t, root, config.CgroupConfig{PSIWindows: []string{"total", "avg10"}}, proc.GroupByName{
		"nma": proc.Group{CgroupV2Path: path},
	})

	const want = `
# HELP namedprocess_namegroup_cgroup_pressure_ratio Kernel-computed PSI average (percent stalled) over the labelled window.
# TYPE namedprocess_namegroup_cgroup_pressure_ratio gauge
namedprocess_namegroup_cgroup_pressure_ratio{groupname="nma",kind="some",resource="memory",window="avg10"} 1
namedprocess_namegroup_cgroup_pressure_ratio{groupname="nma",kind="some",resource="memory",window="avg300"} 3
namedprocess_namegroup_cgroup_pressure_ratio{groupname="nma",kind="some",resource="memory",window="avg60"} 2
`
	if err := testutil.CollectAndCompare(tc, strings.NewReader(want),
		"namedprocess_namegroup_cgroup_pressure_ratio",
	); err != nil {
		t.Error(err)
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
