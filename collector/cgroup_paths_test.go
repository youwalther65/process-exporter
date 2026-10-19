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

// writePodTree builds a cgroupfs tree with two pod cgroups under one QoS
// parent, each carrying the memory.stat fields the swap experiment reads.
func writePodTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	pods := map[string]string{
		"kubepods.slice/kubepods-burstable.slice/kubepods-burstable-podaaa.slice": "4194304",
		"kubepods.slice/kubepods-burstable.slice/kubepods-burstable-podbbb.slice": "8388608",
	}
	for dir, file := range pods {
		full := filepath.Join(root, dir)
		if err := os.MkdirAll(full, 0o755); err != nil {
			t.Fatal(err)
		}
		stat := "anon 1024\nfile " + file + "\nworkingset_refault_file 77\n"
		if err := os.WriteFile(filepath.Join(full, "memory.stat"), []byte(stat), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func newPathCollector(t *testing.T, root string, cfg config.CgroupConfig) *testCgroupCollector {
	t.Helper()
	cc, err := newCgroupCollector(CgroupCollectorOption{
		CgroupFSPath: root,
		Memory:       true,
		Config:       cfg,
	})
	if err != nil {
		t.Fatalf("newCgroupCollector: %v", err)
	}
	if cc == nil {
		t.Fatal("expected non-nil collector when Memory enabled")
	}
	return &testCgroupCollector{cc: cc, groups: proc.GroupByName{}}
}

// A wildcard pattern produces one series per pod cgroup, labelled by path. This
// is the measurement the per-group collector cannot make, because the exporter
// has no process group whose procs all live in one pod cgroup.
func TestCgroupPathsWildcard(t *testing.T) {
	c := newPathCollector(t, writePodTree(t), config.CgroupConfig{
		Paths:            []string{"/kubepods.slice/*/*"},
		MemoryStatFields: []string{"file", "workingset_refault_file"},
	})

	expected := `
# HELP namedprocess_cgroup_memory_stat_bytes Selected memory.stat fields for this cgroup.
# TYPE namedprocess_cgroup_memory_stat_bytes gauge
namedprocess_cgroup_memory_stat_bytes{cgroup="/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-podaaa.slice",field="file"} 4194304
namedprocess_cgroup_memory_stat_bytes{cgroup="/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-podaaa.slice",field="workingset_refault_file"} 77
namedprocess_cgroup_memory_stat_bytes{cgroup="/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-podbbb.slice",field="file"} 8388608
namedprocess_cgroup_memory_stat_bytes{cgroup="/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-podbbb.slice",field="workingset_refault_file"} 77
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected),
		"namedprocess_cgroup_memory_stat_bytes"); err != nil {
		t.Error(err)
	}
}

// A pattern over max_paths contributes no series at all and bumps the dropped
// counter, so a missing cgroup is visible in the metrics rather than silent.
func TestCgroupPathsOverLimitDropped(t *testing.T) {
	cfg := config.CgroupConfig{
		Paths:            []string{"/kubepods.slice/*/*"},
		MaxPaths:         1,
		MemoryStatFields: []string{"file"},
	}
	root := writePodTree(t)

	if n := testutil.CollectAndCount(newPathCollector(t, root, cfg),
		"namedprocess_cgroup_memory_stat_bytes"); n != 0 {
		t.Errorf("got %d series, want 0 when the pattern is dropped", n)
	}

	// A fresh collector, because the counter is per process lifetime and
	// collecting twice would count the drop twice.
	c := newPathCollector(t, root, cfg)
	expected := `
# HELP namedprocess_scrape_cgroup_pattern_dropped incremented each time a configured cgroup path pattern is dropped, because it matched more cgroups than max_paths or could not be expanded
# TYPE namedprocess_scrape_cgroup_pattern_dropped counter
namedprocess_scrape_cgroup_pattern_dropped 1
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected),
		"namedprocess_scrape_cgroup_pattern_dropped"); err != nil {
		t.Error(err)
	}
}

// With no paths configured the path metrics are absent, so the feature cannot
// change what an existing deployment exports.
func TestCgroupPathsAbsentByDefault(t *testing.T) {
	c := newPathCollector(t, writePodTree(t), config.CgroupConfig{
		MemoryStatFields: []string{"file"},
	})

	for _, name := range []string{
		"namedprocess_cgroup_memory_stat_bytes",
		"namedprocess_cgroup_memory_current_bytes",
	} {
		if n := testutil.CollectAndCount(c, name); n != 0 {
			t.Errorf("%s: got %d series, want 0 with no paths configured", name, n)
		}
	}
	if n := testutil.CollectAndCount(c, "namedprocess_scrape_cgroup_pattern_dropped"); n != 1 {
		t.Errorf("dropped counter: got %d series, want 1 (value 0)", n)
	}
}

// The path and group metrics are separate families, so a cgroup read both ways
// produces two series that do not overwrite one another.
func TestCgroupPathsAndGroupsCoexist(t *testing.T) {
	root := t.TempDir()
	unit := filepath.Join(root, "runtime.slice", "containerd.service")
	if err := os.MkdirAll(unit, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(unit, "memory.current"), []byte("2048\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cc, err := newCgroupCollector(CgroupCollectorOption{
		CgroupFSPath: root,
		Memory:       true,
		Config:       config.CgroupConfig{Paths: []string{"/runtime.slice/containerd.service"}},
	})
	if err != nil {
		t.Fatalf("newCgroupCollector: %v", err)
	}
	c := &testCgroupCollector{cc: cc, groups: proc.GroupByName{
		"containerd": {CgroupV2Path: "/runtime.slice/containerd.service"},
	}}

	expected := `
# HELP namedprocess_cgroup_memory_current_bytes Total memory currently in use by this cgroup (memory.current).
# TYPE namedprocess_cgroup_memory_current_bytes gauge
namedprocess_cgroup_memory_current_bytes{cgroup="/runtime.slice/containerd.service"} 2048
# HELP namedprocess_namegroup_cgroup_memory_current_bytes Total memory currently in use by this group's cgroup (memory.current).
# TYPE namedprocess_namegroup_cgroup_memory_current_bytes gauge
namedprocess_namegroup_cgroup_memory_current_bytes{groupname="containerd"} 2048
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected),
		"namedprocess_cgroup_memory_current_bytes",
		"namedprocess_namegroup_cgroup_memory_current_bytes"); err != nil {
		t.Error(err)
	}
}

// Every descriptor is unique across the two sets; a duplicate would make the
// collector unregisterable.
func TestCgroupDescriptorsUnique(t *testing.T) {
	cc, err := newCgroupCollector(CgroupCollectorOption{
		CgroupFSPath: t.TempDir(),
		Memory:       true,
		PSI:          true,
		CPU:          true,
		Pids:         true,
		IO:           true,
	})
	if err != nil {
		t.Fatalf("newCgroupCollector: %v", err)
	}

	ch := make(chan *prometheus.Desc, 64)
	go func() {
		cc.describe(ch)
		close(ch)
	}()
	seen := map[string]bool{}
	for d := range ch {
		if seen[d.String()] {
			t.Errorf("duplicate descriptor: %s", d)
		}
		seen[d.String()] = true
	}
	if len(seen) == 0 {
		t.Fatal("describe emitted nothing")
	}
}
