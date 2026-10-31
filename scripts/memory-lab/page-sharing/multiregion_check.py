#!/usr/bin/env python3
"""Restore actual adjacent RAM slots; verify CPU writes and full/delta snapshots."""
import argparse
import hashlib
from pathlib import Path
import subprocess

import kvm_check as k
from tracked_delta_check import capture_checkpoint


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ['firecracker', 'kernel', 'guest-binary', 'nbd-helper', 'split-tool', 'output']:
        parser.add_argument('--' + name, required=True, type=Path)
    args = parser.parse_args()
    run = args.output.resolve()
    run.mkdir()
    k.make_initrd(args.guest_binary, run / 'initrd.gz')
    vms, helpers = [], []
    try:
        seed = k.VM(args, 'seed', run)
        vms.append(seed)
        seed.api('PUT', '/machine-config', {'vcpu_count': 2, 'mem_size_mib': 128})
        seed.api('PUT', '/boot-source', {'kernel_image_path': str(args.kernel),
                 'initrd_path': str(run / 'initrd.gz'), 'boot_args': 'console=ttyS0 reboot=k panic=1 pci=off'})
        seed.api('PUT', '/actions', {'action_type': 'InstanceStart'})
        ready = seed.event('ready')
        offsets = [gpa - 0x80000000 for gpa in ready['gpas']]
        seed.api('PATCH', '/vm', {'state': 'Paused'})
        seed.snapshot(run / 'A.single.state', run / 'A.ram')
        seed.close()
        vms.remove(seed)
        # Split immediately before and after a guest marker, so CPU stores and
        # full exporters must cover both sides of a real slot boundary. GPA
        # remains unchanged and logical offsets have no artificial holes.
        cuts = sorted(set([offsets[1], offsets[1] + 4096]))
        assert 0 < cuts[0] < cuts[1] < 128 << 20
        result = subprocess.run([str(args.split_tool), str(run / 'A.single.state'),
                                 str(run / 'A.state')] + [str(n) for n in cuts], check=True, capture_output=True, text=True)
        k.save_json(run / 'split.json', {'cuts': cuts, 'marker_gpas': ready['gpas'], 'tool_output': result.stdout,
                    'input_sha256': k.sha256_file(run / 'A.single.state'), 'output_sha256': k.sha256_file(run / 'A.state'),
                    'gpa_unchanged': True, 'fixture_only': True})
        summary = {}
        for backend in ['file', 'nbd']:
            for tracked in [False, True]:
                name = backend + ('-delta' if tracked else '-full')
                args.tracked_delta = tracked
                helper = None
                backing = run / 'A.ram'
                if backend == 'nbd':
                    directory = run / (name + '-source')
                    directory.mkdir()
                    helper = k.NBDHelper(args.nbd_helper, backing, directory)
                    helpers.append(helper)
                    backing = Path(helper.ready['path'])
                vm = k.VM(args, name, run)
                vms.append(vm)
                vm.restore(run / 'A.state', backing)
                regions = vm.mappings()
                assert len(regions) == 3
                assert [r['offset'] for r in regions] == [0] + cuts
                vm.api('PATCH', '/vm', {'state': 'Resumed'})
                vm.command('write 1 165')
                vm.command('zero 2')
                vm.api('PATCH', '/vm', {'state': 'Paused'})
                state = run / (name + '.state')
                ram = run / (name + '.ram')
                vm.snapshot(state)
                if tracked:
                    receipt = capture_checkpoint(vm, regions, ram)
                    candidates = vm.api('GET', '/memory/dirty')['bitmap']
                    for offset in offsets[1:3]:
                        page = offset // 4096
                        assert candidates[page // 64] >> (page % 64) & 1
                else:
                    receipt = k.export_memory(vm, regions, ram)
                    native = run / (name + '.native.ram')
                    vm.snapshot(run / (name + '.native.state'), native)
                    assert receipt['sha256'] == k.sha256_file(native)
                k.check_marker_file(ram, offsets, 4096, [0x31, 165, 0, 0x34])
                vm.close()
                vms.remove(vm)
                restored = k.VM(args, name + '-restored', run)
                vms.append(restored)
                restored.restore(state, ram)
                assert len(restored.mappings()) == 3
                restored.api('PATCH', '/vm', {'state': 'Resumed'})
                actual = restored.command('read')
                expected = [hashlib.sha256(bytes([v]) * 4096).hexdigest() for v in [0x31, 165, 0, 0x34]]
                assert actual['sha256'] == expected
                restored.close()
                vms.remove(restored)
                if helper:
                    helper.close()
                    helpers.remove(helper)
                summary[name] = {'regions': regions, 'checkpoint': receipt, 'restored': actual}
        k.save_json(run / 'result.json', {'result': 'passed', 'variants': summary, 'real_kvm_regions': 3,
                    'scope': 'ARM64 contiguous GPA slots; test fixture split preserves RAM bytes and guest GPAs'})
    finally:
        for vm in vms:
            vm.close()
        for helper in helpers:
            helper.close()


if __name__ == '__main__':
    main()
