#!/usr/bin/env python3
"""M10 actual guest balloon discard, source isolation and checkpoint oracle."""
import argparse
import gzip
import hashlib
import json
import os
import platform
import select
import signal
import time
from pathlib import Path
import kvm_check as k


def wait_balloon(vm, target, path):
    """Require actual guest allocation, not merely the host requested target."""
    samples = []
    deadline = time.monotonic() + 30
    while True:
        stats = vm.api('GET', '/balloon/statistics')
        samples.append(stats)
        if stats['actual_mib'] == target:
            k.save_json(path, samples)
            return
        if time.monotonic() >= deadline:
            k.save_json(path, samples)
            raise TimeoutError(f'balloon did not reach {target} MiB')
        time.sleep(0.2)


def checkpoint(vm, regions, run, name):
    if vm.track_dirty:
        from tracked_delta_check import capture_checkpoint
        return capture_checkpoint(vm, regions, run / (name + '.ram'))
    receipt = k.export_memory(vm, regions, run / (name + '.ram'))
    vm.snapshot(run / (name + '.native.state'), run / (name + '.native.ram'))
    if k.sha256_file(run / (name + '.native.ram')) != receipt['sha256']:
        raise AssertionError(f'{name}: native oracle mismatch')
    return receipt


def configure_metrics(vm):
    path = vm.directory / 'metrics.jsonl'
    path.touch(exist_ok=False)
    vm.api('PUT', '/metrics', {'metrics_path': str(path)})


def remapped_offsets(vm, regions, candidates):
    """Observe MAP_PRIVATE backing replaced by balloon/FPR anonymous mapping."""
    anonymous = []
    for line in Path(f'/proc/{vm.process.pid}/maps').read_text().splitlines():
        fields = line.split()
        bounds = [int(v, 16) for v in fields[0].split('-')]
        if len(fields) < 6 or fields[5].startswith('[anon'):
            anonymous.append(bounds)
    return [off for off in candidates if any((lo <= k.host_address(regions, off) < hi for lo, hi in anonymous))]


def cleanup_resources(vms, helpers, all_vms, all_helpers, run, success, requested):
    """Never drain RAM source while a mapping user's exit is uncertain.

    A close error does not prevent checking the other owned children. Polling
    our Popen objects confirms exit without signalling or retrying unknown VMs.
    A helper remains live, with its references and source, if any VM may map it.
    """
    errors = []
    if requested:
        for vm in reversed(vms):
            try:
                vm.close()
            except Exception as error:
                errors.append({'kind': 'vm', 'pid': vm.process.pid, 'error': repr(error)})
    known_pids = {vm.process.pid for vm in all_vms}
    unknown_receipts = []
    # A constructor can write its PID before failing to return a VM object.
    # Its receipt is ours, but lack of a Popen object means exit is unconfirmed.
    for path in run.glob('*/pid.json'):
        if path.parent.name == 'nbd-helper':
            continue
        record = json.loads(path.read_text())
        if record['pid'] not in known_pids:
            unknown_receipts.append({'receipt': str(path), 'pid': record['pid']})
    vm_status = []
    for vm in all_vms:
        try:
            status = vm.process.poll()
            vm_status.append({'pid': vm.process.pid, 'returncode': status, 'exit_confirmed': status is not None})
        except Exception as error:
            vm_status.append({'pid': vm.process.pid, 'returncode': None, 'exit_confirmed': False})
            errors.append({'kind': 'vm-poll', 'pid': vm.process.pid, 'error': repr(error)})
    all_exited = not unknown_receipts and all(row['exit_confirmed'] for row in vm_status)
    if requested and all_exited:
        for helper in reversed(helpers):
            try:
                helper.close()
            except Exception as error:
                errors.append({'kind': 'helper', 'pid': helper.process.pid, 'error': repr(error)})
    helper_status = [{'pid': helper.process.pid, 'returncode': helper.process.poll()} for helper in all_helpers]
    receipt = {'success': success and not errors and all_exited,
               'cleanup_requested': requested, 'all_vm_exits_confirmed': all_exited,
               'quarantined': (not all_exited or bool(errors)) and any(row['returncode'] is None for row in helper_status),
               'vm_status': vm_status, 'helper_status': helper_status,
               'unknown_vm_receipts': unknown_receipts, 'errors': errors,
               'vm_pids': [row['pid'] for row in vm_status],
               'vm_returncodes': [row['returncode'] for row in vm_status],
               'helper_pids': [row['pid'] for row in helper_status],
               'helper_returncodes': [row['returncode'] for row in helper_status]}
    k.save_json(run / 'cleanup.json', receipt)
    if receipt['quarantined']:
        k.save_json(run / 'quarantine.json', {
            'status': 'quarantined', 'runner_pid': os.getpid(),
            'reason': 'VM exit or helper teardown unconfirmed; helper/source retained without disconnect or reuse',
            'cleanup': receipt,
            'action': 'Inspect original PID receipts; stop only this run mapping users before helper/runner teardown'})
    if errors:
        k.save_json(run / 'result.json', {'status': 'failed', 'reason': 'cleanup error', 'cleanup': receipt})
    return receipt


def retain_quarantined_runner(receipt, helpers, run):
    """Retain PIPE readers so an emit cannot kill a live source with SIGPIPE.

    This is intentionally operator-managed: no retry, signal, disconnect or
    device reuse follows an unknown mapping state. SIGINT/SIGTERM are recorded
    without dropping the source's transport; investigate the PID receipts first.
    """
    def interrupted(signum, _frame):
        try:
            k.save_json(run / 'quarantine-signal.json', {'runner_pid': os.getpid(), 'signal': signum,
                        'reason': 'mapping users unconfirmed; source remains held'})
        except OSError:
            pass  # Evidence I/O failure must not drop a live source PIPE.
    signal.signal(signal.SIGTERM, interrupted)
    signal.signal(signal.SIGINT, interrupted)
    while True:
        pipes = {helper.process.stdout: helper for helper in helpers
                 if helper.process.poll() is None and not helper.process.stdout.closed}
        if not pipes:
            time.sleep(0.2)
            continue
        ready, _, _ = select.select(list(pipes), [], [], 0.2)
        for stream in ready:
            try:
                data = os.read(stream.fileno(), 65536)
            except OSError:
                continue
            if data:
                try:
                    pipes[stream].stdout_log.write(data)
                except OSError:
                    # A full evidence disk must not drop the live source PIPE.
                    pass
            else:
                # EOF is evidence, not authorization to release any backing.
                stream.close()


def main():
    p = argparse.ArgumentParser(description=__doc__)
    for name in ('firecracker', 'kernel', 'guest-binary', 'nbd-helper', 'output'):
        p.add_argument('--' + name, required=True, type=Path)
    p.add_argument('--module-dir', required=True, type=Path)
    p.add_argument('--free-page-reporting', action='store_true', help='exercise natural FPR with balloon target held at zero')
    p.add_argument('--probe-only', action='store_true')
    p.add_argument('--cleanup-failed', action='store_true')
    p.add_argument('--tracked-delta', action='store_true', help='select KVM/VMM candidates before reading RAM; compare reconstructed delta with native full RAM')
    a = p.parse_args()
    for n in ('firecracker', 'kernel', 'guest_binary', 'nbd_helper'):
        setattr(a, n, getattr(a, n).resolve(strict=True))
    if os.sysconf('SC_PAGE_SIZE') != 4096:
        p.error('M10 fixture requires 4096-byte host pages')
    a.module_dir = a.module_dir.resolve(strict=True)
    run = a.output.resolve()
    run.mkdir(parents=True, exist_ok=False)
    vms = []
    helpers = []
    all_vms = []
    all_helpers = []
    success = False
    k.save_json(run / 'environment.json', {'uname': list(platform.uname()), 'host_page_size': os.sysconf('SC_PAGE_SIZE'), 'fc_sha256': k.sha256_file(a.firecracker), 'kernel_sha256': k.sha256_file(a.kernel), 'guest_sha256': k.sha256_file(a.guest_binary)})
    k.make_initrd(a.guest_binary, run / 'guest-initrd.gz')
    # Linux accepts concatenated newc archives inside one gzip stream. Only
    # matching guest modules are copied into this new fixture.
    data = gzip.decompress((run / 'guest-initrd.gz').read_bytes())
    data = bytearray(data)
    module_hashes = {}
    for index, name in enumerate(('virtio_mmio', 'virtio_balloon', 'TRAILER!!!'), 100):
        content = b'' if name == 'TRAILER!!!' else (a.module_dir / (name + '.ko')).read_bytes()
        if content:
            module_hashes[name] = hashlib.sha256(content).hexdigest()
        filename = name if name == 'TRAILER!!!' else name + '.ko'
        encoded = filename.encode() + b'\x00'
        fields = [index, 0o100644, 0, 0, 1, 0, len(content), 0, 0, 0, 0, len(encoded), 0]
        data.extend(b'070701' + b''.join((f'{v:08x}'.encode() for v in fields)))
        data.extend(encoded)
        data.extend(b'\x00' * (-len(data) % 4))
        data.extend(content)
        data.extend(b'\x00' * (-len(data) % 4))
    with gzip.open(run / 'guest-initrd.gz', 'wb') as stream:
        stream.write(data)
    k.save_json(run / 'module-hashes.json', module_hashes)
    try:
        initial = k.VM(a, 'initial', run)
        vms.append(initial)
        all_vms.append(initial)
        configure_metrics(initial)
        initial.api('PUT', '/machine-config', {'vcpu_count': 1, 'mem_size_mib': 192})
        initial.api('PUT', '/boot-source', {'kernel_image_path': str(a.kernel), 'initrd_path': str(run / 'guest-initrd.gz'), 'boot_args': 'console=ttyS0 reboot=k panic=1 pci=off'})
        initial.api('PUT', '/balloon', {'amount_mib': 0, 'deflate_on_oom': False, 'stats_polling_interval_s': 1, 'free_page_reporting': a.free_page_reporting})
        initial.api('PUT', '/actions', {'action_type': 'InstanceStart'})
        probe = initial.event('probe')
        k.save_json(run / 'guest-probe.json', probe)
        ready = initial.event('ready')
        k.save_json(run / 'guest-ready.json', ready)
        if ready['page_size'] != 4096:
            raise RuntimeError('M10 fixture requires 4096-byte guest pages')
        k.save_json(run / 'balloon-initial.json', initial.api('GET', '/balloon/statistics'))
        if a.probe_only:
            success = True
            k.save_json(run / 'result.json', {'status': 'probe-passed'})
            return
        allocation = initial.command('allocate 107 40')
        k.save_json(run / 'parent-allocation.json', allocation)
        if not a.free_page_reporting:
            initial.command('release')
        initial.api('PATCH', '/vm', {'state': 'Paused'})
        initial.snapshot(run / 'A.state', run / 'A.ram')
        source_hash = k.sha256_file(run / 'A.ram')
        base = 0x80000000 if platform.machine() in ('aarch64', 'arm64') else 0
        offsets = [gpa - base for gpa in allocation['gpas']]
        if any((o < 0 or o + 4096 > 192 << 20 for o in offsets)):
            raise RuntimeError('GPA outside fixture RAM')
        with (run / 'A.ram').open('rb') as f:
            candidates = []
            for off in offsets:
                f.seek(off)
                if f.read(4096) == bytes([107]) * 4096:
                    candidates.append(off)
        if not candidates:
            raise AssertionError('parent snapshot has no nonzero free-arena pages')
        initial.close()
        vms.remove(initial)
        helper = k.NBDHelper(a.nbd_helper, run / 'A.ram', run, layout_id='fixture-192m-low-ram-4k-v1')
        helpers.append(helper)
        all_helpers.append(helper)
        if helper.ready['content_id'] != 'sha256:' + source_hash:
            raise AssertionError('wrong NBD source identity')
        left = k.VM(a, 'balloon-left', run)
        vms.append(left)
        all_vms.append(left)
        right = k.VM(a, 'balloon-right', run)
        vms.append(right)
        all_vms.append(right)
        for vm in (left, right):
            configure_metrics(vm)
            vm.restore(run / 'A.state', Path(helper.ready['path']))
            vm.api('PATCH', '/vm', {'state': 'Resumed'})
            k.check_guest_markers(vm, 4096, [49, 50, 51, 52])
        right.api('PATCH', '/vm', {'state': 'Paused'})
        regions = left.mappings()
        right_regions = right.mappings()
        if a.free_page_reporting:
            left.command('release')
            deadline = time.monotonic() + 30
            while True:
                left.api('PUT', '/actions', {'action_type': 'FlushMetrics'})
                if remapped_offsets(left, regions, candidates):
                    break
                if time.monotonic() >= deadline:
                    raise TimeoutError('no FPR anonymous remap observed for released nonzero arena pages')
                time.sleep(0.2)
            stats = left.api('GET', '/balloon/statistics')
            k.save_json(run / 'fpr-balloon-zero.json', stats)
            if stats['actual_mib'] != 0 or stats['target_mib'] != 0:
                raise AssertionError('FPR run unexpectedly inflated balloon')
        else:
            left.api('PATCH', '/balloon', {'amount_mib': 96})
            wait_balloon(left, 96, run / 'inflate-stats.json')
        left.api('PATCH', '/vm', {'state': 'Paused'})
        left.snapshot(run / 'discard-unwritten.state')
        maps = Path(f'/proc/{left.process.pid}/maps').read_text()
        (run / 'discard-host-maps.txt').write_text(maps)
        remapped = remapped_offsets(left, regions, candidates)
        if not remapped:
            raise AssertionError('no source-nonzero arena page actually anonymously remapped by balloon')
        unwritten = checkpoint(left, regions, run, 'discard-unwritten')
        with (run / 'discard-unwritten.ram').open('rb') as f:
            discarded = []
            for off in remapped:
                f.seek(off)
                if f.read(4096) == bytes(4096):
                    discarded.append(off)
        if not a.free_page_reporting and len(discarded) != len(remapped):
            raise AssertionError('balloon remapped source-nonzero pages were not all zero before reuse')
        if not discarded:
            raise AssertionError('no actual zero page among source-nonzero anonymous remaps')
        # In FPR mode the allocator can legitimately reuse some remapped
        # pages before pause; only observed final-zero pages are selected.
        k.save_json(run / 'discard-pages.json', {'parent_nonzero_candidates': len(candidates), 'discarded_pages': len(discarded), 'offsets': discarded, 'evidence': 'guest FPR with balloon held zero' if a.free_page_reporting else 'guest balloon actual_mib=96', 'byte_evidence': 'host file mapping replaced by anonymous VMA; same offsets source=0x6b and final bytes=zero'})
        # Independent dump of the same frozen checkpoint must remain stable.
        left.snapshot(run / 'discard-unwritten.repeat.state', run / 'discard-unwritten.repeat.ram')
        if k.sha256_file(run / 'discard-unwritten.repeat.ram') != unwritten['sha256']:
            raise AssertionError('frozen balloon checkpoint changed')
        right.snapshot(run / 'other-unmodified.state')
        other = checkpoint(right, right_regions, run, 'other-unmodified')
        k.check_marker_file(run / 'other-unmodified.ram', discarded, 4096, [107] * len(discarded))
        if k.sha256_file(run / 'A.ram') != source_hash:
            raise AssertionError('balloon modified immutable source')
        left.api('PATCH', '/vm', {'state': 'Resumed'})
        left.api('PATCH', '/balloon', {'amount_mib': 0})
        wait_balloon(left, 0, run / 'deflate-stats.json')
        rewritten = left.command('allocate 165 100')
        k.save_json(run / 'rewritten-allocation.json', rewritten)
        rewritten_offsets = sorted(set(discarded) & {gpa - base for gpa in rewritten['gpas']})
        if not rewritten_offsets:
            raise AssertionError('guest did not reuse any previously discarded nonzero source page')
        expected_arena = hashlib.sha256(bytes([165]) * (100 << 20)).hexdigest()
        if left.command('arena-read')['sha256'] != expected_arena:
            raise AssertionError('guest arena bytes differ after rewrite')
        left.api('PATCH', '/vm', {'state': 'Paused'})
        left.snapshot(run / 'discard-rewritten.state')
        rewritten_receipt = checkpoint(left, regions, run, 'discard-rewritten')
        k.check_marker_file(run / 'discard-rewritten.ram', rewritten_offsets, 4096, [165] * len(rewritten_offsets))
        k.save_json(run / 'rewritten-pages.json', {'rewritten_discarded_pages': len(rewritten_offsets), 'offsets': rewritten_offsets})
        left.api('PUT', '/actions', {'action_type': 'FlushMetrics'})
        left.close()
        vms.remove(left)
        k.save_json(run / 'release-one.json', helper.release_one())
        right.api('PATCH', '/vm', {'state': 'Resumed'})
        k.check_guest_markers(right, 4096, [49, 50, 51, 52])
        right.api('PATCH', '/vm', {'state': 'Paused'})
        right.api('PUT', '/actions', {'action_type': 'FlushMetrics'})
        right.close()
        vms.remove(right)
        helper.close()
        helpers.remove(helper)
        restored_receipts = {}
        for name, check_offsets, value in (('discard-unwritten', discarded, 0), ('discard-rewritten', rewritten_offsets, 165)):
            restored = k.VM(a, name + '-restored', run)
            vms.append(restored)
            all_vms.append(restored)
            restored.restore(run / (name + '.state'), run / (name + '.ram'))
            restored_regions = restored.mappings()
            restored.snapshot(run / (name + '.restored.state'))
            restored_receipts[name] = k.export_memory(restored, restored_regions, run / (name + '.restored.ram'))
            k.check_marker_file(run / (name + '.restored.ram'), check_offsets, 4096, [value] * len(check_offsets))
            restored.api('PATCH', '/vm', {'state': 'Resumed'})
            k.check_guest_markers(restored, 4096, [49, 50, 51, 52])
            if name == 'discard-rewritten' and restored.command('arena-read')['sha256'] != expected_arena:
                raise AssertionError('restored guest rewritten arena mismatch')
            restored.close()
            vms.remove(restored)
        if k.sha256_file(run / 'A.ram') != source_hash:
            raise AssertionError('final source hash changed')
        metrics = {}
        for name in ('balloon-left', 'balloon-right'):
            rows = [json.loads(line) for line in (run / name / 'metrics.jsonl').read_text().splitlines() if line]
            metrics[name] = {'free_page_report_count': sum((row.get('balloon', {}).get('free_page_report_count', 0) for row in rows)), 'free_page_report_freed': sum((row.get('balloon', {}).get('free_page_report_freed', 0) for row in rows)), 'free_page_report_fails': sum((row.get('balloon', {}).get('free_page_report_fails', 0) for row in rows))}
        k.save_json(run / 'metrics-summary.json', metrics)
        if a.free_page_reporting and (not metrics['balloon-left']['free_page_report_count'] or metrics['balloon-left']['free_page_report_fails']):
            raise AssertionError('FPR missing successful device metrics or has processing errors')
        success = True
        k.save_json(run / 'result.json', {'status': 'passed', 'mechanism': 'actual guest free-page-reporting with balloon target=actual=0' if a.free_page_reporting else 'actual guest virtio-balloon inflation/deflation, not synthetic host madvise', 'guest_kernel': probe['/proc/version'], 'parent_nonzero_candidates': len(candidates), 'discarded_pages': len(discarded), 'rewritten_discarded_pages': len(rewritten_offsets), 'unwritten': unwritten, 'rewritten': rewritten_receipt, 'other': other, 'restored': restored_receipts, 'source_sha256': source_hash, 'free_page_reporting': a.free_page_reporting, 'scope': 'M10 balloon/FPR path, independent full-logical exporter/native dump; orchestrator integration is separate'})
    except BaseException as e:
        k.save_json(run / 'result.json', {'status': 'failed', 'type': type(e).__name__, 'error': str(e)})
        raise
    finally:
        receipt = cleanup_resources(vms, helpers, all_vms, all_helpers, run,
                                    success, success or a.cleanup_failed)
        if receipt['quarantined']:
            retain_quarantined_runner(receipt, helpers, run)
        if success and receipt['errors']:
            raise RuntimeError('M10 cleanup failed; inspect cleanup.json')


if __name__ == '__main__':
    main()
