# Isolated RAM checkpoint checks

These checks create new 128 MiB Firecracker instances and use their own API
sockets and artifacts. They do not deploy the orchestrator or replay an
application trace. Use a new output directory for each run. Existing guest
kernels and Firecracker binaries are inputs and are never overwritten.

Build on the Linux target or cross compile with the matching architecture:

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o /tmp/page-sharing-guest-init scripts/memory-lab/page-sharing/guest.go
cd packages/orchestrator
CGO_ENABLED=0 go build -o /tmp/page-sharing-source ./cmd/page-sharing-source
```

First run the regular-file M01/M07 mechanism oracle from the repository root:

```sh
sudo -n python3 scripts/memory-lab/page-sharing/kvm_check.py \
  --firecracker /absolute/path/to/patched-firecracker \
  --kernel /absolute/path/to/existing-kernel \
  --guest-binary /tmp/page-sharing-guest-init \
  --output /absolute/path/to/new-run-directory
```

The patched Firecracker must support `/memory/mappings`, File restore, snapshot
creation without `mem_file_path`, and device freezing until resume. The guest
kernel needs initramfs/gzip, procfs/pagemap PFNs, devtmpfs, serial console and KVM
support. Defaults are the arm64 low-RAM base GPA `0x80000000` and `console=ttyS0`;
override `--ram-base-gpa` or `--boot-args` explicitly if the kernel requires it.

M01 requires both an absent guest-RAM host PTE and a cold source marker page.
The runner applies `fsync` and `POSIX_FADV_DONTNEED` solely to its own `A.ram`
inode, verifies actual cache residency with `mincore`, and fails if the
precondition cannot be established. It never drops global caches. B captures
the unexecuted restored state; C captures a private write and write-to-zero.
The exporter streams logical regions through bounded `process_vm_readv` reads,
continues short reads, and hashes exactly the exported bytes. A separate native
FC full dump of the same frozen C checkpoint provides a whole-RAM oracle.

After that succeeds, add `--nbd-helper /tmp/page-sharing-source` to a fresh run.
This adds two native VMs using the same registry-owned read-only NBD device.
It samples visible host PFNs three times before full RAM validation, checks
shared clean pages and private write/zero separation, releases one VM lease
while the other remains usable, then drains only its own device after both VMs
exit. The helper uses kernel `IndexAny` allocation. It never picks an occupied
device number or disconnects an unrelated device. A 4 KiB host and guest page
size and privileged pagemap/NBD access are required for this mode.

Optionally add `--attempt-pageout` with `--nbd-helper` in a fresh run. After
private write/zero, pause and device freeze, the runner opens a pidfd for its
own `nbd-left` child and issues one `process_madvise(MADV_PAGEOUT)` request for
only marker pages 1 and 2. Both must first be exclusive anonymous COW pages.
It changes no system configuration and creates no memory pressure. The syscall
numbers for arm64/amd64 (440) and advice value (21) were verified from
`golang.org/x/sys@v0.40.0/unix` architecture syscall tables and Linux constants.

`M09-pageout.json` records the request and bounded pagemap observations without
reading guest bytes. Permissions, unsupported APIs, or either page failing to
become a swapped PTE produce `unaccepted`, even if the syscall succeeds. Only
after both pages actually swap and the existing exported marker, native
whole-RAM hash, and restored guest marker oracles pass does M09 become
`mechanism_only`. This is a two-page swap correctness check, not a pressure,
capacity, or reclamation benchmark. An unavailable diagnostic leaves the
functional checks running but makes `result.json` and stdout say `unaccepted`;
the process exit code alone is not its acceptance oracle.

`result.json` appears after the functional checks complete. Failures write
`failure.json`, per-process PID receipts, raw serial logs and API responses.
By default the failed run's new VM/helper processes remain available for
inspection; do not restart them on an SSH timeout. `--cleanup-failed` explicitly
stops only that run's children, before releasing its source. Unconfirmed helper
teardown emits `quarantined` and keeps the helper and source FDs alive.

Target KVM results remain separate from Go package tests and orchestrator E2E
acceptance. These fixtures do not test guest block/network completion races,
FPR/balloon, memory pressure, remote object storage, or application traces.
Real swap is tested only when the optional M09 receipt says `mechanism_only`.

The additional `m10_check.py` and `m11_check.py` runners cover actual
balloon/FPR discard and concurrent Async block/TAP receive respectively.
Their fixtures, preconditions and independent oracles are described in
`m10.md` and `m11.md`. They remain separate from application acceptance.

## Real Factory integration

`TestSharingFactoryIntegration` in the sandbox package runs real envd through
the production Factory, Pause, local-storage upload, template cache and restore
paths. It is opt-in, needs root/KVM/NBD/network privileges, and uses only new
run artifacts and its own network slots. Set `E2B_UNRESTRICTED_STRESS=true` to
select kernel-allocated NBD indices (the test also sets it explicitly).

Inputs are absolute paths in `E2B_SHARING_FC`, `E2B_SHARING_KERNEL`,
`E2B_SHARING_ROOTFS`, `E2B_SHARING_INITRD` and `E2B_SHARING_TRACE_FIXTURE`.
`E2B_SHARING_INTEGRATION_DIR` must name a new directory. The optional guest init
path is `E2B_SHARING_INIT` (default `/sbin/sharing-init`). The guest needs real
envd, `/bin/sh`, Python 3 and base64, plus the matching block/network drivers.
The test does not install applications or start platform services.

`prepare_factory_fixture.sh EXISTING_TEMPLATE NEW_DIRECTORY` copies the
existing ARM64 envd image and creates the init script/initrd in that directory.
It targets the recorded Debian `6.1.0-50-arm64` module layout; it is not a
general guest image builder. `prepare_factory_network_deps.sh NEW_DIRECTORY`
can unpack a SHA-pinned openEuler ARM64 iptables RPM into a task directory;
prepend the printed directory to the test's PATH. System packages stay intact.

```sh
# Build once with the Linux target toolchain and dependency caches.
cd packages/orchestrator
go test -c -o /absolute/path/to/sharing-sandbox.test ./internal/sandbox
# Set the explicit inputs above, then run in a fresh directory.
sudo -n --preserve-env=PATH,E2B_SHARING_FC,E2B_SHARING_KERNEL,E2B_SHARING_ROOTFS,E2B_SHARING_INITRD,E2B_SHARING_TRACE_FIXTURE,E2B_SHARING_INTEGRATION_DIR \
  /absolute/path/to/sharing-sandbox.test \
  -test.run '^TestSharingFactoryIntegration$' -test.v -test.timeout 10m
```

Both B0 regular-file RAM and B1 shared-NBD RAM restore the same initial
checkpoint and compare the original source-edit fixture's complete file hashes.
Each mode executes A→B→C through upload and cache restore. Same-freeze native
full RAM dumps provide an independent oracle for the actual Pause export.
B1 also checks two concurrent holders and writable rootfs isolation. These five
original tools do not establish complete original trace compatibility.

## Original source-edit slice

`biofabric-source-edit-slice.json` preserves the complete original arguments of
five BioFabric trace tools: Write steps 50/55 and Edit steps 60/63/65. The source
SHA, original call IDs, excluded steps, and full-file SHA/size oracles are stored
in the fixture. This is a trace-derived file editing check; it does not execute
the excluded compilation commands or establish complete original trace parity.

Run the two Write calls before the checkpoint, verify `after_writes`, pause and
restore, verify the same files again, and then run the three Edit calls and
verify `after_edits`. Preserve absolute paths and UTF-8 bytes. An Edit without
`replace_all` must match exactly once. The included original Read parameters
are optional validation probes; their earlier historical observations describe
different file contents and are not the checkpoint oracle.

```sh
python3 scripts/memory-lab/page-sharing/trace_slice.py verify \
  --fixture scripts/memory-lab/page-sharing/biofabric-source-edit-slice.json
```

This checks fixture and oracle consistency. Add `--source /path/to/agent/trajectory.json`
to verify exact extraction against the pinned original trace. To check actual
downloaded guest files, add `--phase after_writes` or `--phase after_edits` and
`--directory /path/to/downloaded-root`; files must retain their paths below
that directory. The `expected` subcommand writes the independently computed
phase files into a new directory. `extract --source ... --output ...` creates
a fresh fixture and refuses to overwrite an existing file.
