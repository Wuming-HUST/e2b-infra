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
