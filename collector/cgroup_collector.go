package collector

import (
	"errors"
	"log"

	"github.com/ncabatoff/process-exporter/cgroup"
	"github.com/ncabatoff/process-exporter/config"
	"github.com/ncabatoff/process-exporter/proc"
	"github.com/prometheus/client_golang/prometheus"
)

// microsecondsPerSecond converts PSI totals (reported in microseconds) to the
// seconds used by the exported *_seconds_total counters.
const microsecondsPerSecond = 1e6

// defaultMaxPaths bounds how many cgroups one configured pattern may expand to
// when the config names no limit. Pod cgroups are created and destroyed by the
// kubelet, so a wildcard over them has no natural ceiling; this is what keeps
// one line of config from becoming thousands of series.
const defaultMaxPaths = 200

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
	// IO enables io.stat (per-device bytes and operation counts).
	IO bool
	// Config carries the field allowlists, the PSI window selection, and the
	// cgroup paths to read directly.
	Config config.CgroupConfig
	Debug  bool
}

// defaultMemoryStatFields is the curated subset of memory.stat emitted when the
// config names no explicit fields. Kept small deliberately: memory.stat has
// ~40 fields and emitting all of them is the biggest cardinality trap.
var defaultMemoryStatFields = []string{"anon", "file", "kernel", "slab", "sock"}

// descSet holds one descriptor per metric family. Two instances exist: the
// per-group set, labelled "groupname", and the per-path set, labelled "cgroup".
// They differ in the metric name, in the name of that first label and in the
// wording of the help string, but never in label arity, so every emit function
// below works against either one.
type descSet struct {
	psiCPUWaiting    *prometheus.Desc
	psiCPUStalled    *prometheus.Desc
	psiMemoryWaiting *prometheus.Desc
	psiMemoryStalled *prometheus.Desc
	psiIOWaiting     *prometheus.Desc
	psiIOStalled     *prometheus.Desc
	psiPercent       *prometheus.Desc

	memoryCurrent     *prometheus.Desc
	memorySwapCurrent *prometheus.Desc
	memoryStat        *prometheus.Desc
	memoryEvents      *prometheus.Desc

	cpu *prometheus.Desc

	pidsCurrent   *prometheus.Desc
	pidsMax       *prometheus.Desc
	pidsPeak      *prometheus.Desc
	pidsEventsMax *prometheus.Desc

	ioBytes *prometheus.Desc
	ioOps   *prometheus.Desc
}

// withExtra returns the identifying label followed by the family's own labels.
func withExtra(label string, extra ...string) []string {
	return append([]string{label}, extra...)
}

// newDescSet builds a descriptor set. prefix is the metric-name prefix, label is
// the name of the identifying label, and subject is the noun phrase the help
// strings use for whatever the numbers describe.
//
// Following node_exporter's vocabulary, the PSI resource (cpu/memory/io) and the
// some/full distinction ("waiting"/"stalled") are encoded in the metric name
// rather than in labels. Only the total (as seconds) is emitted by default; the
// avg10/60/300 ratios are opt-in.
func newDescSet(prefix, label, subject string) *descSet {
	return &descSet{
		psiCPUWaiting: prometheus.NewDesc(
			prefix+"_pressure_cpu_waiting_seconds_total",
			"Total time procs in "+subject+" were stalled waiting on CPU (PSI some).",
			withExtra(label), nil),

		// cpu.pressure gained a "full" line in Linux 5.13; on older kernels the
		// line is absent and ReadPSI returns Full == nil, so this stays unemitted.
		psiCPUStalled: prometheus.NewDesc(
			prefix+"_pressure_cpu_stalled_seconds_total",
			"Total time all procs in "+subject+" were stalled on CPU (PSI full, Linux >= 5.13).",
			withExtra(label), nil),

		psiMemoryWaiting: prometheus.NewDesc(
			prefix+"_pressure_memory_waiting_seconds_total",
			"Total time some proc in "+subject+" was stalled on memory (PSI some).",
			withExtra(label), nil),

		psiMemoryStalled: prometheus.NewDesc(
			prefix+"_pressure_memory_stalled_seconds_total",
			"Total time all procs in "+subject+" were stalled on memory (PSI full).",
			withExtra(label), nil),

		psiIOWaiting: prometheus.NewDesc(
			prefix+"_pressure_io_waiting_seconds_total",
			"Total time some proc in "+subject+" was stalled on IO (PSI some).",
			withExtra(label), nil),

		psiIOStalled: prometheus.NewDesc(
			prefix+"_pressure_io_stalled_seconds_total",
			"Total time all procs in "+subject+" were stalled on IO (PSI full).",
			withExtra(label), nil),

		// The kernel reports these as percentages (0-100), so the metric is
		// named _percent rather than _ratio (which by convention would be 0-1).
		psiPercent: prometheus.NewDesc(
			prefix+"_pressure_percent",
			"Kernel-computed PSI average, percent of wall time stalled (0-100), over the labelled window.",
			withExtra(label, "resource", "kind", "window"), nil),

		memoryCurrent: prometheus.NewDesc(
			prefix+"_memory_current_bytes",
			"Total memory currently in use by "+subject+" (memory.current).",
			withExtra(label), nil),

		memorySwapCurrent: prometheus.NewDesc(
			prefix+"_memory_swap_current_bytes",
			"Swap currently in use by "+subject+" (memory.swap.current); the whole-cgroup analog of per-process memory_bytes{memtype=\"swapped\"}.",
			withExtra(label), nil),

		memoryStat: prometheus.NewDesc(
			prefix+"_memory_stat_bytes",
			"Selected memory.stat fields for "+subject+".",
			withExtra(label, "field"), nil),

		// memory.events counters, one series per key the kernel reports: low,
		// high, max, oom, oom_kill and (Linux >= 5.13) oom_group_kill. oom_kill
		// is the number of processes the OOM killer killed in this cgroup or its
		// descendants, which is what attributes a kill to a systemd unit or a pod
		// after the fact. memory.events is hierarchical unless the cgroup2 mount
		// carries memory_localevents.
		memoryEvents: prometheus.NewDesc(
			prefix+"_memory_events_total",
			"Memory event counters for "+subject+" (memory.events), by event: low, high, max, oom, oom_kill, oom_group_kill.",
			withExtra(label, "event"), nil),

		cpu: prometheus.NewDesc(
			prefix+"_cpu_seconds_total",
			"CPU time consumed by "+subject+" (cpu.stat), by mode.",
			withExtra(label, "mode"), nil),

		pidsCurrent: prometheus.NewDesc(
			prefix+"_pids_current",
			"Number of processes/threads currently in "+subject+" (pids.current).",
			withExtra(label), nil),

		// In cgroup v2 the pids controller counts every task (thread or process),
		// so pids.max is the combined process+thread limit — there is no separate
		// thread cap at the cgroup level. pids.peak is the high-water mark, which
		// survives a fork-bomb / thread-exhaustion event even after the count
		// recedes (e.g. after an NMA SIGABRT), unlike the instantaneous
		// pids.current. Omitted when the file is absent (older kernels).
		pidsMax: prometheus.NewDesc(
			prefix+"_pids_max",
			"Task (process+thread) limit for "+subject+" (pids.max); only emitted when a finite limit is set.",
			withExtra(label), nil),

		pidsPeak: prometheus.NewDesc(
			prefix+"_pids_peak",
			"Highest number of tasks (processes+threads) "+subject+" has ever held (pids.peak, Linux >= 6.1).",
			withExtra(label), nil),

		// pids.events: the "max" key counts how many times a fork/clone in this
		// cgroup was rejected because pids.max was reached. This is the direct
		// pid/thread-exhaustion signal — a monotonic counter that records the
		// event even if the task count has since receded (e.g. after a SIGABRT),
		// unlike pids.current/peak. rate()/increase() it to alert on exhaustion.
		pidsEventsMax: prometheus.NewDesc(
			prefix+"_pids_events_max_total",
			"Number of fork/clone failures in "+subject+" caused by hitting pids.max (pids.events 'max').",
			withExtra(label), nil),

		// io.stat counters, per block device (the "MAJ:MIN" device label lets you
		// isolate a single volume, e.g. the root EBS device). rios/wios/dios are
		// the operation counts that correspond to device IOPS; rbytes/wbytes/dbytes
		// are the throughput. iomode is read/write/discard.
		ioBytes: prometheus.NewDesc(
			prefix+"_io_bytes_total",
			"Bytes transferred to/from each block device by "+subject+" (io.stat rbytes/wbytes/dbytes), by device and iomode.",
			withExtra(label, "device", "iomode"), nil),

		ioOps: prometheus.NewDesc(
			prefix+"_io_ops_total",
			"IO operations issued to each block device by "+subject+" (io.stat rios/wios/dios); the per-cgroup analog of device IOPS, by device and iomode.",
			withExtra(label, "device", "iomode"), nil),
	}
}

// groupDescs is the per-process-group metric set. pathDescs is the
// per-cgroup-path set; it carries a different metric name so the two never
// collide in a registry or in a query, because a path series is identified by
// where it sits in the cgroup tree rather than by which processes the exporter
// grouped.
var (
	groupDescs = newDescSet("namedprocess_namegroup_cgroup", "groupname", "this group's cgroup")
	pathDescs  = newDescSet("namedprocess_cgroup", "cgroup", "this cgroup")

	cgroupSkippedDesc = prometheus.NewDesc(
		"namedprocess_scrape_cgroup_skipped",
		"incremented each time a group's cgroup metrics are skipped because its procs span multiple cgroups",
		nil, nil)

	// A pattern over its limit is dropped whole rather than truncated, so this
	// counter is the only signal that configured cgroups are missing from the
	// metrics. Alert on it.
	cgroupPatternDroppedDesc = prometheus.NewDesc(
		"namedprocess_scrape_cgroup_pattern_dropped",
		"incremented each time a configured cgroup path pattern is dropped, because it matched more cgroups than max_paths or could not be expanded",
		nil, nil)
)

// cgroupCollector reads cgroupv2 metrics for groups with a resolved single
// cgroup path, and for the configured cgroup paths, and emits them. It is
// created only when at least one family is enabled; when nil, the exporter
// behaves exactly as upstream.
type cgroupCollector struct {
	reader *cgroup.Reader
	psi    bool
	memory bool
	cpu    bool
	pids   bool
	io     bool
	// psiWindows is the set of PSI windows to emit, resolved from config.
	// "total" gates the *_seconds_total counters; "avg10"/"avg60"/"avg300"
	// each gate one _pressure_percent series. Empty config defaults to
	// {"total"} so behavior matches "total only".
	psiWindows    map[string]bool
	memStatFields []string
	// paths holds the configured cgroup path patterns. They are expanded on
	// every scrape, because pod cgroups come and go between scrapes.
	paths    []string
	maxPaths int
	debug    bool

	// skipped counts groups whose cgroup metrics were skipped this process
	// lifetime because their procs spanned multiple cgroups (skip-and-count).
	skipped float64
	// patternDropped counts patterns dropped this process lifetime for
	// exceeding maxPaths or for failing to expand.
	patternDropped float64
}

// newCgroupCollector opens the cgroupfs root and returns a collector for the
// enabled families. Returns nil (no error) if no family is enabled.
func newCgroupCollector(opt CgroupCollectorOption) (*cgroupCollector, error) {
	if !opt.PSI && !opt.Memory && !opt.CPU && !opt.Pids && !opt.IO {
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
	maxPaths := opt.Config.MaxPaths
	if maxPaths == 0 {
		maxPaths = defaultMaxPaths
	}
	return &cgroupCollector{
		reader:        reader,
		psi:           opt.PSI,
		memory:        opt.Memory,
		cpu:           opt.CPU,
		pids:          opt.Pids,
		io:            opt.IO,
		psiWindows:    resolvePSIWindows(opt.Config.PSIWindows),
		memStatFields: memStatFields,
		paths:         opt.Config.Paths,
		maxPaths:      maxPaths,
		debug:         opt.Debug,
	}, nil
}

// resolvePSIWindows turns the configured window list into a lookup set,
// silently ignoring unknown entries. An empty/absent list defaults to
// {"total"}: emit the *_seconds_total counters and no averages, matching the
// documented default. A list that names only averages disables totals, exactly
// as configured.
func resolvePSIWindows(windows []string) map[string]bool {
	if len(windows) == 0 {
		return map[string]bool{"total": true}
	}
	set := make(map[string]bool, len(windows))
	for _, w := range windows {
		switch w {
		case "total", "avg10", "avg60", "avg300":
			set[w] = true
		}
	}
	return set
}

// close releases the cgroupfs root handle. Safe to call on a nil collector or
// one whose reader failed to open.
func (c *cgroupCollector) close() {
	if c != nil && c.reader != nil {
		c.reader.Close()
	}
}

func (c *cgroupCollector) describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*descSet{groupDescs, pathDescs} {
		ch <- d.psiCPUWaiting
		ch <- d.psiCPUStalled
		ch <- d.psiMemoryWaiting
		ch <- d.psiMemoryStalled
		ch <- d.psiIOWaiting
		ch <- d.psiIOStalled
		ch <- d.psiPercent
		ch <- d.memoryCurrent
		ch <- d.memorySwapCurrent
		ch <- d.memoryStat
		ch <- d.memoryEvents
		ch <- d.cpu
		ch <- d.pidsCurrent
		ch <- d.pidsMax
		ch <- d.pidsPeak
		ch <- d.pidsEventsMax
		ch <- d.ioBytes
		ch <- d.ioOps
	}
	ch <- cgroupSkippedDesc
	ch <- cgroupPatternDroppedDesc
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
	// An empty path means no proc reported a v2 cgroup. A "/" path is the
	// machine-wide root cgroup, which shows up on hybrid/v1-with-empty-v2 hosts
	// (procs report "0::/"); emitting it would mislabel host-root totals as this
	// group's own metrics, so skip both.
	if group.CgroupV2Path == "" || group.CgroupV2Path == "/" {
		return
	}

	c.collectOne(ch, groupDescs, gname, group.CgroupV2Path)
}

// collectPaths emits metrics for the configured cgroup path patterns. This is
// the collection that does not depend on process grouping, which is what makes
// a slice the exporter has no process group for — kubepods.slice and the pod
// cgroups under it — readable at all.
//
// The patterns are expanded on every scrape, because a pod cgroup created since
// the last scrape has to appear without a restart and one that went away has to
// stop being read. A pattern that cannot be expanded, or that matches more
// cgroups than maxPaths, is dropped whole and counted: a truncated expansion
// would quietly leave cgroups out of a sum.
func (c *cgroupCollector) collectPaths(ch chan<- prometheus.Metric) {
	for _, pattern := range c.paths {
		matches, err := c.reader.ExpandPattern(pattern, c.maxPaths)
		if err != nil {
			c.patternDropped++
			// Over the limit is a configuration problem the operator has to
			// see, so it is logged whatever -debug says. Anything else is a
			// transient read error and stays behind the flag.
			if errors.Is(err, cgroup.ErrTooManyMatches) {
				log.Printf("dropping cgroup pattern %q: %v", pattern, err)
			} else {
				c.debugf("dropping cgroup pattern %q: %v", pattern, err)
			}
			continue
		}
		for _, p := range matches {
			c.collectOne(ch, pathDescs, p, p)
		}
	}
}

// collectOne emits every enabled family for one cgroup, labelled with id.
func (c *cgroupCollector) collectOne(ch chan<- prometheus.Metric, d *descSet, id, cgroupPath string) {
	if c.psi {
		c.collectPSI(ch, d, id, cgroupPath)
	}
	if c.memory {
		c.collectMemory(ch, d, id, cgroupPath)
	}
	if c.cpu {
		c.collectCPU(ch, d, id, cgroupPath)
	}
	if c.pids {
		c.collectPids(ch, d, id, cgroupPath)
	}
	if c.io {
		c.collectIO(ch, d, id, cgroupPath)
	}
}

// collectMemory emits memory.current, memory.swap.current, the allowlisted
// memory.stat fields and the memory.events counters.
func (c *cgroupCollector) collectMemory(ch chan<- prometheus.Metric, d *descSet, id, cgroupPath string) {
	if cur, err := c.reader.ReadUint64(cgroupPath, "memory.current"); err != nil {
		c.debugf("cgroup %q: reading memory.current: %v", id, err)
	} else {
		ch <- prometheus.MustNewConstMetric(d.memoryCurrent, prometheus.GaugeValue, float64(cur), id)
	}

	// memory.swap.current is absent when swap accounting is disabled; a read
	// error is skipped like any other missing file so we degrade gracefully.
	if sw, err := c.reader.ReadUint64(cgroupPath, "memory.swap.current"); err != nil {
		c.debugf("cgroup %q: reading memory.swap.current: %v", id, err)
	} else {
		ch <- prometheus.MustNewConstMetric(d.memorySwapCurrent, prometheus.GaugeValue, float64(sw), id)
	}

	// memory.events is read before memory.stat so that a missing memory.stat
	// cannot suppress the OOM counters. Every key present is emitted; the
	// kernel adds keys over time (oom_group_kill arrived in 5.13), and a
	// counter that exists should not be dropped by an allowlist here.
	if ev, err := c.reader.ReadKeyed(cgroupPath, "memory.events"); err != nil {
		c.debugf("cgroup %q: reading memory.events: %v", id, err)
	} else {
		for event, v := range ev {
			ch <- prometheus.MustNewConstMetric(d.memoryEvents, prometheus.CounterValue, float64(v), id, event)
		}
	}

	stat, err := c.reader.ReadKeyed(cgroupPath, "memory.stat")
	if err != nil {
		c.debugf("cgroup %q: reading memory.stat: %v", id, err)
		return
	}
	for _, field := range c.memStatFields {
		// Only emit fields actually present, so a mistyped/absent field silently
		// produces no series rather than a zero.
		if v, ok := stat[field]; ok {
			ch <- prometheus.MustNewConstMetric(d.memoryStat, prometheus.GaugeValue, float64(v), id, field)
		}
	}
}

// collectCPU emits user/system CPU seconds from cpu.stat (reported in µs).
func (c *cgroupCollector) collectCPU(ch chan<- prometheus.Metric, d *descSet, id, cgroupPath string) {
	stat, err := c.reader.ReadKeyed(cgroupPath, "cpu.stat")
	if err != nil {
		c.debugf("cgroup %q: reading cpu.stat: %v", id, err)
		return
	}
	if v, ok := stat["user_usec"]; ok {
		ch <- prometheus.MustNewConstMetric(d.cpu, prometheus.CounterValue, float64(v)/microsecondsPerSecond, id, "user")
	}
	if v, ok := stat["system_usec"]; ok {
		ch <- prometheus.MustNewConstMetric(d.cpu, prometheus.CounterValue, float64(v)/microsecondsPerSecond, id, "system")
	}
}

// collectPids emits pids.current plus the exhaustion-related pids.max, pids.peak
// and pids.events. Each is read independently so a cgroup with only some of the
// files (e.g. no pids.peak on kernels < 6.1, or no pids.max on the root cgroup)
// still emits whatever is present.
func (c *cgroupCollector) collectPids(ch chan<- prometheus.Metric, d *descSet, id, cgroupPath string) {
	if cur, err := c.reader.ReadUint64(cgroupPath, "pids.current"); err != nil {
		c.debugf("cgroup %q: reading pids.current: %v", id, err)
	} else {
		ch <- prometheus.MustNewConstMetric(d.pidsCurrent, prometheus.GaugeValue, float64(cur), id)
	}

	// pids.max holds "max" (unlimited) on cgroups with no configured limit; in
	// that case emit no series so a saturation ratio (current/max) is simply
	// absent rather than dividing by a sentinel.
	if limit, limited, err := c.reader.ReadPidsMax(cgroupPath); err != nil {
		c.debugf("cgroup %q: reading pids.max: %v", id, err)
	} else if limited {
		ch <- prometheus.MustNewConstMetric(d.pidsMax, prometheus.GaugeValue, float64(limit), id)
	}

	if peak, err := c.reader.ReadUint64(cgroupPath, "pids.peak"); err != nil {
		c.debugf("cgroup %q: reading pids.peak: %v", id, err)
	} else {
		ch <- prometheus.MustNewConstMetric(d.pidsPeak, prometheus.GaugeValue, float64(peak), id)
	}

	if events, err := c.reader.ReadKeyed(cgroupPath, "pids.events"); err != nil {
		c.debugf("cgroup %q: reading pids.events: %v", id, err)
	} else if v, ok := events["max"]; ok {
		ch <- prometheus.MustNewConstMetric(d.pidsEventsMax, prometheus.CounterValue, float64(v), id)
	}
}

// ioStatFields maps io.stat's per-device counter keys to the metric and iomode
// label they are emitted under. Bytes and ops are separate metrics so each keeps
// consistent units; the read/write/discard direction is a label. Keys absent on
// a given kernel (dbytes/dios predate some kernels, or are 0 and omitted) simply
// produce no series.
var ioStatFields = []struct {
	key    string
	bytes  bool
	iomode string
}{
	{"rbytes", true, "read"},
	{"wbytes", true, "write"},
	{"dbytes", true, "discard"},
	{"rios", false, "read"},
	{"wios", false, "write"},
	{"dios", false, "discard"},
}

// collectIO emits per-device io.stat bytes and operation counts. The device
// ("MAJ:MIN") is a label so a single volume (e.g. the root EBS device) can be
// isolated; rate() over the *_io_ops_total series gives per-cgroup IOPS.
func (c *cgroupCollector) collectIO(ch chan<- prometheus.Metric, d *descSet, id, cgroupPath string) {
	// io.stat is absent when the io controller isn't enabled for the cgroup; a
	// read error is skipped like any other missing file so we degrade gracefully.
	devices, err := c.reader.ReadIOStat(cgroupPath)
	if err != nil {
		c.debugf("cgroup %q: reading io.stat: %v", id, err)
		return
	}
	for dev, counters := range devices {
		for _, f := range ioStatFields {
			// Only emit fields actually present, so a counter the kernel omits
			// produces no series rather than a spurious zero.
			v, ok := counters[f.key]
			if !ok {
				continue
			}
			desc := d.ioOps
			if f.bytes {
				desc = d.ioBytes
			}
			ch <- prometheus.MustNewConstMetric(desc, prometheus.CounterValue, float64(v), id, dev, f.iomode)
		}
	}
}

func (c *cgroupCollector) debugf(format string, args ...interface{}) {
	if c.debug {
		log.Printf(format, args...)
	}
}

// collectPSI reads the three pressure files for the cgroup and emits the
// configured metrics. A missing file (e.g. PSI disabled in the kernel) is
// silently skipped so the collector degrades gracefully.
func (c *cgroupCollector) collectPSI(ch chan<- prometheus.Metric, d *descSet, id, cgroupPath string) {
	// resource -> (some desc, full desc). A nil full desc means the resource
	// never has a "full" line to export. cpu.pressure emits "full" only on
	// Linux >= 5.13; on older kernels ReadPSI returns Full == nil, so a
	// non-nil fullDesc here is still safe (nothing is emitted when Full is nil).
	type psiTarget struct {
		resource string
		someDesc *prometheus.Desc
		fullDesc *prometheus.Desc
	}
	targets := []psiTarget{
		{"cpu", d.psiCPUWaiting, d.psiCPUStalled},
		{"memory", d.psiMemoryWaiting, d.psiMemoryStalled},
		{"io", d.psiIOWaiting, d.psiIOStalled},
	}

	for _, tgt := range targets {
		stats, err := c.reader.ReadPSI(cgroupPath, tgt.resource)
		if err != nil {
			if c.debug {
				log.Printf("cgroup %q: reading %s.pressure: %v", id, tgt.resource, err)
			}
			continue
		}
		if stats.Some != nil {
			if c.psiWindows["total"] {
				ch <- prometheus.MustNewConstMetric(tgt.someDesc,
					prometheus.CounterValue, float64(stats.Some.Total)/microsecondsPerSecond, id)
			}
			c.emitPercents(ch, d, id, tgt.resource, "some", *stats.Some)
		}
		if stats.Full != nil && tgt.fullDesc != nil {
			if c.psiWindows["total"] {
				ch <- prometheus.MustNewConstMetric(tgt.fullDesc,
					prometheus.CounterValue, float64(stats.Full.Total)/microsecondsPerSecond, id)
			}
			c.emitPercents(ch, d, id, tgt.resource, "full", *stats.Full)
		}
	}
}

// emitPercents emits the kernel's pre-computed avgN percentages, one series per
// window named in the config allowlist.
func (c *cgroupCollector) emitPercents(ch chan<- prometheus.Metric, d *descSet, id, resource, kind string, line cgroup.PSILine) {
	if c.psiWindows["avg10"] {
		ch <- prometheus.MustNewConstMetric(d.psiPercent, prometheus.GaugeValue, line.Avg10, id, resource, kind, "avg10")
	}
	if c.psiWindows["avg60"] {
		ch <- prometheus.MustNewConstMetric(d.psiPercent, prometheus.GaugeValue, line.Avg60, id, resource, kind, "avg60")
	}
	if c.psiWindows["avg300"] {
		ch <- prometheus.MustNewConstMetric(d.psiPercent, prometheus.GaugeValue, line.Avg300, id, resource, kind, "avg300")
	}
}

// collectSummary emits the collector-wide counters (once per scrape).
func (c *cgroupCollector) collectSummary(ch chan<- prometheus.Metric) {
	ch <- prometheus.MustNewConstMetric(cgroupSkippedDesc, prometheus.CounterValue, c.skipped)
	ch <- prometheus.MustNewConstMetric(cgroupPatternDroppedDesc, prometheus.CounterValue, c.patternDropped)
}
