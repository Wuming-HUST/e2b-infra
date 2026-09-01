# Memory Overcommit Ablation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Establish one correctness baseline and independent Git branches that each enable exactly one memory-overcommit mechanism for controlled ablation experiments on openEuler 24.04 / Linux 6.6.

**Architecture:** `mem-overcommit-baseline` contains correctness fixes, common observability, and experiment documentation only. Every `mem-overcommit-exp-*` branch forks from the baseline commit and adds one treatment; experiment branches never contain another treatment branch. The same VM image, workload, node shape, swap capacity, admission limits, and measurement procedure are used for control and treatment runs.

**Tech Stack:** Go, Firecracker, cgroup v2, Bash host initialization, Linux 6.6 MGLRU/DAMON/KSM/zswap/zram/ETMem, existing E2B snapshot and UFFD pipeline.

## Global Constraints

- Target host is openEuler 24.04 with Linux 6.6; ublk is unavailable.
- HugeTLB is disabled for all 4 KiB-page experiments.
- Baseline and treatment branches differ by one mechanism only.
- Cross-tenant KSM is prohibited; the KSM branch is for an isolated trust domain.
- Host and guest kernel features must pass branch-specific preflight checks before a run is accepted.
- Density is compared at the same runtime and resume p99 SLO; raw sandbox count without SLO compliance is not accepted.
- No experiment may silently continue when a requested kernel feature is unavailable.

---

## Branch Topology

```text
arm-perf-on-2026.09
└── mem-overcommit-baseline
    ├── mem-overcommit-exp-file-page-cache
    ├── mem-overcommit-exp-host-mglru
    ├── mem-overcommit-exp-host-zswap
    ├── mem-overcommit-exp-guest-damon-fpr
    ├── mem-overcommit-exp-ksm
    ├── mem-overcommit-exp-host-etmem
    ├── mem-overcommit-exp-guest-zram
    └── mem-overcommit-exp-snapshot-compression
```

## Progress

| Branch | Treatment | Status | Verification gate |
|---|---|---|---|
| `mem-overcommit-baseline` | Correctness and controls | Locally compiled; remote validation pending | API placement tests, arm64 Linux orchestrator build, `bash -n` passed |
| `mem-overcommit-exp-file-page-cache` | Shared immutable raw file with Firecracker File/MAP_PRIVATE backend | Planned | Same inode in all Firecracker maps; PSS/Shared_Clean grows sublinearly |
| `mem-overcommit-exp-host-mglru` | Host MGLRU only | Planned | `/sys/kernel/mm/lru_gen/enabled` reports requested mask |
| `mem-overcommit-exp-host-zswap` | Host zswap only | Planned | zswap enabled; pool and swap counters move under pressure |
| `mem-overcommit-exp-guest-damon-fpr` | Guest DAMON_RECLAIM file-page reclaim plus FPR | Planned | guest DAMON active; FPR reduces host RSS |
| `mem-overcommit-exp-ksm` | KSM for Firecracker processes in one trust domain | Planned | `/proc/<pid>/ksm_stat` enabled and KSM profit positive |
| `mem-overcommit-exp-host-etmem` | ETMem reclaim for selected Firecracker PIDs | Planned | target PID registered; cold-page counters and swap move |
| `mem-overcommit-exp-guest-zram` | Guest zram swap only | Planned | guest zram active; freed guest pages reach FPR |
| `mem-overcommit-exp-snapshot-compression` | LZ4 memory artifact chunks | Planned | round-trip checksum; lower artifact bytes without violating pause/resume SLO |

## Common Measurement Contract

Every branch records the same fields before load, at steady state, and after pressure:

```text
node: MemAvailable, anon/file/shmem, PSI some/full, pgmajfault, swapin/swapout
cgroup: memory.current, memory.peak, memory.stat, memory.events, memory.swap.current
process: RSS, PSS, Shared_Clean, Private_Dirty
firecracker: boot/resume p50/p95/p99, FPR released bytes
snapshot: logical bytes, stored bytes, capture time, first-fault latency
```

The control run is the exact `mem-overcommit-baseline` commit used as the parent of the treatment branch.

---

### Task 1: Baseline cgroup placement

**Files:**
- Modify: `packages/orchestrator/internal/sandbox/fc/process.go`
- Create: `packages/orchestrator/internal/sandbox/fc/process_cgroup_linux.go`
- Create: `packages/orchestrator/internal/sandbox/fc/process_cgroup_other.go`
- Create: `packages/orchestrator/internal/sandbox/fc/process_cgroup_linux_test.go`
- Modify: `packages/orchestrator/internal/sandbox/sandbox.go`

**Interfaces:**
- Consumes: `cgroup.CgroupHandle.GetFD() int` and `cgroup.NoCgroupFD`.
- Produces: `applyCgroupFD(*exec.Cmd, int)` and `Process.Create(..., cgroupFD int)`; fresh and resumed Firecracker processes atomically join the sandbox cgroup during clone.

- [x] Add a Linux-only failing unit test asserting a valid FD sets `UseCgroupFD` and `CgroupFD`, while `NoCgroupFD` leaves them disabled.
- [x] Compile the Linux test before implementation and confirm failure because `applyCgroupFD` does not exist.
- [x] Add Linux and non-Linux implementations of `applyCgroupFD`; the non-Linux implementation is a no-op used only for local builds.
- [x] Call `applyCgroupFD` immediately before `cmd.Start()`.
- [x] Create the cgroup in fresh sandbox creation, pass its FD to `Process.Create`, and release only the directory FD after clone.
- [x] Cross-compile the Firecracker package and complete arm64 Linux orchestrator build.

Exact verification:

```bash
cd packages/orchestrator
go test ./internal/sandbox/cgroup ./internal/sandbox/fc
go test ./internal/sandbox
CGO_ENABLED=0 GOOS=linux go test -c -o /tmp/fc-linux.test ./internal/sandbox/fc
```

### Task 2: Baseline memory-aware placement

**Files:**
- Modify: `packages/api/internal/orchestrator/nodemanager/mock.go`
- Modify: `packages/api/internal/orchestrator/placement/placement_best_of_K.go`
- Modify: `packages/api/internal/orchestrator/placement/placement_best_of_K_test.go`

**Interfaces:**
- Consumes: `Metrics.MemoryAllocatedBytes`, `MemoryUsedBytes`, `MemoryTotalBytes`, and `SandboxResources.MiBMemory`.
- Produces: memory-aware score and admission; missing memory metrics preserve the existing CPU-only behavior.

- [x] Add failing tests proving a lower-memory-pressure node scores better and a request exceeding physical headroom cannot fit.
- [x] Run the focused placement tests and confirm the new assertions fail.
- [x] Add `WithMemoryMetrics(allocated, used, total uint64)` to the test node fixture.
- [x] Normalize requested MiB with overflow-safe conversion, add memory pressure to `Score`, and require both CPU and memory fit when total memory is known.
- [x] Run focused and package tests.

Exact verification:

```bash
cd packages/api
go test ./internal/orchestrator/placement ./internal/orchestrator/nodemanager
```

### Task 3: Baseline HugeTLB gate

**Files:**
- Modify: `.github/actions/host-init/init-client.sh`
- Modify: `.github/actions/host-init/action.yml`
- Modify: `iac/provider-gcp/nomad/jobs/env.template`

**Interfaces:**
- Consumes: `HUGE_PAGES_ENABLED=true|false`.
- Produces: HugeTLB mount and reservation only when the value is `true`; false leaves the base-page pool at zero during host initialization.

- [x] Pass `HUGE_PAGES_ENABLED` explicitly into the composite action with default `false`.
- [x] Guard hugetlbfs mount, `nr_hugepages`, and `nr_overcommit_hugepages` setup behind an exact boolean check.
- [x] Make the false path clear existing boot-time reservations before workloads start.
- [ ] Inspect both traced configuration paths during the remote openEuler host-init validation.

Exact local verification:

```bash
bash -n .github/actions/host-init/init-client.sh
```


Observed local baseline verification on 2026-09-02:

```text
go test ./internal/orchestrator/placement ./internal/orchestrator/nodemanager
  PASS
CGO_ENABLED=0 GOOS=linux go test -c ./internal/sandbox/fc
  PASS
CC=\"zig cc -target aarch64-linux-gnu\" CGO_ENABLED=1 GOOS=linux GOARCH=arm64 go build ./packages/orchestrator
  PASS
go build ./packages/api
  PASS
bash -n .github/actions/host-init/init-client.sh
  PASS
```

The full orchestrator test suite is Linux-specific and is intentionally deferred to the remote Kunpeng/openEuler validation; local acceptance requires compilation only.
### Task 4: Shared file page-cache branch

**Files:**
- Modify: `packages/orchestrator/internal/sandbox/fc/client.go`
- Modify: `packages/orchestrator/internal/sandbox/sandbox.go`
- Modify: `packages/orchestrator/internal/sandbox/template/cache.go`
- Add focused tests beside the modified packages.

**Interfaces:**
- Produces: `MemoryBackendAuto`, selecting Firecracker `File` for an immutable node-local raw image and current `Uffd` otherwise.

- [ ] Materialize one content-addressed sparse raw memory file per template build.
- [ ] Publish with fsync and atomic rename; never replace a published digest path.
- [ ] Pass `MemBackendType::File` and the shared path for eligible fresh-template restores.
- [ ] Retain UFFD for cache misses, remote-only templates, and individualized paused snapshots.
- [ ] Verify all Firecracker mappings use the same device/inode and writes remain private.

### Task 5: Host MGLRU branch

**Files:**
- Create: `scripts/memory-experiments/host-mglru.sh`
- Modify: `.github/actions/host-init/action.yml`
- Modify: `iac/provider-gcp/nomad/jobs/env.template`

**Interfaces:**
- Consumes: `HOST_MGLRU_ENABLED`, default true only on this branch.
- Produces: fail-closed preflight and a requested `/sys/kernel/mm/lru_gen/enabled` bitmask.

- [ ] Add `enable`, `disable`, `status`, and `metrics` commands.
- [ ] Refuse enable when the sysfs interface is missing.
- [ ] Preserve default reclaim knobs; do not set `min_ttl_ms`.
- [ ] Validate script syntax and run `status` on the target host.

### Task 6: Host zswap branch

**Files:**
- Create: `scripts/memory-experiments/host-zswap.sh`
- Modify: `.github/actions/host-init/action.yml`
- Modify: `iac/provider-gcp/nomad/jobs/env.template`

**Interfaces:**
- Consumes: compressor, zpool, max-pool-percent, and an existing backing swap device.
- Produces: fail-closed zswap configuration with `enable`, `disable`, `status`, and `metrics` commands.

- [ ] Refuse enable when no backing swap or zswap parameters exist.
- [ ] Apply compressor and zpool before enabling zswap.
- [ ] Bound the pool and expose pool/swap counters.
- [ ] Keep backing swap capacity identical in baseline and treatment runs.

### Task 7: Guest DAMON plus FPR branch

**Files:**
- Modify: `packages/orchestrator/internal/cfg/model.go`
- Modify: `packages/orchestrator/internal/sandbox/fc/process.go`
- Modify: `iac/provider-gcp/nomad/jobs/env.template`
- Add focused kernel-argument tests.

**Interfaces:**
- Produces: template-build kernel arguments for DAMON_RECLAIM with `skip_anon=Y`; existing balloon FPR remains enabled.

- [ ] Add typed config for enable, minimum age, quota time, quota bytes, and watermarks.
- [ ] Add exact boot arguments only when enabled.
- [ ] Require template rebuild because resumed snapshots do not reprocess kernel arguments.
- [ ] Verify guest DAMON status and host RSS reduction through FPR.

### Task 8: Isolated KSM branch

**Files:**
- Modify: `packages/orchestrator/cmd/fc-netns-exec/main.go`
- Create: `scripts/memory-experiments/host-ksm.sh`
- Modify: `.github/actions/host-init/action.yml`
- Modify: `iac/provider-gcp/nomad/jobs/env.template`
- Add wrapper unit tests.

**Interfaces:**
- Produces: process-wide `PR_SET_MEMORY_MERGE` for Firecracker only and bounded host ksmd controls.

- [ ] Enable merge policy immediately before `exec` and fail if the kernel rejects it.
- [ ] Enable host KSM only on an explicitly isolated node pool.
- [ ] Set `merge_across_nodes=0` and expose KSM profit/CPU counters.
- [ ] Verify `/proc/<pid>/ksm_stat` after exec; reject the run if openEuler lacks the exec inheritance fix.

### Task 9: Host ETMem branch

**Files:**
- Create: `scripts/memory-experiments/host-etmem.sh`
- Add branch-specific operator instructions to this document.

**Interfaces:**
- Consumes: Firecracker PID, cold-age threshold, scan interval, and swap quota.
- Produces: generated ETMem task configuration with `attach`, `detach`, `status`, and `metrics` commands.

- [ ] Refuse attach when ETMem tools/modules, PID, or backing swap are missing.
- [ ] Generate one task identity per sandbox PID.
- [ ] Remove task state on detach and tolerate an already-exited Firecracker process.
- [ ] Record swapin and runtime p99 before accepting density gains.

### Task 10: Guest zram branch

**Files:**
- Create: `scripts/memory-experiments/guest-zram.sh`
- Integrate the script into template provisioning for this branch only.

**Interfaces:**
- Produces: bounded guest zram swap with `enable`, `disable`, `status`, and `metrics` commands.

- [ ] Refuse enable when zram is unavailable.
- [ ] Set algorithm, memory limit, disksize, and swap priority explicitly.
- [ ] Keep host zswap/zram disabled during this branch's treatment run.
- [ ] Verify guest swap activity, FPR host-RSS reduction, and snapshot size.

### Task 11: Snapshot compression branch

**Files:**
- Modify: `packages/shared/pkg/storage/header`
- Modify: `packages/orchestrator/internal/sandbox/block`
- Add round-trip and corruption tests.

**Interfaces:**
- Produces: versioned LZ4-compressed 4 MiB memory artifact chunks with codec, logical size, compressed size, and checksum.

- [ ] Write failing round-trip, zero-block, mixed-version, and checksum-corruption tests.
- [ ] Implement bounded chunk compression after Firecracker memory export.
- [ ] Decompress once into the existing raw local cache before serving 4 KiB UFFD faults.
- [ ] Keep the uncompressed artifact reader for baseline compatibility.
- [ ] Verify pause/resume behavior and artifact byte reduction.

### Task 12: Branch verification and experiment record

**Files:**
- Modify: `docs/superpowers/plans/2026-09-02-memory-overcommit-ablation.md`

- [ ] Record the baseline commit and each treatment branch commit.
- [ ] Record exact build/test/preflight commands and their observed output.
- [ ] Confirm every treatment branch merge-base is the baseline commit.
- [ ] Confirm no treatment branch contains another treatment's commit.
- [ ] Update the progress table with implemented, verified, or target-host-blocked status; a target-host-blocked branch must still contain complete preflight and operator tooling.
