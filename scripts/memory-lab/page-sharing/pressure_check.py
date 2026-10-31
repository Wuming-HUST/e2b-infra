#!/usr/bin/env python3
"""Two shared-NBD VMs under a private bounded cgroup; real swap plus RAM oracles."""
import argparse
import os
from pathlib import Path
import subprocess
import sys
import time

import kvm_check as k
from tracked_delta_check import capture_checkpoint, pageout_one


def cgroup_stats(group):
    return {name: (group / name).read_text() for name in
            ['memory.current', 'memory.swap.current', 'memory.events', 'memory.stat', 'memory.pressure']}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ['firecracker', 'kernel', 'guest-binary', 'nbd-helper', 'output']:
        parser.add_argument('--' + name, type=Path, required=True)
    args = parser.parse_args()
    run = args.output.resolve()
    run.mkdir()
    group = Path('/sys/fs/cgroup') / ('e2b-sharing-pressure-' + str(os.getpid()))
    group.mkdir()
    (group / 'memory.max').write_text(str(640 << 20))
    (group / 'memory.high').write_text(str(192 << 20))
    (group / 'memory.swap.max').write_text(str(512 << 20))
    k.save_json(run / 'cgroup.json', {'path': str(group), 'memory_max': 640 << 20,
                'memory_high': 192 << 20, 'swap_max': 512 << 20, 'before': cgroup_stats(group)})
    k.make_initrd(args.guest_binary, run / 'initrd.gz')
    vms, helpers, pressure = [], [], None
    success = False
    try:
        seed = k.VM(args, 'seed', run)
        vms.append(seed)
        seed.api('PUT', '/machine-config', {'vcpu_count': 1, 'mem_size_mib': 128})
        seed.api('PUT', '/boot-source', {'kernel_image_path': str(args.kernel),
                 'initrd_path': str(run / 'initrd.gz'), 'boot_args': 'console=ttyS0 reboot=k panic=1 pci=off'})
        seed.api('PUT', '/actions', {'action_type': 'InstanceStart'})
        ready = seed.event('ready')
        offsets = [gpa - 0x80000000 for gpa in ready['gpas']]
        seed.api('PATCH', '/vm', {'state': 'Paused'})
        seed.snapshot(run / 'A.state', run / 'A.ram')
        seed.close()
        vms.remove(seed)
        source_hash = k.sha256_file(run / 'A.ram')
        all_results = {}
        for tracked in [False, True]:
            args.tracked_delta = tracked
            mode = 'delta' if tracked else 'full'
            directory = run / (mode + '-source')
            directory.mkdir()
            helper = k.NBDHelper(args.nbd_helper, run / 'A.ram', directory)
            helpers.append(helper)
            pair = []
            for side in ['left', 'right']:
                vm = k.VM(args, mode + '-' + side, run)
                vms.append(vm)
                vm.restore(run / 'A.state', Path(helper.ready['path']))
                (group / 'cgroup.procs').write_text(str(vm.process.pid))
                vm.api('PATCH', '/vm', {'state': 'Resumed'})
                k.check_guest_markers(vm, 4096, [0x31, 0x32, 0x33, 0x34])
                pair.append(vm)
            stop = run / (mode + '-pressure.stop')
            ready_path = run / (mode + '-pressure.ready')
            code = """import mmap,os,time,sys
from pathlib import Path
Path(sys.argv[1]+'/cgroup.procs').write_text(str(os.getpid()))
data=mmap.mmap(-1,192<<20,flags=mmap.MAP_PRIVATE|mmap.MAP_ANONYMOUS)
for i in range(0,len(data),4096):data[i]=0x6d
Path(sys.argv[2]).touch()
while not Path(sys.argv[3]).exists():
 for i in range(0,len(data),4096):data[i]=(data[i]+1)%256
 time.sleep(.05)
"""
            pressure = subprocess.Popen([sys.executable, '-c', code, str(group), str(ready_path), str(stop)])
            k.save_json(run / (mode + '-pressure-pid.json'), {'pid': pressure.pid, 'allocation_bytes': 192 << 20})
            deadline = time.monotonic() + 45
            while not ready_path.exists():
                assert pressure.poll() is None and time.monotonic() < deadline
                time.sleep(.1)
            time.sleep(2)
            checkpoints = []
            latencies = []
            for generation in range(3):
                # Both VMs repeatedly write private bytes while pressure runs.
                for index, vm in enumerate(pair):
                    value = 0x80 + 16 * index + generation
                    for _ in range(20):
                        started = time.monotonic()
                        vm.command('write 1 ' + str(value))
                        latencies.append(time.monotonic() - started)
                    k.check_guest_markers(vm, 4096, [0x31, value, 0x33, 0x34])
                k.record_memory_stats(run, mode + '-active-' + str(generation), pair)
                for index, vm in enumerate(pair):
                    prefix = mode + '-' + str(generation) + '-' + str(index)
                    vm.api('PATCH', '/vm', {'state': 'Paused'})
                    vm.snapshot(run / (prefix + '.state'))
                    regions = vm.mappings()
                    before = k.host_page_entry(vm.process.pid, k.host_address(regions, offsets[1]))
                    if before['swapped'] and not before['present']:
                        swap = {'status': 'swapped_pending_oracles', 'before': before,
                                'logical_offset': offsets[1], 'pid': vm.process.pid,
                                'reason': 'known CPU-written marker already swapped under cgroup pressure'}
                        k.save_json(run / (prefix + '-swap.json'), swap)
                    else:
                        swap = pageout_one(vm, regions, offsets[1], run / (prefix + '-swap.json'))
                    assert swap['status'] == 'swapped_pending_oracles', swap
                    k.record_memory_stats(run, prefix + '-swapped-before-oracle', [vm])
                    ram = run / (prefix + '.ram')
                    if tracked:
                        receipt = capture_checkpoint(vm, regions, ram)
                    else:
                        receipt = k.export_memory(vm, regions, ram)
                        native = run / (prefix + '.native.ram')
                        vm.snapshot(run / (prefix + '.native.state'), native)
                        assert receipt['sha256'] == k.sha256_file(native)
                    value = 0x80 + 16 * index + generation
                    k.check_marker_file(ram, offsets, 4096, [0x31, value, 0x33, 0x34])
                    checkpoints.append({'generation': generation, 'side': index, 'checkpoint': receipt,
                                        'real_private_swap_observed': True, 'cgroup': cgroup_stats(group)})
                if generation < 2:
                    for vm in pair:
                        vm.api('PATCH', '/vm', {'state': 'Resumed'})
            stop.touch()
            assert pressure.wait(timeout=30) == 0
            pressure = None
            pressure_stats = cgroup_stats(group)
            assert int(pressure_stats['memory.events'].split('high ')[1].split()[0]) > 0
            for vm in pair:
                vm.close()
                vms.remove(vm)
            helper.close()
            helpers.remove(helper)
            for index in range(2):
                prefix = mode + '-2-' + str(index)
                restored = k.VM(args, prefix + '-restored', run)
                vms.append(restored)
                restored.restore(run / (prefix + '.state'), run / (prefix + '.ram'))
                restored.api('PATCH', '/vm', {'state': 'Resumed'})
                k.check_guest_markers(restored, 4096, [0x31, 0x82 + 16 * index, 0x33, 0x34])
                restored.close()
                vms.remove(restored)
            ordered = sorted(latencies)
            all_results[mode] = {'checkpoints': checkpoints, 'command_count': len(ordered),
                'command_p99_seconds': ordered[min(len(ordered)-1, int(len(ordered)*.99))],
                'latency_samples_seconds': latencies, 'after_pressure': pressure_stats,
                'both_final_restores_passed': True}
        assert k.sha256_file(run / 'A.ram') == source_hash
        k.save_json(run / 'result.json', {'result': 'passed', 'variants': all_results, 'source_unchanged': True,
                   'scope': 'bounded cgroup correctness; command latency is diagnostic, not a production SLO'})
        success = True
    finally:
        if pressure is not None:
            pressure.terminate()
            pressure.wait(timeout=30)
        for vm in vms:
            vm.close()
        for helper in helpers:
            helper.close()
        remaining = (group / 'cgroup.procs').read_text().strip()
        k.save_json(run / 'cleanup.json', {'success': success, 'remaining_cgroup_processes': remaining,
                    'final': cgroup_stats(group), 'remaining_vms': [vm.process.pid for vm in vms if vm.process.poll() is None]})
        assert not remaining
        group.rmdir()


if __name__ == '__main__':
    main()
