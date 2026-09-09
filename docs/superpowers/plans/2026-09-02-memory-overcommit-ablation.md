# Memory Overcommit Ablation Plan and Implementation Review

**Review date:** 2026-09-03

**Original target:** ARM64, openEuler 24.04, Linux 6.6, cgroup v2

**2026-09-08 compatibility update:** The existing intranet deployment uses
cgroup v1/hybrid. Keep that layout and its historical experiment records.
Apply the common compatibility fix to both baseline and treatments; use the
[v1/hybrid compatibility runbook](2026-09-08-cgroup-v1-hybrid-compatibility.md)
for current rollout, accounting limits, completed checks and prioritized TODOs.
The frozen commits and v2-only commands below remain the original validation
record, not instructions to migrate/reboot the existing intranet servers.

**Goal:** Establish one correctness control and eight isolated treatment branches,
then measure the maximum sustainable sandbox density at an unchanged runtime and
resume p99 SLO.

This document is both the implementation record and the remaining target-host
runbook. No tests, builds, shell execution, Terraform commands, or linters were
run on the local macOS workstation. User-space validation that does not mutate
the host kernel was run in the ARM/openEuler container described below; live VM,
kernel-treatment, storage-provider, density, and latency claims remain pending.

## Non-Negotiable Controls

- For the existing v1/hybrid environment, record the patched branch SHA and
  apply the same compatibility patch to every arm. Skip ETMem, whose current
  swap bound requires v2. Do not mix unavailable v2 accounting fields with
  process metrics or compare v1 and v2 runs as if only the treatment changed.
- The experiment control is commit `2425bbcfdcf7e6056bfe599c568070a425b5626c`,
  not the moving tip of a documentation branch.
- Every treatment branch starts at that exact commit and contains only one
  treatment.
- Baseline and treatment use the same host image, kernel command line, node
  shape, NUMA policy, guest image, Firecracker binary, workload, regular swap
  capacity, admission limit, warmup, run duration, and SLO.
- HugeTLB remains disabled for all 4 KiB-page treatments. Transparent Huge Pages
  are a separate experiment and must not be changed during this matrix.
- A result is valid only when both runtime p99 and snapshot-resume p99 remain
  within the same predeclared numeric SLO. Raw sandbox count is not a density
  result.
- Any unavailable feature, no-op treatment, failed preflight, OOM kill, or
  unbounded swap/PSI event invalidates that run.
- KSM is allowed only on a node pool externally restricted to one trust domain.
- Stateful host treatments use fresh nodes for A/B runs. Do not rely on toggling
  a production node back to a clean baseline.

## Branch Ledger

The following 2026-09-08 commits add the same v1/hybrid compatibility fix and
its validation/runbook document. All seven code/test files and that document
were compared across the nine branches and are byte-identical. The ARM
pre-validation applies to the common files, not new live treatment results.
Use these patched control/treatment revisions for the existing intranet host:

| Branch | Compatibility commit |
|---|---|
| `mem-overcommit-baseline` | `eae4ba17e454dee4a9429ceb10657b6f8defb3ac` |
| `mem-overcommit-exp-file-page-cache` | `4020e63d9fcac943cfed2ed1ba2ca0d2cdc4fd36` |
| `mem-overcommit-exp-host-mglru` | `560b3778b9aea9542ba9af4a1a3b3552d14e3dfa` |
| `mem-overcommit-exp-host-zswap` | `bb950c0d45702298716ce2ce2da7b8d4f6b96c9b` |
| `mem-overcommit-exp-guest-damon-fpr` | `9e866cc2c4fb2eba20827c5239bef08cbb450350` |
| `mem-overcommit-exp-ksm` | `abe3099d198f04610eb99c9591cda7314b833eb9` |
| `mem-overcommit-exp-host-etmem` | `dad8e700f7e797e2622018cfb9cc2a372c51a348` (ETMem treatment ineligible on v1) |
| `mem-overcommit-exp-guest-zram` | `4f2259d00901ce8c436efce71faa1bf12f7dd51b` |
| `mem-overcommit-exp-snapshot-compression` | `cf8c9f3974c36ed5a9efca9100d7130080cb05ce` |

Original 2026-09-03 treatment ledger (preserved as historical evidence):

| Branch | Final treatment commit | Implementation status | Target-host gate |
|---|---|---|---|
| `mem-overcommit-baseline` | `2425bbcfd` | Implemented; ARM unit/build pre-validated | Live cgroup placement, memory admission, HugeTLB-off |
| `mem-overcommit-exp-file-page-cache` | `270bf8fc8` | Implemented; ARM unit/build pre-validated | Same inode, MAP_PRIVATE isolation, PSS sharing, pause/resume |
| `mem-overcommit-exp-host-mglru` | `85e319821` | Explicit same-code A/B implemented; ARM fake-sysfs pre-validated | Fresh nodes, equal `initial_mask`, distinct verified arm state |
| `mem-overcommit-exp-host-zswap` | `8d3031d6f` | Implemented; ARM syntax/status/fmt pre-validated | Baseline zswap off, backing swap equal, counters move |
| `mem-overcommit-exp-guest-damon-fpr` | `c6043943c` | Implemented; ARM unit/build pre-validated | Guest boot parameters, service gate, FPR RSS reduction |
| `mem-overcommit-exp-ksm` | `cda231986` | Mechanism and fake-sysfs tests pre-validated; net decision external | Trust isolation, prctl inheritance, positive measured net profit |
| `mem-overcommit-exp-host-etmem` | `0bd30ae17` | Implemented; ARM syntax pre-validated | ETMem modules/tools, PID identity, bounded swap |
| `mem-overcommit-exp-guest-zram` | `f1663083c` | Implemented without duplicate logic; ARM unit/build pre-validated | Host exclusion, guest service, zram and FPR counters |
| `mem-overcommit-exp-snapshot-compression` | `741a6e8dd` | Same-code raw/LZ4 implementation; ARM unit/build pre-validated | Round trip, cold/warm resume, actual LZ4 format, SLO |

Full immutable commit IDs:

```text
baseline             2425bbcfdcf7e6056bfe599c568070a425b5626c
file page cache      270bf8fc8fdbc9f6f8a1b60e4941db02faa50257
host MGLRU           85e319821841d5c43d52be1911b6f25b990b1fc7
host zswap           8d3031d6fd3ffd56472e562003692fe7f0707ae8
guest DAMON + FPR    c6043943c1d38ce60bdd04442b7283a4650c110c
isolated KSM         cda2319869a8c3baf560dd0d4ab5792b6217cf93
host ETMem           0bd30ae17bd5cf6411ed33dafad5932c5f9c9461
guest zram           f1663083c95db30a71ce5a0bc68bcaa566cf331b
snapshot compression 741a6e8dd6b5ac3cf18d94a281b827c7cbdc92a4
```

Static history inspection found the merge-base of every treatment against the
baseline branch to be
`2425bbcfdcf7e6056bfe599c568070a425b5626c`. Each treatment log and changed-file
set contains its own mechanism only; no treatment commit from another branch is
present.

## Local Review Evidence

Completed locally without executing target behavior:

- Read every treatment log and changed-file set and recomputed all eight
  merge-bases.
- Statically traced configuration, template compatibility, process startup,
  storage publication, restore, pause, and cleanup paths.
- Inspected the separate Firecracker File/UFFD and balloon FPR implementation;
  its worktree remained unchanged.
- Ran `gofmt` only on the Go files changed for the snapshot raw-control follow-up.
- Ran `git diff --check` on every treatment diff and the final documentation
  worktree; all were clean at the time recorded.

Not executed locally: any Go/Rust test, build or compile; shell syntax or live
script command; Terraform/HCL validation; lint; deployment; VM launch; or
performance workload. All behavioral claims remain gated by the target-host
runbook below.

## ARM Container Pre-Validation Record (2026-09-03)

`coder ssh agent03/agent03` provides an ARM64 Docker workspace that can be used
for native compilation, focused Go tests, shell syntax checks, and fake-sysfs
tests. It is not a valid density or live-kernel treatment host because it lacks
a complete E2B deployment, does not expose all host interfaces, and already has
state that differs from the experiment control.

All remote files for this review are confined to
`/home/coder/projects/yangshengxin/e2b`, with separate `src`, `artifacts`,
`toolchains`, and `logs` directories. An independently installed official Go
1.25.4 Linux/ARM64 toolchain is available under `toolchains/go1.25.4`; its
archive matched the Go download metadata SHA-256
`a68e86d4b72c2c2fecf7dfed667680b6c2a071221bbdb6913cf83ce3f80d9ff0`.
Terraform 1.5.7 Linux/ARM64 is installed under `toolchains/terraform-1.5.7`;
its archive passed the publisher's SHA256SUMS check.

Observed preflight state:

```text
OS                 openEuler 24.03 LTS-SP2 (not the planned 24.04 image)
kernel / arch      6.6.0-95.0.0.99.oe2403sp2.aarch64 / aarch64
cgroup             cgroup v2
kernel config      CGROUPS, MEMCG, USERFAULTFD, SWAP, KSM, ZSWAP,
                   ZSMALLOC, LRU_GEN, DAMON, DAMON_RECLAIM, PSI enabled;
                   ZRAM and VIRTIO_BALLOON built as modules
MGLRU mask         0x0000
zswap              disabled
KSM                run=1 (already enabled, so not a clean KSM control)
regular swap       about 5 GiB total and already materially used
HugeTLB 2 MiB      nr_hugepages=4096, nr_overcommit_hugepages=0
PSI                CONFIG_PSI=y, but /proc/pressure/memory is not exposed
DAMON              module is present, parameter reads are permission-denied
zram               no /sys/block/zram0; modprobe is unavailable in container
ETMem               etmem, etmemd, and module tooling are unavailable
build tools        Go 1.25.4 linux/arm64, GCC 12.3.1, pkg-config 1.9.5
network            goproxy.cn and GitHub reachable; proxy.golang.org times out
```

Consequences for validation:

- Use agent03 to run the focused Go tests and native ARM builds below with
  `GOPROXY=https://goproxy.cn,direct`, plus shell syntax and fake-sysfs tests.
- Do not enable/disable MGLRU, zswap, KSM, DAMON, zram, HugeTLB, or swap on this
  shared Docker host. Even where `sudo -n` succeeds, those operations can alter
  global host state outside the workspace.
- Do not report agent03 measurements as an experiment result. HugeTLB is not
  zero, KSM is already enabled, PSI is hidden, and the OS image differs from the
  declared control.
- Re-run the complete one-time preflight on fresh openEuler 24.04 experiment
  nodes. Require KSM and zswap off in their untouched controls. For MGLRU,
  require fresh explicit control/treatment nodes with equal recorded
  `initial_mask` values.
- Live cgroup-at-birth placement, Firecracker restore/FPR, same-inode CoW/PSS,
  guest services, kernel treatment counters, object-store behavior, density,
  and p99 SLO validation remain for the full deployment servers.

All commands below ran from clean detached commits with `GOTOOLCHAIN=local`,
`GOPROXY=https://goproxy.cn,direct`, and Go caches under the same `e2b`
directory. Logs are retained in
`/home/coder/projects/yangshengxin/e2b/logs/2026-09-03-arm-prevalidation`.

| Scope and tested commit | ARM container validation completed | Result | Still required on a full target host |
|---|---|---|---|
| Baseline `2425bbcfd` | API placement/nodemanager tests; cgroup/fc/sandbox tests; full API and Orchestrator `go build ./...`; host-init Bash syntax | Pass | Process-at-birth cgroup placement, HugeTLB-off, deployed `CanFit` flag, load/SLO |
| File pages `270bf8fc8` | template/fc/sandbox tests; full Orchestrator build | Pass | Same inode, immutable CoW backing, PSS sharing, FPR, pause/resume |
| MGLRU `85e319821` | script and startup syntax; root-only fake-sysfs state machine; real read-only status; Terraform `fmt -check -recursive` | Pass; agent03 reported `enabled_mask=0x0000` and was not changed | Fresh control/treatment nodes, equal recorded `initial_mask`, reclaim and p99 deltas |
| zswap `8d3031d6f` | script/startup syntax; real read-only status; Terraform `fmt -check -recursive` | Pass; agent03 reported `enabled=N` and was not changed | Equal backing swap, live enable, pool/swap movement, CPU/PSI/p99 |
| Guest DAMON `c6043943c` | cfg/fc/metadata/rootfs tests; full Orchestrator build; deploy-script syntax | Pass | Guest kernel parameters/service, DAMON counters, FPR and host PSS effect |
| KSM `cda231986` | wrapper test; full Orchestrator build; script syntax; fake-sysfs unmerge/timeout test | Pass | Trust isolation, live prctl inheritance, saved bytes minus CPU/p99 cost |
| ETMem `0bd30ae17` | script syntax | Pass | All module/tool, process, cgroup, swap, detach, and SLO behavior; ETMem is unavailable on agent03 |
| Guest zram `f1663083c` | cfg/metadata/rootfs tests; full Orchestrator build; proxy/canonical/preflight syntax; read-only host exclusion; fake status through the proxy | Pass | Guest module/service, compression, FPR host reduction, snapshot effect |
| Snapshot compression `741a6e8dd` | shared header/storage tests; block/build/sandbox/copy-build tests; full Orchestrator build | Pass | Deployed local/GCS/Mooncake behavior, corruption injection, cold/warm resume and SLO |

The nine `Pass` rows above are the authoritative completed pre-validation
record. They may be reused without rerunning the same user-space commands when
the exact commit, toolchain, build inputs, and promoted artifacts are unchanged.
They do not close any item in the full-host column.

The original zswap tip `054c50246` failed remote Terraform formatting. The
alignment-only follow-ups ending at `8d3031d6f` fix that review finding; the
final SHA passed syntax, read-only status, and `fmt -check`. Full
`terraform validate` is not credited: provider initialization stalled while
downloading the Cloudflare provider, so this remains a target or connected-CI
check rather than a product-test failure.

Each `coder ssh` connection also reported a client/server version mismatch and
a failed workspace startup script. The repository, pinned toolchains, tests,
and builds remained usable. No pre-validation command enabled or disabled
MGLRU, zswap, KSM, DAMON, zram, HugeTLB, regular swap, or ETMem on the shared
container host. Therefore no command in this section passes a live target-host
gate by itself.

## Engineering Assessment

For `N` concurrently running sandboxes, reason about host memory approximately
as:

```text
M_host(N) = M_control_plane + M_shared_templates
          + N * (M_vmm + M_page_tables + M_private_resident
                 + M_compressed_pool_resident)
          + M_other_host_cache
```

File-backed sharing and KSM reduce the physical terms in this equation. DAMON,
ETMem, zswap, ballooning, and guest zram reduce resident bytes only by adding
CPU, I/O, compressed-pool, or refault costs. Snapshot compression changes
artifact storage and transfer, not this steady-state equation. Report the
effective overcommit ratio only at the SLO-valid density knee as
`N * configured_guest_memory / usable_host_memory`, where usable memory excludes
the predeclared control-plane reserve.

For MGLRU control semantics, Linux v6.6
`Documentation/admin-guide/mm/multigen_lru.rst` defines `0x0001` as the main
switch and `0x0002`/`0x0004` as accessed-bit optimizations. Therefore
`enabled=0x0000` is a real MGLRU-off control rather than merely a tuning change.

| Mechanism | What it can improve | Review conclusion and limits |
|---|---|---|
| Baseline cgroup placement | Correct accounting and containment | `CLONE_INTO_CGROUP` closes the pre-placement accounting race. A configured cgroup manager now fails closed. This is a prerequisite, not an overcommit gain. |
| Baseline memory placement | Avoids selecting an already pressured node | The score uses resident and committed memory and includes pending placements. The hard rejection is active only when the existing BestOfK `CanFit` feature flag is enabled. It conservatively assumes the full new guest allocation may become resident during startup. CPU and memory currently share one `R` ratio; production tuning should separate them. |
| Shared immutable file pages | Directly shares untouched template pages across VMs | This is the strongest low-latency density candidate for cloned templates. The content-addressed sparse file, hard-link publication, read-only mode, and Firecracker File/MAP_PRIVATE backend are coherent. First restores may still use UFFD while materialization runs. Validate cache lifecycle, backing-file immutability, CoW behavior, FPR, and pause/resume remotely. |
| Host MGLRU | Better reclaim decisions under mixed hot/cold pressure | Linux 6.6 documents bit `0x0001` as the MGLRU main switch, so verified `0x0000` is an explicit disabled control and a nonzero mask is treatment. The final branch provisions `control` and `treatment` through the same code, persists the initial mask and arm, and rejects same-node arm switches or mask drift. The experiment is valid only on two fresh nodes with the same recorded `initial_mask`; an initial `0x0007` is acceptable because the control is explicitly changed to `0x0000`. MGLRU improves reclaim policy rather than creating free memory, so it still must prove a density/p99 gain under pressure. |
| Host zswap | Compresses swapped anonymous pages and reduces backing-swap I/O | Useful only under anonymous pressure with active backing swap. It consumes RAM and CPU, so stored pages are not free memory. Require baseline zswap to be disabled before the run; an already-enabled matching configuration is idempotently accepted by the script but is not a valid treatment delta. Do not combine with guest zram in this ablation. |
| Guest DAMON_RECLAIM + FPR | Drops cold guest file cache and returns freed GPAs to the host | `skip_anon=Y` is a cautious choice and suits file-cache-heavy sandboxes, but it cannot reclaim anonymous heaps. All boot parameters, enabled state, `skip_anon=Y`, and balloon FPR are bound into template compatibility, so stale snapshots fail closed. The guest kernel and FPR effect still require target validation. |
| Isolated KSM | Deduplicates identical anonymous pages after guests diverge | Process-wide `PR_SET_MEMORY_MERGE` in the Firecracker wrapper is the right scope and avoids marking the orchestrator. It has CPU cost and a cross-VM side channel, so trust-domain isolation is mandatory. `run=2` now waits for global unmerge, but existing mergeable Firecracker processes retain their prctl policy; drain them before a trust-domain transition. The script's `verify` command is only a mechanism gate (`general_profit > 0` plus prctl inheritance); CPU cost and p99 impact must be evaluated by the experiment result analysis. |
| Host ETMem | Moves cold Firecracker pages to regular swap | Appropriate to evaluate on openEuler because it uses the platform ETMem modules. PID plus process start time protects against PID reuse, and `memory.swap.max` bounds each target. It remains operationally expensive per PID and must prove that scan/swap CPU and refault latency do not erase density gains. |
| Guest zram + FPR | Compresses cold anonymous guest data in guest RAM, freeing other GPAs | Plausible when the compression ratio exceeds zram metadata and pool cost. Configuration is template-bound and the guest service blocks `envd` on failure. Host zswap and host zram are excluded. Validate that guest swap activity actually reduces host PSS/RSS through FPR and does not merely move bytes inside the guest. |
| Snapshot compression | Reduces stored/transferred snapshot bytes | The versioned 4 MiB LZ4 chunks, per-chunk SHA-256, raw-chunk fallback, external manifest, and raw reader compatibility are reasonable. It does not by itself improve steady-state RAM density. The current reader materializes the whole raw image before serving UFFD, so cold-resume latency and local disk amplification are the main risks. |

The treatments answer different questions and must not initially be stacked:

- File-backed sharing and KSM can reduce the physical bytes needed by multiple
  simultaneously running VMs.
- DAMON, ETMem, zswap, and guest zram trade CPU, I/O, or refault latency for a
  smaller resident set under pressure.
- Snapshot compression primarily targets object-store bytes, transfer time, and
  possibly cold-start behavior. Report it on a separate artifact-efficiency
  track unless runtime PSS also changes.

## Firecracker Decision

No additional Firecracker change is required for the implemented matrix. The
reviewed Firecracker worktree at
`/Users/yangshengxin/Documents/Projects/04.Agentic/e2b/firecracker-nashzhou` is
clean on `feat-fpr` at `2160f28ac` and was not modified.

The required capabilities already exist:

- `src/vmm/src/vmm_config/snapshot.rs` exposes
  `MemBackendType::{File,Uffd}`.
- `src/vmm/src/persist.rs` restores a raw snapshot through either the shared
  file backend or UFFD.
- `src/vmm/src/devices/virtio/balloon/persist.rs` reconstructs the FPR queue
  layout from snapshot feature bits.
- `src/vmm/src/devices/virtio/balloon/util.rs` replaces a reported private
  file-backed range with anonymous memory before `MADV_DONTNEED`. This preserves
  guest-zero semantics without modifying the shared backing file.
- Snapshot compression is decoded by the orchestrator before Firecracker sees
  the memory image.
- KSM opt-in is a process policy applied immediately before `exec`, so it belongs
  in `fc-netns-exec` rather than Firecracker.

The existing custom FPR work is therefore a prerequisite and must be validated
on ARM, but there is no new Rust change to test. A Firecracker change would
become justified only for a new direct compressed-memory backend or a missing
balloon control/metric that cannot be expressed through the current API. The
preferred next compressed-snapshot design is still an indexed UFFD reader, which
can remain in the orchestrator.

## Publication and Compatibility Invariants

These rules are mandatory for the snapshot and shared-file experiments:

1. Treat build artifacts as immutable. Never reuse a build ID across raw and
   compressed arms, retries with changed content, DAMON parameter sets, guest
   zram parameter sets, page-size modes, or FPR modes.
2. For snapshot compression, deploy the dual-format reader everywhere first
   with `SNAPSHOT_COMPRESSION_ENABLED=false`. Enable compressed writers only
   after old readers have drained.
3. The data object is published before its memory-artifact manifest, and
   `metadata.json` is the final build discoverability marker. Readers must not
   infer format from payload magic.
4. `compression_requested=true` is not proof of treatment. Incompressible input
   falls back to manifested raw. Accept only runs whose manifest and telemetry
   report `lz4-chunks-v1`.
5. Use different build IDs for the same-code raw and LZ4 arms. This also avoids
   template/build-cache reuse and the create-only destination semantics in
   `copy-build`.
6. GCS copy publication uses
   `gcloud storage cp --if-generation-match 0`. Validate once on the target
   toolchain that a second create of the same destination is rejected.
7. `copy-build` currently supports local paths and `gs://`, not a Mooncake URI.
   Do not route Mooncake migration through it.
8. Mooncake `Put` calls do not accept a Go context, and its blob `Put` ignores
   cancellation. Metadata-last still prevents a successful build marker before
   dependencies complete, but timeout/cancellation latency cannot be guaranteed.
   Exercise failure and timeout behavior against the actual Mooncake deployment.
9. The shared file backend may be used only for fully materialized immutable
   template memory. Individual paused snapshots and materialization misses stay
   on UFFD.

## Measurement Contract

Predeclare the numeric SLO and failure budget before launching load. Capture the
same fields before load, at steady state, at the density knee, and after
pressure:

```text
node:
  MemAvailable, Active/Inactive anon/file, Shmem, Slab
  PSI memory some/full totals and p50/p95/p99 windows
  pgscan, pgsteal, workingset_refault, pgmajfault, pswpin, pswpout

sandbox cgroup:
  memory.current, memory.peak, memory.stat
  memory.events, memory.swap.current, memory.swap.events

Firecracker process:
  RSS, PSS, Shared_Clean, Shared_Dirty, Private_Clean, Private_Dirty
  minor/major faults, CPU time, I/O, FPR reported/freed/fail counters

service:
  create/boot/resume p50/p95/p99, runtime request p50/p95/p99
  error, timeout, OOM, restart, and eviction counts

snapshot:
  logical bytes, stored bytes, actual format, chunk count
  pause time, cold materialization time, cold and warm resume p50/p95/p99
```

Primary density is the largest sustained concurrent sandbox count for which:

- runtime p99 and resume p99 both satisfy the same SLO as control;
- no sandbox or host-control-plane OOM occurs;
- error and timeout rates remain inside the predeclared budget;
- memory PSI `full` and major-fault/swap-in rates do not enter an unbounded
  upward trend; and
- the result holds for at least three independent runs.

Report bootstrap confidence intervals or, at minimum, all repetitions rather
than only the best run. Alternate or randomize control/treatment order, and
recreate nodes between stateful treatments.

## Implementation Checklist

### Baseline

- [x] Atomically place fresh and resumed Firecracker processes with
  `CLONE_INTO_CGROUP`.
- [x] Fail closed when a configured sandbox cgroup cannot be created.
- [x] Add memory-aware placement scoring and optional hard admission using
  resident, committed, and pending memory.
- [x] Gate HugeTLB host initialization and clear stale reservations in 4 KiB
  mode.
- [x] Add focused unit tests.
- [x] Execute focused tests and full API/Orchestrator builds on ARM Linux.
- [ ] Execute live process-at-birth placement and HugeTLB checks.
- [ ] Confirm the BestOfK `CanFit` feature flag is enabled for any run claiming
  hard memory admission.

### Shared File Page Cache

- [x] Materialize a content-addressed immutable sparse raw memory image.
- [x] Publish data with sync plus hard link and publish the build reference
  atomically.
- [x] Select Firecracker File/MAP_PRIVATE only for eligible template restores.
- [x] Retain UFFD for cache misses, builds, and individualized snapshots.
- [x] Add concurrency, corruption, and restore-policy tests.
- [x] Execute focused tests and a full Orchestrator build on ARM Linux.
- [ ] Execute same-inode/CoW/FPR/pause-resume validation.

### Host MGLRU

- [x] Add fail-closed `enable`, `disable`, `status`, and `metrics` operations.
- [x] Record and restore the original mask; leave `min_ttl_ms` unchanged.
- [x] Add explicit `control` (`0x0000`) and nonzero `treatment` provisioning
  modes through the same startup code.
- [x] Persist `initial_mask` and configured arm; reject same-node arm switching
  and mask drift.
- [x] Require every client cluster to declare `host_mglru_mode` as `control` or
  `treatment`; leave build workers `unchanged`.
- [x] Execute syntax, fake-sysfs state-machine, read-only status, and Terraform
  formatting checks on ARM Linux.
- [ ] Provision two fresh arms and require equal recorded `initial_mask` before
  spending time on a load run.

### Host zswap

- [x] Require the sysfs interface and active backing swap.
- [x] Configure compressor, zpool, and bounded pool before enabling.
- [x] Expose zswap, swap, major-fault, and memory-PSI counters.
- [x] Wire the treatment into worker startup.
- [x] Execute syntax, read-only status, and Terraform formatting checks on ARM
  Linux.
- [ ] Confirm the untouched control has zswap disabled and equal regular swap.
- [ ] Execute live configuration, pressure, and SLO validation.

### Guest DAMON + FPR

- [x] Add typed age, quota, and watermark configuration.
- [x] Add exact `DAMON_RECLAIM` boot arguments with `skip_anon=Y`.
- [x] Fail closed when DAMON is requested without balloon FPR.
- [x] Bind all DAMON/FPR state into template and snapshot compatibility.
- [x] Block `envd` startup when the guest kernel reports a mismatch.
- [x] Execute focused tests and a full Orchestrator build on ARM Linux.
- [ ] Rebuild unique templates and validate guest reclaim plus host FPR effect.

### Isolated KSM

- [x] Apply process-wide `PR_SET_MEMORY_MERGE` immediately before Firecracker
  `exec`.
- [x] Require explicit isolation attestation and a non-empty trust-domain label.
- [x] Set `merge_across_nodes=0` and expose benefit plus ksmd CPU metrics.
- [x] Disable with `run=2` and wait until shared/sharing counters reach zero.
- [x] Add wrapper and fake-sysfs tests.
- [x] Execute wrapper/fake-sysfs tests, syntax checks, and a full Orchestrator
  build on ARM Linux.
- [ ] Validate `/proc/<pid>/ksm_stat` inheritance and positive kernel
  `general_profit` as a mechanism gate.
- [ ] Treat `host-ksm.sh verify` only as a mechanism preflight; combine deltas
  in saved bytes, ksmd CPU, throughput, and p99 before accepting the treatment.
- [ ] Document and exercise the drain/unmerge/restart trust-domain transition.

### Host ETMem

- [x] Add `attach`, `detach`, `status`, and `metrics` operations.
- [x] Validate the Firecracker executable and process start time.
- [x] Generate one project per process identity and tolerate exited targets.
- [x] Bound swap with the target cgroup's `memory.swap.max` and restore it only
  when the cgroup identity and applied value still match.
- [x] Execute script syntax checking on ARM Linux.
- [ ] Validate tools/modules, attach/detach, pressure behavior, and SLO remotely.

### Guest zram

- [x] Validate algorithm, size, memory limit, and priority.
- [x] Bake the exact configuration and compatibility marker into each template.
- [x] Fail closed before `envd` if guest zram cannot be configured.
- [x] Add host preflight excluding host zswap and host zram.
- [x] Add unit tests and guest operational tooling.
- [x] Remove the duplicated operator implementation; keep a thin proxy to the
  rootfs canonical script.
- [x] Execute focused tests, full Orchestrator build, syntax, host exclusion,
  and fake proxy status on ARM Linux.
- [ ] Validate guest compression, host FPR reduction, and snapshot impact.

### Snapshot Compression

- [x] Add versioned, bounded 4 MiB LZ4 chunks and per-chunk SHA-256.
- [x] Store incompressible chunks raw and fall back to the whole raw artifact
  when compression has no net saving.
- [x] Add an explicit manifest and retain legacy raw-reader compatibility.
- [x] Materialize verified raw memory before starting UFFD.
- [x] Make cache population singleflight/atomic and publish build metadata last.
- [x] Make `copy-build` format-aware and use create-only GCS copies.
- [x] Add `SNAPSHOT_COMPRESSION_ENABLED`, default true on the treatment branch,
  and a same-code manifested-raw control when false.
- [x] Add round-trip, corruption, cache, publication-order, and disabled-mode
  tests.
- [x] Execute shared/orchestrator focused tests and a full Orchestrator build on
  ARM Linux.
- [ ] Compare distinct raw/LZ4 build IDs on deployed storage and live resumes.
- [ ] Validate local, deployed object store, and Mooncake behavior separately.

## Prioritized Remaining TODO

For the current v1/hybrid deployment, first complete the P0 compatibility
rollout in the linked 2026-09-08 runbook. The original v2 preflight and
process-at-birth placement gates below apply to a separate v2 environment;
they do not require migrating the existing intranet host. The immutable SHAs
below predate the compatibility patch; use patched SHAs for new runs.

Priority meanings:

- `P0`: prerequisite for any scientifically valid density result.
- `P1`: highest expected return and should be evaluated as an isolated treatment.
- `P2`: conditional on workload, security isolation, or platform support.
- `P3`: run only after the relevant isolated treatments pass their gates.

| Order | Priority | TODO | Completion gate |
|---:|---|---|---|
| 1 | P0 | [ ] Freeze the real target definition and complete host/IaC preflight. Resolve the declared openEuler 24.04 versus available 24.03 image explicitly; verify Linux 6.6, ARM64, cgroup v2, 4 KiB mode with zero HugeTLB reservations, equal regular swap, kernel features, and full `terraform validate`. | Control and treatment host manifests are saved and differ only in the declared treatment; unsupported branches are marked `ineligible`. |
| 2 | P0 | [ ] Predeclare the numeric runtime and resume p99 SLO, error/OOM budget, workload mix, warmup, run duration, concurrency ramp, three-run repetition policy, and metrics collection. | A versioned experiment manifest and an empty result table exist before load begins. |
| 3 | P0 | [ ] Deploy baseline `2425bbcfd` and validate fresh-create plus resume process placement from birth, fail-closed cgroup behavior, zero HugeTLB reservations, and deployed BestOfK `CanFit=true`. | Baseline completes three runs without OOM or unbounded PSI/fault growth and meets both p99 SLOs. |
| 4 | P1 | [ ] Run shared file pages `270bf8fc8` first. Validate same backing device/inode, immutable checksum after guest writes, MAP_PRIVATE CoW isolation, sublinear aggregate PSS, `Shared_Clean`, FPR, pause/resume, and UFFD fallback for personalized snapshots. | Three independent runs improve SLO-valid density over baseline with no backing-file mutation or lifecycle regression. |
| 5 | P1 | [ ] Classify the representative workload as file-cache-heavy, compressible-anonymous-heavy, cold-anonymous-heavy, or latency-sensitive using baseline counters. | The next treatment choice and its exact knobs are justified by measured memory composition rather than a global default. |
| 6 | P1 | [ ] For compressible anonymous workloads, evaluate guest zram `f1663083c` and host zswap `8d3031d6f` as separate A/B experiments; never enable both in this matrix. | Guest zram proves FPR-backed host PSS reduction beyond its pool/metadata cost, or zswap proves net bytes saved after charging `pool_total_size`, while p99/PSI gates hold. |
| 7 | P1 | [ ] For file-cache-heavy workloads, evaluate guest DAMON + FPR `c6043943c` with a uniquely rebuilt template. | Boot parameters and service gate match; DAMON and FPR counters move; host PSS falls; p99/PSI gates hold. |
| 8 | P1 | [ ] Evaluate MGLRU `85e319821` on two fresh same-image nodes using explicit `control` and `treatment` modes. | Both arms record the same non-missing `initial_mask`, retain their configured arm/mask without drift, and treatment improves SLO-valid density or is rejected. |
| 9 | P1 | [ ] Run snapshot compression `741a6e8dd` as a separate artifact/resume track using distinct raw and LZ4 build IDs. Exercise local and deployed GCS/Mooncake paths, create-only publication, corruption/truncation, failure/timeout, cold materialization, and warm resume. | Actual manifests prove `raw` versus `lz4-chunks-v1`; storage/transfer savings and resume p99 are reported without claiming unmeasured steady-state RAM gain. |
| 10 | P2 | [ ] Evaluate KSM `cda231986` only after scheduler and network policy attest a single trust domain and the drain/unmerge/restart transition is exercised. | Live prctl inheritance and positive kernel `general_profit` pass, then saved bytes remain positive after ksmd CPU, throughput, and runtime/resume p99 costs. |
| 11 | P2 | [ ] Evaluate ETMem `0bd30ae17` only on an openEuler host with `etmem`, `etmemd`, `etmem_scan`, and `etmem_swap`. | PID identity, cgroup swap bound/restore, attach/detach after exit, cold-page movement, refault/swap cost, PSI, and p99 all pass. |
| 12 | P3 | [ ] Promote accepted mechanisms behind rollout flags, then design a separate factorial experiment for combinations. Start with file-page sharing plus template-aware admission/PSI protection; do not stack guest zram, host zswap, and compressed swap by default. | Each component has an accepted isolated result, rollback path, observability, and a combination result that beats the best single treatment at the same SLO. |

When an item completes, attach the exact commit, image/kernel identity, command
log, result artifact, and failed/passed gate. Update both this table and the
mechanism checklist; do not mark a live item complete from unit or fake-sysfs
evidence.

## Target-Host Validation Runbook

Run this section only on the ARM/openEuler server. Start from a clean worktree.
The commands intentionally use detached immutable commits.
The pure user-space tests and builds recorded above need not be repeated if the
same validated artifacts and commits are promoted; retain the commands for
reproduction or rerun them when the toolchain, image, or source changes.

### 1. One-Time Host Preflight

The following is the original **v2-only** preflight. On existing v1/hybrid
hosts, use the 2026-09-08 runbook instead of requiring or changing cgroup2.

```bash
export E2B_REPO=/path/to/e2b-infra-nashzhou
cd "$E2B_REPO"

test "$(uname -m)" = aarch64
uname -r
cat /etc/os-release
test "$(findmnt -n -o FSTYPE /sys/fs/cgroup)" = cgroup2
cat /proc/swaps
cat /proc/pressure/memory

if [[ -r /proc/config.gz ]]; then
  zgrep -E 'CONFIG_(CGROUPS|MEMCG|USERFAULTFD|SWAP|KSM|ZSWAP|ZRAM|ZSMALLOC|LRU_GEN|DAMON|DAMON_RECLAIM|VIRTIO_BALLOON)=' /proc/config.gz
else
  grep -E 'CONFIG_(CGROUPS|MEMCG|USERFAULTFD|SWAP|KSM|ZSWAP|ZRAM|ZSMALLOC|LRU_GEN|DAMON|DAMON_RECLAIM|VIRTIO_BALLOON)=' "/boot/config-$(uname -r)"
fi

cat /sys/kernel/mm/hugepages/hugepages-2048kB/nr_hugepages
cat /sys/kernel/mm/hugepages/hugepages-2048kB/nr_overcommit_hugepages
findmnt -t hugetlbfs
```

Expected common state is Linux 6.6, cgroup v2, and zero explicit 2 MiB HugeTLB
reservations. Record rather than repair a mismatch; repair would make the
control and treatment environments differ.

### 2. Baseline Build and Focused Tests

```bash
git switch --detach 2425bbcfdcf7e6056bfe599c568070a425b5626c

(cd packages/api && go test ./internal/orchestrator/placement ./internal/orchestrator/nodemanager)
(cd packages/orchestrator && go test ./internal/sandbox/cgroup ./internal/sandbox/fc ./internal/sandbox)
(cd packages/orchestrator && CGO_ENABLED=1 GOOS=linux GOARCH=arm64 go build ./...)
(cd packages/api && CGO_ENABLED=1 GOOS=linux GOARCH=arm64 go build ./...)
bash -n .github/actions/host-init/init-client.sh
```

After one fresh create and one resume, locate each Firecracker PID and confirm it
appears in the intended sandbox `cgroup.procs` from process birth. Confirm the
two HugeTLB counters remain zero. Also verify the deployed feature-flag state
has BestOfK `CanFit=true` before treating memory rejection as active.

### 3. Shared File Page Cache

```bash
git switch --detach 270bf8fc8fdbc9f6f8a1b60e4941db02faa50257
(cd packages/orchestrator && go test ./internal/sandbox/template ./internal/sandbox/fc ./internal/sandbox)
```

Deploy with `FILE_MEMORY_BACKEND_ENABLED=true`,
`FILE_MEMORY_BACKEND_DIR=/orchestrator/template-memory`, and
`HUGE_PAGES_ENABLED=false`. Wait for the `.ref` and digest-named `.raw` file
before measuring; the first restore is allowed to fall back to UFFD.

For at least two Firecracker PIDs from the same build:

```bash
grep -E '[0-9a-f]{64}\.raw' /proc/<pid-a>/maps
grep -E '[0-9a-f]{64}\.raw' /proc/<pid-b>/maps
stat -Lc '%d:%i %s %a %n' /orchestrator/template-memory/<digest>.raw
sha256sum /orchestrator/template-memory/<digest>.raw
cat /proc/<pid-a>/smaps_rollup
cat /proc/<pid-b>/smaps_rollup
```

Require the same device/inode, unchanged backing checksum after guest writes,
sublinear total PSS, increasing `Shared_Clean`, and correctness after pause and
resume. Separately confirm a personalized paused snapshot selects UFFD.

### 4. Host MGLRU

```bash
git switch --detach 85e319821841d5c43d52be1911b6f25b990b1fc7
bash -n scripts/memory-experiments/host-mglru.sh
sudo -E bash scripts/memory-experiments/host-mglru_test.sh
sudo scripts/memory-experiments/host-mglru.sh status
```

Provision two fresh client clusters from the same host image and this same
commit. Set `host_mglru_mode = "control"` on one and
`host_mglru_mode = "treatment"` on the other; the branch intentionally rejects
an omitted or `unchanged` mode for client clusters. Build workers remain
`unchanged`. Startup writes `0x0000` on the control and the configured nonzero
mask on the treatment.

Save `status` from every node. Require `configured_arm=control` and
`enabled_mask=0x0000` in the control pool, and
`configured_arm=treatment` with the requested nonzero mask in the treatment
pool. Most importantly, require the recorded `initial_mask` to be equal across
both pools. If it differs or is unrecorded, discard the run.

For a standalone node outside the Terraform path, configure each fresh node
exactly once:

```bash
# Control node only
sudo scripts/memory-experiments/host-mglru.sh disable

# Treatment node only
sudo env HOST_MGLRU_MASK=0x0007 scripts/memory-experiments/host-mglru.sh enable
sudo scripts/memory-experiments/host-mglru.sh metrics
```

Do not switch arms on one node, use `restore` to manufacture another arm, or set
`min_ttl_ms`. Capture refault, scan/steal, PSI, and p99 deltas.

### 5. Host zswap

```bash
git switch --detach 8d3031d6fd3ffd56472e562003692fe7f0707ae8
bash -n scripts/memory-experiments/host-zswap.sh
cat /proc/swaps
cat /sys/module/zswap/parameters/enabled
sudo scripts/memory-experiments/host-zswap.sh status
```

Reject the A/B run if the untouched control is already enabled. Keep the same
regular swap device/capacity in both arms, mount debugfs if it is part of the
common host image, then:

```bash
sudo env HOST_ZSWAP_COMPRESSOR=lz4 HOST_ZSWAP_ZPOOL=zsmalloc \
  HOST_ZSWAP_MAX_POOL_PERCENT=20 \
  scripts/memory-experiments/host-zswap.sh enable
sudo scripts/memory-experiments/host-zswap.sh metrics
```

Require `stored_pages` and swap counters to move under pressure. Charge
`pool_total_size` as resident memory and report compression, CPU, major faults,
swap-in latency, PSI, and p99.

### 6. Guest DAMON + FPR

```bash
git switch --detach c6043943c1d38ce60bdd04442b7283a4650c110c
(cd packages/orchestrator && go test ./internal/cfg ./internal/sandbox/fc ./internal/template/metadata ./internal/template/build/core/rootfs)
```

Build a new unique template with `GUEST_DAMON_RECLAIM_ENABLED=true` and
`BALLOON_FPR=true`. Do not reuse a baseline or differently tuned build. In the
guest:

```bash
systemctl is-active damon-reclaim-preflight.service
/usr/local/bin/verify-damon-reclaim
cat /proc/cmdline
grep -H . /sys/module/damon_reclaim/parameters/*
```

Use a file-cache-heavy workload first. Require DAMON attempted/reclaimed
counters, guest file-cache reduction, and Firecracker FPR freed bytes to move,
followed by lower host PSS/RSS. An active DAMON module without host reduction is
not a density win.

### 7. Isolated KSM

Do not execute this section until scheduler and network policy prove that the
node contains exactly one trust domain.

```bash
git switch --detach cda2319869a8c3baf560dd0d4ab5792b6217cf93
(cd packages/orchestrator && go test ./cmd/fc-netns-exec)
bash -n scripts/memory-experiments/host-ksm.sh
bash scripts/memory-experiments/host-ksm_test.sh

sudo -E env KSM_ISOLATION_ATTESTED=true KSM_TRUST_DOMAIN=<domain> \
  HOST_KSM_PAGES_TO_SCAN=1000 HOST_KSM_SLEEP_MILLIS=20 \
  scripts/memory-experiments/host-ksm.sh enable
sudo -E env KSM_ISOLATION_ATTESTED=true KSM_TRUST_DOMAIN=<domain> \
  scripts/memory-experiments/host-ksm.sh verify <firecracker-pid>
```

Require `/proc/<pid>/ksm_stat` to report the Firecracker process mergeable and
the script's kernel `general_profit` gate to be positive. This only passes the
mechanism preflight. Separately compare saved bytes, ksmd CPU deltas, throughput,
and runtime/resume p99 against control before calling the treatment profitable.

Trust-domain transition sequence:

1. Stop new placement on the node.
2. Drain and terminate every old Firecracker process carrying
   `PR_SET_MEMORY_MERGE`.
3. Run `host-ksm.sh disable` and wait for both `pages_shared=0` and
   `pages_sharing=0`.
4. Restart workload admission with the new trust-domain environment.
5. Re-enable KSM only after isolation is re-attested.

Host `run=2` unmerges pages but cannot revoke the per-process prctl policy from a
still-running Firecracker process.

### 8. Host ETMem

```bash
git switch --detach 0bd30ae17bd5cf6411ed33dafad5932c5f9c9461
bash -n scripts/memory-experiments/host-etmem.sh
command -v etmem
command -v etmemd
modprobe -n etmem_scan
modprobe -n etmem_swap
cat /proc/swaps

sudo env ETMEM_SWAP_MAX_BYTES=1073741824 \
  scripts/memory-experiments/host-etmem.sh attach <firecracker-pid>
sudo scripts/memory-experiments/host-etmem.sh status <firecracker-pid>
sudo scripts/memory-experiments/host-etmem.sh metrics <firecracker-pid>
sudo scripts/memory-experiments/host-etmem.sh detach <firecracker-pid>
```

Require the ETMem project and both per-PID kernel interfaces, a matching process
start time, an enforced cgroup swap cap, cold-page movement, and bounded
swap-in/refault/p99 cost. Validate detach once after the target has exited.

### 9. Guest zram

```bash
git switch --detach f1663083c95db30a71ce5a0bc68bcaa566cf331b
(cd packages/orchestrator && go test ./internal/cfg ./internal/template/metadata ./internal/template/build/core/rootfs)
bash -n scripts/memory-experiments/guest-zram.sh
bash -n packages/orchestrator/internal/template/build/core/rootfs/files/guest-zram.sh
bash -n scripts/memory-experiments/host-guest-zram-preflight.sh
scripts/memory-experiments/host-guest-zram-preflight.sh
```

Build a new unique template for the exact zram parameter set. In the guest:

```bash
systemctl is-active guest-zram-preflight.service
/usr/local/sbin/e2b-guest-zram status
/usr/local/sbin/e2b-guest-zram metrics
cat /proc/swaps
```

Require guest `pswpin/pswpout` and zram `mm_stat` to move, then demonstrate that
host Firecracker PSS/RSS falls through FPR by more than the compressed zram pool
and metadata cost. Record snapshot logical bytes because the zram pool becomes
guest memory state.

### 10. Snapshot Compression

```bash
git switch --detach 741a6e8dd6b5ac3cf18d94a281b827c7cbdc92a4
(cd packages/shared && go test ./pkg/storage/header ./pkg/storage)
(cd packages/orchestrator && go test ./internal/sandbox/block ./internal/sandbox/build ./internal/sandbox ./cmd/copy-build)
(cd packages/orchestrator && CGO_ENABLED=1 GOOS=linux GOARCH=arm64 go build ./...)
```

Roll out the branch to all readers with
`SNAPSHOT_COMPRESSION_ENABLED=false` first. Validate legacy raw resume and a new
manifested-raw build. Then use two distinct new build IDs with the same workload
and branch:

```text
raw arm: SNAPSHOT_COMPRESSION_ENABLED=false, build ID <raw-unique-id>
LZ4 arm: SNAPSHOT_COMPRESSION_ENABLED=true,  build ID <lz4-unique-id>
```

Require the raw arm's manifest to report `raw` and the treatment to report
`lz4-chunks-v1`. Compare logical/stored bytes, pause time, CPU, temporary/local
disk peak, cold materialization, first resume, and warm resume. Run checksum
corruption and truncated-object tests against the deployed storage provider.

For a GCS deployment, validate the CLI precondition once with disposable object
names:

```bash
gcloud storage cp --if-generation-match 0 <source> gs://<bucket>/<new-object>
if gcloud storage cp --if-generation-match 0 <source> gs://<bucket>/<new-object>; then
  echo "generation-zero create unexpectedly overwrote an object" >&2
  exit 1
fi
```

For Mooncake, build the actual deployment binary with `-tags mooncake`, exercise
raw and LZ4 upload/read/materialization, and inject an unavailable-store or
timeout case. Record that underlying Mooncake writes are not cancellable through
the current Go context; do not claim bounded cancellation until the provider API
supports it.

## Recommended Server Order

To minimize target time:

1. Run the one-time host preflight and baseline focused tests once.
2. Reject unsupported or invalid branches before load: MGLRU arms with unequal
   or unrecorded initial masks, zswap already enabled, missing ETMem, missing
   guest DAMON/zram, or unverified KSM isolation.
3. Test shared file pages first because they offer direct sharing without
   pressure-induced latency.
4. Test guest zram or host zswap next according to whether the workload is
   anonymous-heavy; never enable both in the initial matrix.
5. Test DAMON for file-cache-heavy workloads and ETMem for cold anonymous
   workloads.
6. Test KSM only in the isolated pool.
7. Run snapshot compression as a separate storage/resume experiment using its
   same-code raw control.
8. Run MGLRU last and only with fresh explicit control/treatment nodes and equal
   recorded initial masks.

## Further Density Roadmap

Priority is based on expected sandbox-density benefit, isolation risk, and
implementation cost.

| Priority | Method | Proposed implementation and experiment |
|---|---|---|
| P0 | Productionize immutable shared memory images | Promote the file-page-cache branch behind a rollout flag. Add cache quota/GC, startup reconciliation, fs-verity or an equivalent integrity policy, materialization latency metrics, and per-template shared-page accounting. Compare warm pools of identical templates before considering KSM. |
| P0 | Template-aware admission and placement | Learn p95/p99 resident working set by template, lifecycle phase, and workload class. Use PSS plus startup burst estimates rather than charging every request as fully resident forever. Add separate CPU and memory overcommit ratios, freshness bounds on node metrics, NUMA-local headroom, PSI/refault feedback, and a reserved control-plane margin. |
| P0 | cgroup v2 closed-loop protection | Protect orchestrator/network/storage services with `memory.low`. Use per-sandbox `memory.high` for reclaim/throttling, `memory.max` only as a final containment limit, and `memory.swap.max` for bounded spill. A PSI controller can stop admission and invoke `memory.reclaim` before host OOM. Validate fairness and tail latency. |
| P0 | Reduce the guest/application working set | Remove unused services and packages from templates, lazy-start tools, bound language-runtime heaps/caches, reduce allocator arenas, and return unused pages with `madvise`. Measure per-template PSS after boot and representative idle periods. This often yields density without kernel reclaim complexity. |
| P0 | Idle-sandbox hibernation tier | Snapshot and terminate sandboxes that exceed an evidence-based idle threshold, while keeping hot sandboxes and popular template bases resident. This removes VMM, page-table, and private-resident costs rather than compressing them. Account for snapshot I/O, lost in-guest caches, and cold-resume p99; do not count hibernated instances as immediately runnable capacity. |
| P1 | Proactive balloon control with FPR | FPR is passive: it returns pages only after the guest frees them. Use existing Firecracker balloon updates/stats to reclaim from idle sandboxes with hysteresis, a minimum guest reserve, and rapid deflation on activity. Drive targets from host PSI plus per-template working-set predictions. No Firecracker change is expected unless current metrics prove insufficient. |
| P1 | Shared immutable rootfs/block bases | Ensure sandboxes from one template read the same immutable base image or lower layer and keep writes in bounded per-sandbox CoW layers. Verify same backing identity, page-cache sharing, discard/trim, write isolation, and snapshot compatibility. This complements shared memory snapshots by removing duplicate host page-cache and block-cache residency. |
| P1 | Guest zram plus bounded NVMe swap hierarchy | Let guest zram absorb compressible anonymous cold data, then allow a small, per-cgroup host NVMe swap budget for overflow. Explicitly avoid host zswap double compression. Tune priorities and limits from compression ratio and refault SLO, then test failure behavior when both tiers fill. |
| P1 | Host DAMON-based reclaim | Attach DAMON to selected Firecracker address ranges/cgroups and use a bounded pageout scheme for cold host mappings. This can cover anonymous-heavy guests that `skip_anon=Y` guest DAMON cannot. Compare independently with ETMem; use quotas, PSI stop conditions, and major-fault limits. |
| P1 | Indexed lazy decompression for UFFD | Add a v2 artifact index or sidecar mapping logical chunks to stored offsets. On UFFD faults, fetch and verify only required chunks, singleflight decompression into a bounded content-addressed node cache, and prefetch from recorded fault traces. This removes whole-image materialization from cold resume and can share decompressed chunks across sandboxes without changing Firecracker. |
| P2 | Adaptive treatment selection | Classify templates as code/file-cache-heavy, compressible-anon-heavy, or latency-sensitive. Apply only the mechanism with demonstrated benefit instead of globally stacking reclaim, compression, and deduplication. Feed observed compression, PSS, refault, and PSI data back into placement. |
| P2 | KSM within explicit trust domains | After the isolated experiment, add scheduler-enforced trust-domain labels, node draining, scan-rate control, and benefit/CPU thresholds that automatically disable unprofitable KSM. Shared immutable files remain preferred because they avoid scanning and side-channel exposure. |

Avoid the following as density shortcuts:

- HugeTLB for reclaim-heavy overcommit: it pins coarse pages and works against
  4 KiB reclaim, swapping, and CoW sharing.
- Cross-tenant or globally enabled KSM.
- Raising `vm.overcommit_memory` or guest-visible RAM without admission,
  reclaim, and an OOM safety margin; address-space commitment is not physical
  capacity.
- Unbounded swap, unconditional global `drop_caches`, aggressive
  `min_ttl_ms`, or ballooning below measured working set.
- Guest zram, host zswap, and compressed backing swap enabled together without a
  dedicated factorial experiment; stacked compression can spend more CPU and
  RAM than it saves.
- Combining successful branches before each one independently passes the common
  p99 SLO and density contract.

## Result Record

Fill one row per independent repetition:

| Commit | Build ID | Treatment and exact knobs | Actual feature state | Concurrent sandboxes | Host PSS | PSI full | Major faults | Swap in/out | Runtime p99 | Resume p99 | OOM/errors | Valid |
|---|---|---|---|---:|---:|---:|---:|---:|---:|---:|---:|---|
| | | | | | | | | | | | | |

The final decision for each branch is `accept`, `reject`, or `ineligible`, with
the failed gate recorded. Do not collapse `ineligible` into a zero-effect result.
