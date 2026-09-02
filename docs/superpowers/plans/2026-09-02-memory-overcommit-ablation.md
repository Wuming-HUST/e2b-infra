# Memory Overcommit Ablation Plan and Implementation Review

**Review date:** 2026-09-02

**Target:** ARM64, openEuler 24.04, Linux 6.6, cgroup v2

**Goal:** Establish one correctness control and eight isolated treatment branches,
then measure the maximum sustainable sandbox density at an unchanged runtime and
resume p99 SLO.

This document is both the implementation record and the remaining target-host
runbook. Local tests, builds, shell syntax checks, Terraform validation, and
linters were intentionally not run during this review. Tests present in the
branches are authored but unexecuted until the ARM/openEuler validation.

## Non-Negotiable Controls

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

| Branch | Final treatment commit | Implementation status | Target-host gate |
|---|---|---|---|
| `mem-overcommit-baseline` | `2425bbcfd` | Implemented and statically reviewed | Build/tests, cgroup placement, memory admission, HugeTLB-off |
| `mem-overcommit-exp-file-page-cache` | `fa6cce8d6` | Implemented and statically reviewed | Same inode, MAP_PRIVATE isolation, PSS sharing, pause/resume |
| `mem-overcommit-exp-host-mglru` | `ba6cebced` | Implemented; eligibility conditional | Untouched baseline mask must differ from requested mask |
| `mem-overcommit-exp-host-zswap` | `054c50246` | Implemented and statically reviewed | Baseline zswap off, backing swap equal, counters move |
| `mem-overcommit-exp-guest-damon-fpr` | `c6043943c` | Implemented and statically reviewed | Guest boot parameters, service gate, FPR RSS reduction |
| `mem-overcommit-exp-ksm` | `cda231986` | Implemented and statically reviewed | Trust isolation, prctl inheritance, positive net profit |
| `mem-overcommit-exp-host-etmem` | `0bd30ae17` | Implemented and statically reviewed | ETMem modules/tools, PID identity, bounded swap |
| `mem-overcommit-exp-guest-zram` | `cfa4cdb93` | Implemented and statically reviewed | Host exclusion, guest service, zram and FPR counters |
| `mem-overcommit-exp-snapshot-compression` | `f4992438d` | Implemented with same-code raw control | Round trip, cold/warm resume, actual LZ4 format, SLO |

Full immutable commit IDs:

```text
baseline             2425bbcfdcf7e6056bfe599c568070a425b5626c
file page cache      fa6cce8d6ee9877ea5aa8d6b028dcdc2ba80c6f8
host MGLRU           ba6cebced758938a5aa3c2720a07d94ed553ed94
host zswap           054c502463f138828911fc2ed1a9871ff287cba8
guest DAMON + FPR    c6043943c1d38ce60bdd04442b7283a4650c110c
isolated KSM         cda2319869a8c3baf560dd0d4ab5792b6217cf93
host ETMem           0bd30ae17bd5cf6411ed33dafad5932c5f9c9461
guest zram           cfa4cdb93b9c6550294aba22eca5228f93682158
snapshot compression f4992438d1639e3002673f68a84ba1e2c8c02686
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

## Engineering Assessment

| Mechanism | What it can improve | Review conclusion and limits |
|---|---|---|
| Baseline cgroup placement | Correct accounting and containment | `CLONE_INTO_CGROUP` closes the pre-placement accounting race. A configured cgroup manager now fails closed. This is a prerequisite, not an overcommit gain. |
| Baseline memory placement | Avoids selecting an already pressured node | The score uses resident and committed memory and includes pending placements. The hard rejection is active only when the existing BestOfK `CanFit` feature flag is enabled. It conservatively assumes the full new guest allocation may become resident during startup. CPU and memory currently share one `R` ratio; production tuning should separate them. |
| Shared immutable file pages | Directly shares untouched template pages across VMs | This is the strongest low-latency density candidate for cloned templates. The content-addressed sparse file, hard-link publication, read-only mode, and Firecracker File/MAP_PRIVATE backend are coherent. First restores may still use UFFD while materialization runs. Validate cache lifecycle, backing-file immutability, CoW behavior, FPR, and pause/resume remotely. |
| Host MGLRU | Better reclaim decisions under mixed hot/cold pressure | Technically sound tooling, but Linux 6.6 commonly already reports `0x0007`. The script correctly refuses an enable operation with no baseline delta. If the untouched host is already `0x0007`, this experiment is invalid and should be skipped or rerun with both node pools booted from an explicit MGLRU-off control image. |
| Host zswap | Compresses swapped anonymous pages and reduces backing-swap I/O | Useful only under anonymous pressure with active backing swap. It consumes RAM and CPU, so stored pages are not free memory. Require baseline zswap to be disabled before the run; an already-enabled matching configuration is idempotently accepted by the script but is not a valid treatment delta. Do not combine with guest zram in this ablation. |
| Guest DAMON_RECLAIM + FPR | Drops cold guest file cache and returns freed GPAs to the host | `skip_anon=Y` is a cautious choice and suits file-cache-heavy sandboxes, but it cannot reclaim anonymous heaps. All boot parameters, enabled state, `skip_anon=Y`, and balloon FPR are bound into template compatibility, so stale snapshots fail closed. The guest kernel and FPR effect still require target validation. |
| Isolated KSM | Deduplicates identical anonymous pages after guests diverge | Process-wide `PR_SET_MEMORY_MERGE` in the Firecracker wrapper is the right scope and avoids marking the orchestrator. It has CPU cost and a cross-VM side channel, so trust-domain isolation is mandatory. `run=2` now waits for global unmerge, but existing mergeable Firecracker processes retain their prctl policy; drain them before a trust-domain transition. |
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
- [ ] Execute the baseline tests/build and live placement checks on ARM Linux.
- [ ] Confirm the BestOfK `CanFit` feature flag is enabled for any run claiming
  hard memory admission.

### Shared File Page Cache

- [x] Materialize a content-addressed immutable sparse raw memory image.
- [x] Publish data with sync plus hard link and publish the build reference
  atomically.
- [x] Select Firecracker File/MAP_PRIVATE only for eligible template restores.
- [x] Retain UFFD for cache misses, builds, and individualized snapshots.
- [x] Add concurrency, corruption, and restore-policy tests.
- [ ] Execute tests and same-inode/CoW/FPR/pause-resume validation on ARM Linux.

### Host MGLRU

- [x] Add fail-closed `enable`, `disable`, `status`, and `metrics` operations.
- [x] Record and restore the original mask; leave `min_ttl_ms` unchanged.
- [x] Reject a treatment whose requested mask already equals the recorded
  baseline.
- [ ] Check the untouched target mask before spending time on a load run.
- [ ] If needed, provision both arms from an explicit MGLRU-off boot image.

### Host zswap

- [x] Require the sysfs interface and active backing swap.
- [x] Configure compressor, zpool, and bounded pool before enabling.
- [x] Expose zswap, swap, major-fault, and memory-PSI counters.
- [x] Wire the treatment into worker startup.
- [ ] Confirm the untouched control has zswap disabled and equal regular swap.
- [ ] Execute syntax, live configuration, pressure, and SLO validation.

### Guest DAMON + FPR

- [x] Add typed age, quota, and watermark configuration.
- [x] Add exact `DAMON_RECLAIM` boot arguments with `skip_anon=Y`.
- [x] Fail closed when DAMON is requested without balloon FPR.
- [x] Bind all DAMON/FPR state into template and snapshot compatibility.
- [x] Block `envd` startup when the guest kernel reports a mismatch.
- [ ] Execute tests, rebuild unique templates, and validate reclaim plus FPR.

### Isolated KSM

- [x] Apply process-wide `PR_SET_MEMORY_MERGE` immediately before Firecracker
  `exec`.
- [x] Require explicit isolation attestation and a non-empty trust-domain label.
- [x] Set `merge_across_nodes=0` and expose benefit plus ksmd CPU metrics.
- [x] Disable with `run=2` and wait until shared/sharing counters reach zero.
- [x] Add wrapper and fake-sysfs tests.
- [ ] Validate `/proc/<pid>/ksm_stat` inheritance and positive net profit.
- [ ] Document and exercise the drain/unmerge/restart trust-domain transition.

### Host ETMem

- [x] Add `attach`, `detach`, `status`, and `metrics` operations.
- [x] Validate the Firecracker executable and process start time.
- [x] Generate one project per process identity and tolerate exited targets.
- [x] Bound swap with the target cgroup's `memory.swap.max` and restore it only
  when the cgroup identity and applied value still match.
- [ ] Validate tools/modules, attach/detach, pressure behavior, and SLO remotely.

### Guest zram

- [x] Validate algorithm, size, memory limit, and priority.
- [x] Bake the exact configuration and compatibility marker into each template.
- [x] Fail closed before `envd` if guest zram cannot be configured.
- [x] Add host preflight excluding host zswap and host zram.
- [x] Add unit tests and guest operational tooling.
- [ ] Execute tests and validate guest compression, host FPR reduction, and
  snapshot impact.

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
- [ ] Execute the target test matrix and compare distinct raw/LZ4 build IDs.
- [ ] Validate local, deployed object store, and Mooncake behavior separately.

## Target-Host Validation Runbook

Run this section only on the ARM/openEuler server. Start from a clean worktree.
The commands intentionally use detached immutable commits.

### 1. One-Time Host Preflight

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
(cd packages/orchestrator && CGO_ENABLED=1 GOOS=linux GOARCH=arm64 go build .)
(cd packages/api && CGO_ENABLED=1 GOOS=linux GOARCH=arm64 go build .)
bash -n .github/actions/host-init/init-client.sh
```

After one fresh create and one resume, locate each Firecracker PID and confirm it
appears in the intended sandbox `cgroup.procs` from process birth. Confirm the
two HugeTLB counters remain zero. Also verify the deployed feature-flag state
has BestOfK `CanFit=true` before treating memory rejection as active.

### 3. Shared File Page Cache

```bash
git switch --detach fa6cce8d6ee9877ea5aa8d6b028dcdc2ba80c6f8
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
git switch --detach ba6cebced758938a5aa3c2720a07d94ed553ed94
bash -n scripts/memory-experiments/host-mglru.sh
sudo scripts/memory-experiments/host-mglru.sh status
```

Read the untouched mask before enable. If it is already `0x0007` and the
requested mask is `0x0007`, stop: there is no treatment. For a valid off-to-on
pair, both node pools must boot with the same explicit off state, after which
only the treatment runs:

```bash
sudo env HOST_MGLRU_MASK=0x0007 scripts/memory-experiments/host-mglru.sh enable
sudo scripts/memory-experiments/host-mglru.sh metrics
```

Do not set `min_ttl_ms`. Capture refault, scan/steal, PSI, and p99 deltas.

### 5. Host zswap

```bash
git switch --detach 054c502463f138828911fc2ed1a9871ff287cba8
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
`general_profit` to be positive after subtracting ksmd CPU and latency cost.

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
git switch --detach cfa4cdb93b9c6550294aba22eca5228f93682158
(cd packages/orchestrator && go test ./internal/cfg ./internal/template/metadata ./internal/template/build/core/rootfs)
bash -n scripts/memory-experiments/guest-zram.sh
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
git switch --detach f4992438d1639e3002673f68a84ba1e2c8c02686
(cd packages/shared && go test ./pkg/storage/header ./pkg/storage)
(cd packages/orchestrator && go test ./internal/sandbox/block ./internal/sandbox/build ./internal/sandbox ./cmd/copy-build)
(cd packages/orchestrator && CGO_ENABLED=1 GOOS=linux GOARCH=arm64 go build .)
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
2. Reject unsupported/no-op branches before load: MGLRU with equal mask, zswap
   already enabled, missing ETMem, missing guest DAMON/zram, or unverified KSM
   isolation.
3. Test shared file pages first because they offer direct sharing without
   pressure-induced latency.
4. Test guest zram or host zswap next according to whether the workload is
   anonymous-heavy; never enable both in the initial matrix.
5. Test DAMON for file-cache-heavy workloads and ETMem for cold anonymous
   workloads.
6. Test KSM only in the isolated pool.
7. Run snapshot compression as a separate storage/resume experiment using its
   same-code raw control.
8. Run MGLRU last and only if a real mask delta can be established.

## Further Density Roadmap

Priority is based on expected sandbox-density benefit, isolation risk, and
implementation cost.

| Priority | Method | Proposed implementation and experiment |
|---|---|---|
| P0 | Productionize immutable shared memory images | Promote the file-page-cache branch behind a rollout flag. Add cache quota/GC, startup reconciliation, fs-verity or an equivalent integrity policy, materialization latency metrics, and per-template shared-page accounting. Compare warm pools of identical templates before considering KSM. |
| P0 | Template-aware admission and placement | Learn p95/p99 resident working set by template, lifecycle phase, and workload class. Use PSS plus startup burst estimates rather than charging every request as fully resident forever. Add separate CPU and memory overcommit ratios, freshness bounds on node metrics, NUMA-local headroom, PSI/refault feedback, and a reserved control-plane margin. |
| P0 | cgroup v2 closed-loop protection | Protect orchestrator/network/storage services with `memory.low`. Use per-sandbox `memory.high` for reclaim/throttling, `memory.max` only as a final containment limit, and `memory.swap.max` for bounded spill. A PSI controller can stop admission and invoke `memory.reclaim` before host OOM. Validate fairness and tail latency. |
| P0 | Reduce the guest/application working set | Remove unused services and packages from templates, lazy-start tools, bound language-runtime heaps/caches, reduce allocator arenas, and return unused pages with `madvise`. Measure per-template PSS after boot and representative idle periods. This often yields density without kernel reclaim complexity. |
| P1 | Proactive balloon control with FPR | FPR is passive: it returns pages only after the guest frees them. Use existing Firecracker balloon updates/stats to reclaim from idle sandboxes with hysteresis, a minimum guest reserve, and rapid deflation on activity. Drive targets from host PSI plus per-template working-set predictions. No Firecracker change is expected unless current metrics prove insufficient. |
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
