package collector

import (
	"log"

	"github.com/ncabatoff/process-exporter/cgroup"
	"github.com/ncabatoff/process-exporter/config"
	"github.com/ncabatoff/process-exporter/proc"
	"github.com/prometheus/client_golang/prometheus"
)

// microsecondsPerSecond converts PSI totals (reported in microseconds) to the
// seconds used by the exported *_seconds_total counters.
const microsecondsPerSecond = 1e6

// CgroupCollectorOption configures the cgroupv2 collector. The metric families
// are enabled independently (via CLI flags upstream); Config carries the
// field-level allowlists that bound cardinality.
type CgroupCollectorOption struct {
	// CgroupFSPath is the cgroupv2 mount point, e.g. /sys/fs/cgroup.
	CgroupFSPath string
	// PSI enables the pressure-stall metrics.
	PSI bool
	// Memory enables memory.current and memory.stat (allowlisted fields).
	Memory bool
	// CPU enables cpu.stat (user/system seconds).
	CPU bool
	// Pids enables pids.current.
	Pids bool
	// Config carries the field allowlists / PSI window selection.
	Config config.CgroupConfig
	Debug  bool
}

// defaultMemoryStatFields is the curated subset of memory.stat emitted when the
// config names no explicit fields. Kept small deliberately: memory.stat has
// ~40 fields and emitting all of them is the biggest cardinality trap.
var defaultMemoryStatFields = []string{"anon", "file", "kernel", "slab", "sock"}

// PSI metric descriptors. Following node_exporter's vocabulary, the resource
// (cpu/memory/io) and the some/full distinction ("waiting"/"stalled") are
// encoded in the metric name rather than in labels. Only the total (as seconds)
// is emitted by default; the avg10/60/300 ratios are opt-in.
var (
	psiCPUWaitingDesc = prometheus.NewDesc(
		"namedprocess_namegroup_cgroup_pressure_cpu_waiting_seconds_total",
		"Total time procs in this group's cgroup were stalled waiting on CPU (PSI some).",
		[]string{"groupname"}, nil)

	psiMemoryWaitingDesc = prometheus.NewDesc(
		"namedprocess_namegroup_cgroup_pressure_memory_waiting_seconds_total",
		"Total time some proc in this group's cgroup was stalled on memory (PSI some).",
		[]string{"groupname"}, nil)

	psiMemoryStalledDesc = prometheus.NewDesc(
		"namedprocess_namegroup_cgroup_pressure_memory_stalled_seconds_total",
		"Total time all procs in this group's cgroup were stalled on memory (PSI full).",
		[]string{"groupname"}, nil)

	psiIOWaitingDesc = prometheus.NewDesc(
		"namedprocess_namegroup_cgroup_pressure_io_waiting_seconds_total",
		"Total time some proc in this group's cgroup was stalled on IO (PSI some).",
		[]string{"groupname"}, nil)

	psiIOStalledDesc = prometheus.NewDesc(
		"namedprocess_namegroup_cgroup_pressure_io_stalled_seconds_total",
		"Total time all procs in this group's cgroup were stalled on IO (PSI full).",
		[]string{"groupname"}, nil)

	// Opt-in PSI averages. The window (avg10/avg60/avg300) is a label.
	psiRatioDesc = prometheus.NewDesc(
		"namedprocess_namegroup_cgroup_pressure_ratio",
		"Kernel-computed PSI average (percent stalled) over the labelled window.",
		[]string{"groupname", "resource", "kind", "window"}, nil)

	cgroupSkippedDesc = prometheus.NewDesc(
		"namedprocess_scrape_cgroup_skipped",
		"incremented each time a group's cgroup metrics are skipped because its procs span multiple cgroups",
		nil, nil)

	memoryCurrentDesc = prometheus.NewDesc(
		"namedprocess_namegroup_cgroup_memory_current_bytes",
		"Total memory currently in use by this group's cgroup (memory.current).",
		[]string{"groupname"}, nil)

	memoryStatDesc = prometheus.NewDesc(
		"namedprocess_namegroup_cgroup_memory_stat_bytes",
		"Selected memory.stat fields for this group's cgroup.",
		[]string{"groupname", "field"}, nil)

	cgroupCPUDesc = prometheus.NewDesc(
		"namedprocess_namegroup_cgroup_cpu_seconds_total",
		"CPU time consumed by this group's cgroup (cpu.stat), by mode.",
		[]string{"groupname", "mode"}, nil)

	pidsCurrentDesc = prometheus.NewDesc(
		"namedprocess_namegroup_cgroup_pids_current",
		"Number of processes/threads currently in this group's cgroup (pids.current).",
		[]string{"groupname"}, nil)
)

// cgroupCollector reads cgroupv2 metrics for groups with a resolved single
// cgroup path and emits them. It is created only when at least one family is
// enabled; when nil, the exporter behaves exactly as upstream.
type cgroupCollector struct {
	reader        *cgroup.Reader
	psi           bool
	memory        bool
	cpu           bool
	pids          bool
	emitAverages  bool
	memStatFields []string
	debug         bool

	// skipped counts groups whose cgroup metrics were skipped this process
	// lifetime because their procs spanned multiple cgroups (skip-and-count).
	skipped float64
}

// newCgroupCollector opens the cgroupfs root and returns a collector for the
// enabled families. Returns nil (no error) if no family is enabled.
func newCgroupCollector(opt CgroupCollectorOption) (*cgroupCollector, error) {
	if !opt.PSI && !opt.Memory && !opt.CPU && !opt.Pids {
		return nil, nil
	}
	reader, err := cgroup.NewReader(opt.CgroupFSPath)
	if err != nil {
		return nil, err
	}
	memStatFields := opt.Config.MemoryStatFields
	if len(memStatFields) == 0 {
		memStatFields = defaultMemoryStatFields
	}
	return &cgroupCollector{
		reader:        reader,
		psi:           opt.PSI,
		memory:        opt.Memory,
		cpu:           opt.CPU,
		pids:          opt.Pids,
		emitAverages:  psiWindowsIncludeAverages(opt.Config.PSIWindows),
		memStatFields: memStatFields,
		debug:         opt.Debug,
	}, nil
}

// psiWindowsIncludeAverages reports whether the configured PSI windows request
// any of the avg10/60/300 averages (anything other than the default "total").
func psiWindowsIncludeAverages(windows []string) bool {
	for _, w := range windows {
		switch w {
		case "avg10", "avg60", "avg300":
			return true
		}
	}
	return false
}

func (c *cgroupCollector) describe(ch chan<- *prometheus.Desc) {
	ch <- psiCPUWaitingDesc
	ch <- psiMemoryWaitingDesc
	ch <- psiMemoryStalledDesc
	ch <- psiIOWaitingDesc
	ch <- psiIOStalledDesc
	ch <- psiRatioDesc
	ch <- cgroupSkippedDesc
	ch <- memoryCurrentDesc
	ch <- memoryStatDesc
	ch <- cgroupCPUDesc
	ch <- pidsCurrentDesc
}

// collectGroup emits cgroup metrics for a single named group. Per the
// skip-and-count policy, a group whose procs span multiple cgroups (or that has
// no resolved cgroup path) contributes no cgroup metrics; conflicts bump the
// skipped counter.
func (c *cgroupCollector) collectGroup(ch chan<- prometheus.Metric, gname string, group proc.Group) {
	if group.CgroupV2Conflict {
		c.skipped++
		if c.debug {
			log.Printf("skipping cgroup metrics for group %q: procs span multiple cgroups", gname)
		}
		return
	}
	if group.CgroupV2Path == "" {
		return
	}

	if c.psi {
		c.collectPSI(ch, gname, group.CgroupV2Path)
	}
	if c.memory {
		c.collectMemory(ch, gname, group.CgroupV2Path)
	}
	if c.cpu {
		c.collectCPU(ch, gname, group.CgroupV2Path)
	}
	if c.pids {
		c.collectPids(ch, gname, group.CgroupV2Path)
	}
}

// collectMemory emits memory.current and the allowlisted memory.stat fields.
func (c *cgroupCollector) collectMemory(ch chan<- prometheus.Metric, gname, cgroupPath string) {
	if cur, err := c.reader.ReadUint64(cgroupPath, "memory.current"); err != nil {
		c.debugf("group %q: reading memory.current: %v", gname, err)
	} else {
		ch <- prometheus.MustNewConstMetric(memoryCurrentDesc, prometheus.GaugeValue, float64(cur), gname)
	}

	stat, err := c.reader.ReadKeyed(cgroupPath, "memory.stat")
	if err != nil {
		c.debugf("group %q: reading memory.stat: %v", gname, err)
		return
	}
	for _, field := range c.memStatFields {
		// Only emit fields actually present, so a mistyped/absent field silently
		// produces no series rather than a zero.
		if v, ok := stat[field]; ok {
			ch <- prometheus.MustNewConstMetric(memoryStatDesc, prometheus.GaugeValue, float64(v), gname, field)
		}
	}
}

// collectCPU emits user/system CPU seconds from cpu.stat (reported in µs).
func (c *cgroupCollector) collectCPU(ch chan<- prometheus.Metric, gname, cgroupPath string) {
	stat, err := c.reader.ReadKeyed(cgroupPath, "cpu.stat")
	if err != nil {
		c.debugf("group %q: reading cpu.stat: %v", gname, err)
		return
	}
	if v, ok := stat["user_usec"]; ok {
		ch <- prometheus.MustNewConstMetric(cgroupCPUDesc, prometheus.CounterValue, float64(v)/microsecondsPerSecond, gname, "user")
	}
	if v, ok := stat["system_usec"]; ok {
		ch <- prometheus.MustNewConstMetric(cgroupCPUDesc, prometheus.CounterValue, float64(v)/microsecondsPerSecond, gname, "system")
	}
}

// collectPids emits pids.current.
func (c *cgroupCollector) collectPids(ch chan<- prometheus.Metric, gname, cgroupPath string) {
	cur, err := c.reader.ReadUint64(cgroupPath, "pids.current")
	if err != nil {
		c.debugf("group %q: reading pids.current: %v", gname, err)
		return
	}
	ch <- prometheus.MustNewConstMetric(pidsCurrentDesc, prometheus.GaugeValue, float64(cur), gname)
}

func (c *cgroupCollector) debugf(format string, args ...interface{}) {
	if c.debug {
		log.Printf(format, args...)
	}
}

// collectPSI reads the three pressure files for the group's cgroup and emits the
// configured metrics. A missing file (e.g. PSI disabled in the kernel) is
// silently skipped so the collector degrades gracefully.
func (c *cgroupCollector) collectPSI(ch chan<- prometheus.Metric, gname, cgroupPath string) {
	// resource -> (some desc, full desc). A nil full desc means the resource
	// has no meaningful "full" line to export (cpu).
	type psiTarget struct {
		resource string
		someDesc *prometheus.Desc
		fullDesc *prometheus.Desc
	}
	targets := []psiTarget{
		{"cpu", psiCPUWaitingDesc, nil},
		{"memory", psiMemoryWaitingDesc, psiMemoryStalledDesc},
		{"io", psiIOWaitingDesc, psiIOStalledDesc},
	}

	for _, tgt := range targets {
		stats, err := c.reader.ReadPSI(cgroupPath, tgt.resource)
		if err != nil {
			if c.debug {
				log.Printf("group %q: reading %s.pressure: %v", gname, tgt.resource, err)
			}
			continue
		}
		if stats.Some != nil {
			ch <- prometheus.MustNewConstMetric(tgt.someDesc,
				prometheus.CounterValue, float64(stats.Some.Total)/microsecondsPerSecond, gname)
			if c.emitAverages {
				c.emitRatios(ch, gname, tgt.resource, "some", *stats.Some)
			}
		}
		if stats.Full != nil && tgt.fullDesc != nil {
			ch <- prometheus.MustNewConstMetric(tgt.fullDesc,
				prometheus.CounterValue, float64(stats.Full.Total)/microsecondsPerSecond, gname)
			if c.emitAverages {
				c.emitRatios(ch, gname, tgt.resource, "full", *stats.Full)
			}
		}
	}
}

func (c *cgroupCollector) emitRatios(ch chan<- prometheus.Metric, gname, resource, kind string, line cgroup.PSILine) {
	ch <- prometheus.MustNewConstMetric(psiRatioDesc, prometheus.GaugeValue, line.Avg10, gname, resource, kind, "avg10")
	ch <- prometheus.MustNewConstMetric(psiRatioDesc, prometheus.GaugeValue, line.Avg60, gname, resource, kind, "avg60")
	ch <- prometheus.MustNewConstMetric(psiRatioDesc, prometheus.GaugeValue, line.Avg300, gname, resource, kind, "avg300")
}

// collectSummary emits the collector-wide counters (once per scrape).
func (c *cgroupCollector) collectSummary(ch chan<- prometheus.Metric) {
	ch <- prometheus.MustNewConstMetric(cgroupSkippedDesc, prometheus.CounterValue, c.skipped)
}
