#!/usr/bin/env python3
"""Small KVM candidate-coverage oracle, separate from the E2B Factory test.

Candidate reads happen before the full parent/oracle reads. Reconstructed raw
files are diagnostic artifacts; production E2B header packing is tested by the
Factory integration. No global cache, swap or pressure settings are changed.
"""
import argparse
import ctypes
import os
import platform
import time
from pathlib import Path

import kvm_check as k


def candidate_ranges(response, regions):
    size = sum(r['size'] for r in regions)
    if (response.get('tracking_version') != 1 or response.get('tracking_mode') != 'bitmap'
            or response.get('tracking_complete') is not True or response.get('capture_ready') is not True
            or response.get('logical_size') != size or response.get('page_size') != 4096):
        raise AssertionError('missing complete frozen tracking contract')
    words = response['bitmap']
    pages = size // 4096
    if size % 4096 or len(words) != (pages + 63) // 64:
        raise AssertionError('bitmap geometry differs from RAM')
    if pages % 64 and words[-1] >> (pages % 64):
        raise AssertionError('bitmap tail exceeds RAM')
    ranges = []
    for page in range(pages):
        if not (words[page // 64] >> (page % 64)) & 1:
            continue
        offset = page * 4096
        address = k.host_address(regions, offset)
        if ranges and ranges[-1]['offset'] + ranges[-1]['size'] == offset and ranges[-1]['base_host_virt_addr'] + ranges[-1]['size'] == address:
            ranges[-1]['size'] += 4096
        else:
            ranges.append({'offset': offset, 'size': 4096, 'base_host_virt_addr': address})
    return ranges


def capture_checkpoint(vm, regions, destination):
    response = vm.api('GET', '/memory/dirty')
    ranges = candidate_ranges(response, regions)
    k.save_json(destination.with_suffix('.candidates.json'), response)
    payload = destination.with_suffix('.delta.ram')
    capture = k.export_memory(vm, ranges, payload)
    # A repeated harvest must preserve earlier CPU/VMM writes even when the
    # first ioctl cleared KVM's kernel bitmap. Do this before full RAM reads.
    repeated = vm.api('GET', '/memory/dirty')
    if response != repeated:
        raise AssertionError('repeated frozen harvest changed the cumulative contract/bitmap')
    size = response['logical_size']
    started = time.monotonic()
    with destination.open('xb', buffering=0) as output, vm.parent_backing.open('rb', buffering=0) as parent:
        remaining = size
        while remaining:
            data = parent.read(min(1 << 20, remaining))
            if not data:
                raise AssertionError('parent source ended early')
            view = memoryview(data)
            while view:
                n = output.write(view)
                if not n:
                    raise OSError('parent reconstruction write made no progress')
                view = view[n:]
            remaining -= len(data)
        with payload.open('rb', buffering=0) as changes:
            for r in ranges:
                output.seek(r['offset'])
                remaining = r['size']
                while remaining:
                    data = changes.read(min(1 << 20, remaining))
                    if not data:
                        raise AssertionError('candidate payload ended early')
                    view = memoryview(data)
                    while view:
                        n = output.write(view)
                        if not n:
                            raise OSError('candidate reconstruction write made no progress')
                        view = view[n:]
                    remaining -= len(data)
            if changes.read(1):
                raise AssertionError('candidate payload has unreferenced bytes')
        os.fsync(output.fileno())
    native = destination.with_suffix('.native.ram')
    vm.snapshot(destination.with_suffix('.native.state'), native)
    digest = k.sha256_file(destination)
    if k.sha256_file(native) != digest:
        raise AssertionError('parent + candidate delta differs from native full frozen RAM')
    receipt = {'mode': 'tracked_delta', 'sha256': digest, 'bytes': size, 'payload_sha256': capture['sha256'],
               'candidate_bytes': capture['bytes'], 'candidate_seconds': capture['seconds'],
               'oracle_seconds': time.monotonic() - started, 'native_full_equal': True,
               'repeated_harvest_equal': True, 'pid': vm.process.pid}
    k.save_json(destination.with_suffix('.json'), receipt)
    return receipt


def pageout_one(vm, regions, offset, destination):
    address = k.host_address(regions, offset)
    before = k.host_page_entry(vm.process.pid, address)
    receipt = {'status': 'unaccepted', 'pid': vm.process.pid, 'logical_offset': offset, 'before': before, 'samples': []}
    if not (before['present'] and before['exclusive'] and not before['file_or_shared_anon'] and not before['swapped']):
        receipt['reason'] = 'private anonymous COW precondition absent'
    elif not hasattr(os, 'pidfd_open') or platform.machine() not in ('aarch64', 'arm64', 'x86_64'):
        receipt['reason'] = 'pidfd/syscall ABI unavailable'
    else:
        # Frozen Popen child identity is retained until all reads/teardown end.
        pidfd = os.pidfd_open(vm.process.pid)
        try:
            libc = ctypes.CDLL(None, use_errno=True)
            libc.syscall.restype = ctypes.c_ssize_t
            vector = k.IOVec(address, 4096)
            n = libc.syscall(ctypes.c_long(440), ctypes.c_int(pidfd), ctypes.byref(vector), ctypes.c_size_t(1), ctypes.c_int(0x15), ctypes.c_ulong(0))
            receipt.update(returned_bytes=n, errno=ctypes.get_errno() if n < 0 else 0)
            deadline = time.monotonic() + 2
            while n >= 0:
                sample = k.host_page_entry(vm.process.pid, address)
                receipt['samples'].append(sample)
                if sample['swapped'] and not sample['present']:
                    receipt['status'] = 'swapped_pending_oracles'
                    break
                if time.monotonic() >= deadline:
                    break
                time.sleep(0.05)
        finally:
            os.close(pidfd)
    k.save_json(destination, receipt)
    return receipt


def main():
    p = argparse.ArgumentParser(description=__doc__)
    for name in ('firecracker', 'kernel', 'guest-binary', 'output'):
        p.add_argument('--' + name, type=Path, required=True)
    a = p.parse_args()
    for name in ('firecracker', 'kernel', 'guest_binary'):
        setattr(a, name, getattr(a, name).resolve(strict=True))
    a.tracked_delta = True
    run = a.output.resolve()
    run.mkdir(parents=True, exist_ok=False)
    if os.sysconf('SC_PAGE_SIZE') != 4096:
        p.error('requires 4096-byte host pages')
    k.save_json(run / 'environment.json', {'uname': list(platform.uname()), 'page_size': 4096,
        'fc_sha256': k.sha256_file(a.firecracker), 'kernel_sha256': k.sha256_file(a.kernel), 'guest_sha256': k.sha256_file(a.guest_binary)})
    k.make_initrd(a.guest_binary, run / 'initrd.gz')
    vms, success = [], False
    try:
        initial = k.VM(a, 'initial', run); vms.append(initial)
        initial.api('PUT', '/machine-config', {'vcpu_count': 1, 'mem_size_mib': 128})
        initial.api('PUT', '/boot-source', {'kernel_image_path': str(a.kernel), 'initrd_path': str(run/'initrd.gz'), 'boot_args': 'console=ttyS0 reboot=k panic=1 pci=off'})
        initial.api('PUT', '/actions', {'action_type': 'InstanceStart'})
        ready = initial.event('ready')
        base = 0x80000000 if platform.machine() in ('aarch64', 'arm64') else 0
        offsets = [gpa-base for gpa in ready['gpas']]
        initial.api('PATCH', '/vm', {'state': 'Paused'})
        initial.snapshot(run/'A.state', run/'A.ram')
        initial.close(); vms.remove(initial)
        cold = k.VM(a, 'cold-restore', run); vms.append(cold)
        cold.restore(run/'A.state', run/'A.ram')
        regions = cold.mappings()
        if cold.api('GET', '/memory/dirty').get('capture_ready') is not False:
            raise AssertionError('load alone must not authorize external RAM capture')
        entry = k.host_page_entry(cold.process.pid, k.host_address(regions, offsets[0]))
        if entry['present'] or entry['swapped']:
            raise AssertionError('cold nonzero marker was already faulted into FC')
        k.save_json(run/'cold-marker.json', entry)
        cold.snapshot(run/'B.state')
        response = cold.api('GET', '/memory/dirty')
        page = offsets[0]//4096
        if response['bitmap'][page//64] >> (page%64) & 1:
            raise AssertionError('untouched marker unexpectedly selected; inheritance precondition absent')
        b = capture_checkpoint(cold, regions, run/'B.ram')
        cold.close(); vms.remove(cold)
        changed = k.VM(a, 'cpu-changed', run); vms.append(changed)
        changed.restore(run/'B.state', run/'B.ram')
        changed.api('PATCH', '/vm', {'state': 'Resumed'})
        changed.command('write 1 165'); changed.command('zero 2')
        changed.api('PATCH', '/vm', {'state': 'Paused'})
        changed.snapshot(run/'C.state')
        regions = changed.mappings()
        response = changed.api('GET', '/memory/dirty')
        for offset in offsets[1:3]:
            page = offset//4096
            if not response['bitmap'][page//64] >> (page%64) & 1:
                raise AssertionError('CPU write/zero candidate missing')
        swap = pageout_one(changed, regions, offsets[1], run/'swap.json')
        c = capture_checkpoint(changed, regions, run/'C.ram')
        k.check_marker_file(run/'C.ram', offsets, 4096, [49,165,0,52])
        changed.close(); vms.remove(changed)
        final = k.VM(a, 'final-restore', run); vms.append(final)
        final.restore(run/'C.state', run/'C.ram')
        final.api('PATCH', '/vm', {'state': 'Resumed'})
        k.check_guest_markers(final, 4096, [49,165,0,52])
        final.close(); vms.remove(final)
        if swap['status'] == 'swapped_pending_oracles':
            swap['status'] = 'passed'; swap['native_and_restore_oracles'] = True
            k.save_json(run/'swap.json', swap)
        if not b['candidate_bytes'] < b['bytes'] or not c['candidate_bytes'] < c['bytes']:
            raise AssertionError('fixture must demonstrate partial reads')
        k.save_json(run/'result.json', {'status':'passed', 'cold_inheritance':True, 'cpu_write_zero':True,
            'A_B_C_restore':True, 'B':b, 'C':c, 'swap':swap, 'not_tested':['production E2E','shared NBD in this fixture','device writes in this fixture']})
        success = True
    except BaseException as error:
        k.save_json(run/'failure.json', {'error':repr(error), 'children':[{'pid':vm.process.pid,'name':vm.name} for vm in vms]})
        raise
    finally:
        # This fixture owns no source helper. Only its own Popen children exit.
        for vm in reversed(vms):
            vm.close()
        k.save_json(run/'cleanup.json', {'success':success, 'remaining_children':[vm.process.pid for vm in vms if vm.process.poll() is None]})


if __name__ == '__main__':
    main()
