# Design: cgroup v2 metrics (PSI, memory, cpu, pids)

Status: implemented on `feature/go126-cgroupv2-psi-collector`; built, deployed,
and validated on EKS/Bottlerocket. This document records the design rationale
and the known open issues from code review so they travel with the code.

User-facing reference (flags, metric names, YAML) lives in the repo `README.md`
under "cgroup v2 Metrics". This document is the *why*, not the *how-to*.

## Goal

Export per-group metrics sourced from the cgroup v2 hierarchy a group's
processes belong to — pressure-stall information (PSI) and selected cgroup stat
files — alongside the existing `/proc`-derived process metrics. The motivating
case: on hosts where process groups map 1:1 onto cgroups (systemd units under
`runtime.slice`, e.g. Bottlerocket), `memory.current`/PSI is the number the
kernel enforces limits against and that triggers OOM kills, whereas summed
process RSS only reflects the matched binaries. (The gap between whole-cgroup
`memory.current` and summed process RSS is itself a useful diagnostic — page
cache, kernel memory, and unmatched/child procs charged to the cgroup.)

## Constraints & principles

- **Opt-in, zero-cost when off.** With no `-cgroup.*` flag the collector is nil
  and the exporter behaves exactly as upstream. Each family is a separate flag
  because `memory.stat` in particular is a cardinality trap (~40 fields).
- **Independent of the `proc` package's reading.** The `cgroup` package takes a
  cgroupfs root + a cgroup path and reads/parses pseudo-files; it does not know
  about `/proc`.
- **Confined reads.** The cgroupfs root is opened with `os.Root` so a surprising
  symlink or `..` in a cgroup path cannot escape the subtree. The agent runs
  privileged and reads paths derived from `/proc/<pid>/cgroup`, so this matters.
- **Bounded cardinality.** `memory.stat` fields and PSI windows are allowlists;
  the defaults are a small curated subset, never "all fields".

## Architecture

```
/proc/<pid>/cgroup ──(read.go)──► Static.CgroupV2Path   (the "0::/path" line, HierarchyID==0)
                                        │
                             (tracker.go getUpdate) Update.CgroupV2Path
                                        │
                          (grouper.go groupadd) Group.CgroupV2Path / .CgroupV2Conflict
                                        │
                    (collector/cgroup_collector.go collectGroup)
                                        │
                    cgroup.Reader (os.Root on /sys/fs/cgroup) ─► PSI / memory / cpu / pids
```

- **Path resolution** (`proc/read.go`): the unified v2 path is the `0::/path`
  line of `/proc/<pid>/cgroup` (`HierarchyID == 0`).
- **Conflict policy** (`proc/grouper.go`): a group only gets cgroup metrics when
  all its procs resolve to one cgroup path. Differing paths set
  `CgroupV2Conflict`; the collector then skips the group and bumps
  `namedprocess_scrape_cgroup_skipped` ("skip-and-count").
- **Collection** (`collector/cgroup_collector.go`): serialized through the
  existing single scrape goroutine (`scrapeChan`), so collector state is not
  accessed concurrently.

## Design decisions worth noting

- **`cpu_stalled` emitted only on Linux >= 5.13.** `cpu.pressure` gained a `full`
  line in 5.13. The collector now carries a `cpu_stalled` descriptor; on older
  kernels `ReadPSI` returns `Full == nil` so nothing is emitted.
- **PSI encoded in metric names, not labels** (`waiting`=some, `stalled`=full),
  following node_exporter's vocabulary.
- **Skip-and-count over best-effort attribution.** When procs span cgroups we
  emit nothing rather than pick one, to avoid silently mislabeling.
- **`memory.swap.current` is its own metric, not a `memory.stat` field.**
  `-cgroup.memory` also reads `memory.swap.current` and emits
  `namedprocess_namegroup_cgroup_memory_swap_current_bytes` (the whole-cgroup
  analog of the per-process `memory_bytes{memtype="swapped"}`). Cgroup swap does
  *not* live in `memory.stat` — the `swapcached`/`zswap*` stat keys are
  swap-cached-back-in-RAM, a different quantity — so the `stat_fields` allowlist
  cannot produce it; it needed a dedicated read. Omitted when swap accounting is
  disabled (file absent).

## Code review (2026-07-13)

Ranked; see the review for full failure scenarios. These are tracked here so a
fork evaluator / upstream reviewer sees them.

### Fixed (2026-07-13)

1. **`cgroups.psi.windows` allowlist is now honored per-window.** The single
   `emitAverages` bool was replaced by a resolved `psiWindows` set: `"total"`
   gates the `*_seconds_total` counters and each of `avg10`/`avg60`/`avg300`
   gates its own `_pressure_percent` series. `windows: [avg60]` now emits exactly
   one percent series and no counter. Covered by `TestCgroupCollectorWindow-
   Allowlist` and the rewritten `TestCgroupCollectorAveragesOptIn`.
2. **`CgroupV2Path == "/"` no longer mislabels root-cgroup metrics.**
   `collectGroup` now skips both `""` and `"/"`. Covered by
   `TestCgroupCollectorRootPathSkipped`.
3. **`_pressure_ratio` renamed to `_pressure_percent`.** The value is a
   percentage (0–100), not a Prometheus ratio (0–1). Done before release since
   there is no back-compat constraint yet.
4. **cpu `full` pressure now exported on kernels ≥5.13** via a new
   `cpu_stalled_seconds_total` descriptor; older kernels emit nothing (Full is
   nil).

### Fixed (2026-07-13, follow-up pass)

7. **Mixed empty-path procs now flag a conflict** (`grouper.go`): a group where
   some procs report `""` and others a real path is treated as a conflict
   (skip-and-count) instead of silently resolving to the non-empty subset.
   Detection is order-independent (tracks `cgroupV2SawEmpty`). Covered by two new
   mixed-order cases in `TestGrouperCgroupV2Path`.
8. **`os.Root` handle released on the error path**: `NewProcessCollector` now
   calls `cgroupCollector.close()` when the first `Update` fails, so the root fd
   isn't leaked. (Steady-state lifetime is still process-long by design.)
10. **Removed dead `Reader.exists()`** (+ its test and the now-unused `io/fs`
    import) and collapsed the three copy-pasted `avg10/60/300` ParseFloat blocks
    in `psi.go` into a single key→field map.

### Won't fix / documented instead

5. **cgroup-namespace assumption** — paths from `/proc/<pid>/cgroup` are relative
   to the exporter's own cgroup namespace; needs `hostPID: true` (without it,
   reads ENOENT and degrade to zero cgroup metrics). This is a deployment
   precondition, not a code bug — documented in the `docs/examples/` manifests
   and their README.
6. **Transient empty/conflict groups drop cgroup counters** → possible false
   `rate()`/`increase()` resets. Giving cgroup counters a full accumulator (like
   the process counters) is disproportionate to the value; accepted as a known
   limitation. Fix #7 slightly widens when a group is skipped (mixed
   empty/non-empty) — the correct trade: skip-and-count over silent
   misattribution.
9. **`PSILine`/`PSIStats` duplicate the vendored `prometheus/procfs` types** —
   left as-is deliberately. procfs's parser is unexported and its public entry is
   hardcoded to `/proc/pressure`, so only the structs could be reused; coupling
   our parser to an external package's types for cosmetic dedup isn't worth it.

## Testing

- Unit: `cgroup/*_test.go` (parsing, os.Root confinement/escape),
  `collector/cgroup_collector_test.go`, `config/config_test.go`,
  `proc/grouper_test.go` (conflict tracking).
- End-to-end: validated as a DaemonSet on EKS Auto Mode / Bottlerocket, plain
  Prometheus via annotations; all families confirmed emitting for
  kubelet/containerd/ipamd/coredns. Ready-to-adapt manifests and the dashboard
  are in [`docs/examples/`](examples/).
