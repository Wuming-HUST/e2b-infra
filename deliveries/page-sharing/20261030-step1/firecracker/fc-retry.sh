#!/bin/sh
set -u
task_root=/home/coder/projects/yangshengxin/e2b/page-sharing-20261030
toolchain=/home/coder/projects/yangshengxin/e2b/validation-20261016/toolchains/rustup/toolchains/1.85.0-aarch64-unknown-linux-gnu
export CARGO_HOME="$task_root/cargo-home"
export CARGO_TARGET_DIR="$task_root/cargo-target"
export CARGO_BUILD_JOBS=4
export LIBRARY_PATH=/home/coder/projects/yangshengxin/e2b/validation-20261016/toolchains/lib
export PATH="$toolchain/bin:/usr/local/bin:/usr/bin:/bin"
export RUSTC="$toolchain/bin/rustc"
export RUSTDOC="$toolchain/bin/rustdoc"
printf '%s\n' "$$" > "$task_root/fc-retry.pid"
cd "$task_root/firecracker" || exit 92
for name in fc-build.log fc-tests.log fc-exit-codes.tsv; do
    if [ ! -e "$task_root/initial-$name" ]; then cp "$task_root/$name" "$task_root/initial-$name" || exit 93; fi
done
rustfmt --check --edition 2024 --config skip_children=true \
    src/vmm/src/device_manager/mmio.rs \
    src/vmm/src/devices/virtio/balloon/device.rs \
    src/vmm/src/devices/virtio/balloon/event_handler.rs \
    src/vmm/src/devices/virtio/rng/device.rs \
    src/vmm/src/devices/virtio/rng/event_handler.rs \
    src/vmm/src/devices/virtio/vsock/device.rs \
    src/vmm/src/devices/virtio/vsock/event_handler.rs \
    src/vmm/src/persist.rs > "$task_root/fc-fmt-changed-files.log" 2>&1
printf 'fmt-changed-files\t%s\n' "$?" > "$task_root/fc-retry-exit-codes.tsv"
cargo test --offline --locked -p vmm test_external_snapshot -- --nocapture > "$task_root/fc-tests.log" 2>&1
printf 'tests\t%s\n' "$?" >> "$task_root/fc-retry-exit-codes.tsv"
cargo build --release --offline --locked -p firecracker > "$task_root/fc-build.log" 2>&1
printf 'build\t%s\n' "$?" >> "$task_root/fc-retry-exit-codes.tsv"
date -u '+%Y-%m-%dT%H:%M:%SZ' > "$task_root/fc-retry.done"
