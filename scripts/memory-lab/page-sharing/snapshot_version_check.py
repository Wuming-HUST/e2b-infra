#!/usr/bin/env python3
"""Boot the actual previous FC binary; migrate its snapshot and reject downgrade."""
import argparse
import copy
import json
from pathlib import Path
import subprocess

import kvm_check as k


def version(binary, run):
    result = subprocess.run([str(binary), '--snapshot-version'], capture_output=True, text=True, check=True)
    value = {'binary': str(binary), 'sha256': k.sha256_file(binary),
             'stdout': result.stdout, 'stderr': result.stderr}
    k.save_json(run, value)
    return value


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ['old-firecracker', 'firecracker', 'kernel', 'guest-binary', 'output']:
        parser.add_argument('--' + name, type=Path, required=True)
    args = parser.parse_args()
    run = args.output.resolve()
    run.mkdir()
    previous = version(args.old_firecracker, run / 'old-binary.json')
    current = version(args.firecracker, run / 'new-binary.json')
    assert previous['stdout'] != current['stdout'], 'Need genuinely different snapshot formats'
    k.make_initrd(args.guest_binary, run / 'initrd.gz')
    old_args = copy.copy(args)
    old_args.firecracker = args.old_firecracker
    live = []
    try:
        seed = k.VM(old_args, 'old-seed', run)
        live.append(seed)
        seed.api('PUT', '/machine-config', {'vcpu_count': 1, 'mem_size_mib': 128})
        seed.api('PUT', '/boot-source', {'kernel_image_path': str(args.kernel),
                 'initrd_path': str(run / 'initrd.gz'), 'boot_args': 'console=ttyS0 reboot=k panic=1 pci=off'})
        seed.api('PUT', '/actions', {'action_type': 'InstanceStart'})
        seed.event('ready')
        seed.command('write 1 119')
        seed.api('PATCH', '/vm', {'state': 'Paused'})
        seed.snapshot(run / 'old.state', run / 'old.ram')
        seed.close()
        live.remove(seed)
        source_hash = k.sha256_file(run / 'old.ram')
        migrated = k.VM(args, 'new-from-old', run)
        live.append(migrated)
        migrated.restore(run / 'old.state', run / 'old.ram')
        migrated.api('PATCH', '/vm', {'state': 'Resumed'})
        k.check_guest_markers(migrated, 4096, [0x31, 119, 0x33, 0x34])
        migrated.command('write 2 153')
        migrated.api('PATCH', '/vm', {'state': 'Paused'})
        migrated.snapshot(run / 'new.state', run / 'new.ram')
        migrated.close()
        live.remove(migrated)
        roundtrip = k.VM(args, 'new-roundtrip', run)
        live.append(roundtrip)
        roundtrip.restore(run / 'new.state', run / 'new.ram')
        roundtrip.api('PATCH', '/vm', {'state': 'Resumed'})
        k.check_guest_markers(roundtrip, 4096, [0x31, 119, 153, 0x34])
        roundtrip.close()
        live.remove(roundtrip)
        reject = k.VM(old_args, 'old-rejects-new', run)
        live.append(reject)
        try:
            reject.restore(run / 'new.state', run / 'new.ram')
        except RuntimeError as error:
            detail = str(error)
            # The API must report a snapshot decode/version error, not a missing input.
            assert any(word in detail.lower() for word in ['version', 'snapshot', 'deserialize']), detail
            assert not any(word in detail.lower() for word in ['no such file', 'permission denied']), detail
        else:
            raise AssertionError('Previous binary accepted a new-format snapshot')
        k.save_json(run / 'downgrade-rejection.json', {'error': detail})
        assert k.sha256_file(run / 'old.ram') == source_hash
        k.save_json(run / 'result.json', {'result': 'passed', 'old_binary': previous,
            'new_binary': current, 'old_snapshot_live_boot': True, 'migration_guest_markers_passed': True,
            'new_snapshot_roundtrip_passed': True, 'downgrade_rejected': True, 'source_unchanged': True})
    finally:
        for vm in live:
            vm.close()
        k.save_json(run / 'cleanup.json', {'remaining_vms': [vm.process.pid for vm in live if vm.process.poll() is None]})


if __name__ == '__main__':
    main()
