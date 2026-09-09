#!/usr/bin/env bash
# Execute on the Linux test host before the experiment. No module unload/reload.
set -euo pipefail
if [[ "${1:-}" != "--apply" ]]; then
    echo "usage: sudo $0 --apply" >&2
    echo "sets vm.overcommit_memory=1 and loads NBD if absent" >&2
    exit 2
fi
printf 'previous vm.overcommit_memory=%s\n' "$(cat /proc/sys/vm/overcommit_memory)"
sysctl -w vm.overcommit_memory=1
if [[ ! -d /sys/module/nbd ]]; then
    # IndexAny creates devices as needed. Zero preallocation is supported.
    modprobe nbd nbds_max=0
fi
printf 'NBD initial devices (not a dynamic allocation cap): %s\n' "$(cat /sys/module/nbd/parameters/nbds_max)"
