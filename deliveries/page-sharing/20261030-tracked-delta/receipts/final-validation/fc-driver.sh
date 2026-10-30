#!/bin/sh
set -u
base=/home/coder/projects/yangshengxin/e2b/page-sharing-20261030/tracked-delta-01
old=/home/coder/projects/yangshengxin/e2b/page-sharing-20261030
toolchain=/home/coder/projects/yangshengxin/e2b/validation-20261016/toolchains/rustup/toolchains/1.85.0-aarch64-unknown-linux-gnu
export CARGO_HOME="$old/cargo-home"
export CARGO_TARGET_DIR="$base/cargo-target"
export CARGO_BUILD_JOBS=4
export LIBRARY_PATH=/home/coder/projects/yangshengxin/e2b/validation-20261016/toolchains/lib
export PATH="$toolchain/bin:/usr/local/bin:/usr/bin:/bin"
export RUSTC="$toolchain/bin/rustc"
export RUSTDOC="$toolchain/bin/rustdoc"
[ -d "$base/cargo-target" ] || exit 91
cd "$base/fc" || exit 92
rustfmt --edition 2024 --config skip_children=true src/vmm/src/vstate/vm.rs src/vmm/src/lib.rs src/vmm/src/builder.rs src/vmm/src/persist.rs src/vmm/src/rpc_interface.rs src/vmm/src/vmm_config/instance_info.rs src/vmm/src/devices/virtio/balloon/util.rs src/vmm/src/utils/dirty_tracking.rs src/firecracker/src/api_server/parsed_request.rs > "$base/final-validation/fmt.log" 2>&1
printf 'fmt\t%s\n' "$?" >> "$base/final-validation/fc-exit.tsv"
cargo test --offline --locked -p vmm devices::virtio::balloon::util -- --nocapture > "$base/final-validation/fc-discard.log" 2>&1
printf 'discard\t%s\n' "$?" >> "$base/final-validation/fc-exit.tsv"
cargo test --offline --locked -p vmm utils::dirty_tracking -- --nocapture > "$base/final-validation/fc-bitmap.log" 2>&1
printf 'bitmap\t%s\n' "$?" >> "$base/final-validation/fc-exit.tsv"
cargo test --offline --locked -p vmm devices::virtio::iovec -- --nocapture > "$base/final-validation/fc-iovec.log" 2>&1
printf 'iovec\t%s\n' "$?" >> "$base/final-validation/fc-exit.tsv"
cargo test --offline --locked -p firecracker api_server::parsed_request -- --nocapture > "$base/final-validation/fc-api.log" 2>&1
printf 'api\t%s\n' "$?" >> "$base/final-validation/fc-exit.tsv"
cargo build --release --offline --locked -p firecracker > "$base/final-validation/fc-build.log" 2>&1
printf 'build\t%s\n' "$?" >> "$base/final-validation/fc-exit.tsv"
date -u > "$base/final-validation/fc.done"
