# Deployment examples (EKS Auto Mode)

Working examples for running the cgroup v2 build on **Amazon EKS Auto Mode**
(Bottlerocket nodes), scraped by plain Prometheus via `prometheus.io/scrape`
annotations (no Prometheus Operator / CRDs). They are illustrative — genericize
the placeholders for your environment.

| File | What it is |
|---|---|
| `process-exporter-daemonset.yaml` | process-exporter DaemonSet + ConfigMap + headless Service, with the `-cgroup.*` families enabled. The main artifact. |
| `nma-metrics-proxy-daemonset.yaml` | Optional nginx reverse-proxy exposing the eks-node-monitoring-agent Go-runtime metrics for scraping. Only needed for the dashboard's "NMA Go Runtime" section. |
| `grafana-dashboard-combined.json` | Grafana dashboard (uid `node-proc-cgroup`): node memory/CPU/pressure, per-process metrics, and per-cgroup PSI/memory/cpu/pids. Import via Dashboards → New → Import → Upload JSON. |

## Substitute before applying

- **Image**: replace `YOUR_REGISTRY/process-exporter:latest` in
  `process-exporter-daemonset.yaml` with your built image. See the repo README
  section [**Building a container image**](../../README.md#building-a-container-image)
  for the multi-arch (amd64 + arm64) buildx + push flow — the arm64 image is
  required for Graviton nodes.
- **Namespace**: the examples use `monitoring`. Change it consistently across all
  three resources if you deploy elsewhere.
- **NMA metrics port** (proxy only): `nma-metrics-proxy-daemonset.yaml` proxies
  `127.0.0.1:8801` — confirm/adjust to your eks-node-monitoring-agent build.

## Why these settings (EKS Auto Mode specifics)

- **`tolerations: [{operator: Exists}]`** — EKS Auto Mode's built-in `system`
  NodePool taints its nodes with `CriticalAddonsOnly:NoSchedule` (the
  general-purpose NodePool is untainted); tolerating everything ensures the
  DaemonSet also lands on `system` nodes. Narrow it if you only want a subset.
- **`hostPID: true` + `privileged`** — required so the exporter sees host
  processes and can read other services' `/proc/<pid>` entries.
- **Two hostPath mounts** — `/proc` (`--procfs=/host/proc`) and `/sys/fs/cgroup`
  (`--cgroupfs=/host/sys/fs/cgroup`). The collector reads the `0::/…` path from
  `/proc/<pid>/cgroup` and joins it under the cgroupfs root, so both are needed.
- **`--children=false`** — keeps each group 1:1 with its cgroup (fewer
  skip-and-count events). Bottlerocket runs the unified cgroup v2 hierarchy with
  PSI enabled, which the collector requires.
- **Scrape annotations on the Service only** — annotating both the Service and
  the pod template makes Prometheus scrape each pod twice.

The image is `FROM scratch` (no shell): inspect `/metrics` via
`kubectl port-forward` + curl, or query Prometheus — not `kubectl exec ... wget`.
