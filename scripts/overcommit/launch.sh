#!/usr/bin/env bash
# Run on the Linux test host: sudo ./launch.sh /path/to/orchestrator [args...]
# Applies only to this process and its children. Parent cgroup limits must be
# removed in the service/container configuration (see README).
set -euo pipefail
if [[ $# -eq 0 ]]; then
    echo "usage: $0 PROGRAM [ARGS...]" >&2
    exit 2
fi
export E2B_UNRESTRICTED_STRESS=true
# Linux has a finite nr_open ceiling; use that actual kernel ceiling instead of
# a fixed E2B connection count. Do not change system-wide sysctls here.
ulimit -n "$(cat /proc/sys/fs/nr_open)"
ulimit -u unlimited
ulimit -v unlimited
ulimit -l unlimited
exec "$@"
