#!/bin/sh
# Prepare only this run's guest fixture; never execute the old fixture init.
# Usage: m11_setup.sh GO_BINARY MATCHED_KERNEL_MODULE_ROOT OUTPUT_DIRECTORY
set -eu
if [ "$#" -ne 3 ]; then
    echo 'usage: m11_setup.sh GO_BINARY MATCHED_KERNEL_MODULE_ROOT OUTPUT_DIRECTORY' >&2
    exit 2
fi
go_binary=$1
module_root=$2
fixture_output=$3
if [ -e "$fixture_output" ]; then
    echo "refusing to overwrite fixture directory: $fixture_output" >&2
    exit 1
fi
mkdir -p "$fixture_output/modules"
for module in drivers/virtio/virtio_mmio drivers/block/virtio_blk net/core/failover drivers/net/net_failover drivers/net/virtio_net; do
    cp "$module_root/$module.ko" "$fixture_output/modules/"
done
script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 "$go_binary" build -o "$fixture_output/m11-guest" "$script_dir/m11_guest.go"
sha256sum "$fixture_output"/modules/*.ko "$fixture_output/m11-guest" > "$fixture_output/fixture-sha256.txt"
