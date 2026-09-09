#!/usr/bin/env python3
"""Read-only Linux host/process limits and pressure capture, as JSON.

Run with --pid for each API, Orchestrator, proxy and representative Firecracker.
Repeat alongside load-generator measurements. Unreadable values remain unknown.
"""
import argparse
import json
from pathlib import Path
import platform
import time


def read(path):
    try:
        return Path(path).read_text().strip()
    except OSError as exc:
        return {"unavailable": str(exc)}


def capture(pid):
    proc = Path('/proc') / str(pid)
    result = {"timestamp": time.time(), "kernel": platform.release(), "pid": pid,
              "limits": read(proc / 'limits'), "cgroup": read(proc / 'cgroup'),
              "status": read(proc / 'status'), "smaps_rollup": read(proc / 'smaps_rollup')}
    for name in ['meminfo', 'vmstat', 'pressure/memory', 'pressure/cpu', 'pressure/io',
                 'sys/vm/overcommit_memory', 'sys/vm/overcommit_ratio',
                 'sys/fs/nr_open', 'sys/fs/file-max', 'sys/fs/file-nr',
                 'sys/kernel/pid_max', 'sys/kernel/threads-max',
                 'sys/net/netfilter/nf_conntrack_max', 'sys/net/netfilter/nf_conntrack_count',
                 'sys/net/ipv4/ip_local_port_range']:
        result[name] = read(Path('/proc') / name)
    result['nbd_initial_devices'] = read('/sys/module/nbd/parameters/nbds_max')
    result['nbd_connected_devices'] = len(list(Path('/sys/block').glob('nbd*/pid')))
    result['cgroup_ancestors'] = []
    memberships = result['cgroup']
    if not isinstance(memberships, str):
        return result
    # Resolve controllers using mountinfo, including v1/hybrid and mount roots
    # that represent only a subtree of the host hierarchy.
    mounts = read(proc / 'mountinfo')
    if not isinstance(mounts, str):
        result['mountinfo'] = mounts
        return result
    memberships = [line.split(':', 2) for line in memberships.splitlines()]
    for line in mounts.splitlines():
        before, after = line.split(' - ', 1)
        fields, fs = before.split(), after.split()
        if fs[0] not in ('cgroup', 'cgroup2'):
            continue
        mountroot, mountpoint = fields[3], fields[4]
        controls = set(fs[2].split(','))
        for _, controllers, relative in memberships:
            if fs[0] == 'cgroup2' and controllers:
                continue
            if fs[0] == 'cgroup' and not controls.intersection(controllers.split(',')):
                continue
            # Namespaced '/' resolves to the visible mount root. Ancestors
            # above it are not observable and must be captured on the host.
            relative_path = Path(relative)
            if relative == '/':
                suffix = Path('.')
            else:
                try:
                    suffix = relative_path.relative_to(mountroot)
                except ValueError:
                    continue
            if '..' in suffix.parts:
                result['cgroup_ancestors'].append({'unavailable': 'cgroup path escapes visible mount', 'path': relative})
                continue
            mount = proc / 'root' / mountpoint.lstrip('/')
            current = mount / suffix
            while True:
                values = {"path": str(current), "filesystem": fs[0]}
                for name in ['cpu.max', 'cpu.stat', 'cpu.cfs_quota_us', 'cpu.cfs_period_us',
                             'memory.max', 'memory.high', 'memory.swap.max', 'memory.events',
                             'memory.limit_in_bytes', 'memory.memsw.limit_in_bytes',
                             'memory.failcnt', 'memory.oom_control', 'pids.max', 'pids.events',
                             'cpuset.cpus.effective', 'cpuset.mems.effective',
                             'cpuset.cpus', 'cpuset.mems']:
                    if (current / name).exists():
                        values[name] = read(current / name)
                result['cgroup_ancestors'].append(values)
                if current == mount:
                    break
                current = current.parent
    return result


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--pid', type=int, required=True)
    args = parser.parse_args()
    print(json.dumps(capture(args.pid), ensure_ascii=False, indent=2))
