#!/usr/bin/env python3
"""Recompute sharing/isolation checks from preserved target receipts.

This checks raw pagemap entries, address translations, nonzero marker hashes,
restored states, and receipt consistency. Full RAM reconstruction was checked
against the native frozen dump inside the target Go test; large RAM artifacts
remain on agent03 and are not part of this small evidence package.
"""
import hashlib
import json
from pathlib import Path
import sys

PAGE = 4096
RAM_BASE = 0x80000000
PMEM_BASE = 1 << 38


def load(path):
    return json.loads(path.read_text())


def page(row, gpa, base, permission):
    entry = row['entry']
    assert entry & (1 << 63), 'page is not present'
    assert row['pfn'] == entry & ((1 << 55) - 1) and row['pfn'] != 0
    assert row['file_page'] == bool(entry & (1 << 61))
    fields = row['mapping'].split()
    start, end = (int(x, 16) for x in fields[0].split('-'))
    assert fields[1] == permission
    assert start <= row['host_address'] < end
    assert row['host_address'] - start + int(fields[2], 16) == gpa - base


def guest(state, epoch):
    assert state['epoch'] == epoch
    values = [0x31, 0x32, 0x33] if epoch == 0 else [0x31, 0xa5, 0]
    if epoch == 2:
        values[0] = 0x7e
    assert state['ram_sha256'] == [hashlib.sha256(bytes([x]) * PAGE).hexdigest() for x in values]
    assert state['original_file_sha256'] == state['mapped_file_sha256'] == state['lower_file_sha256']
    assert state['current_file_sha256'] == state['expected_current_sha256']
    assert PMEM_BASE <= state['file_gpa'] < PMEM_BASE + (2281701376)
    attrs = state['statx']
    assert attrs['/.e2b/lower/sbin/sharing-init']['attributes'] & 0x200000
    if epoch == 0:
        assert state['current_file_sha256'] == state['original_file_sha256']
        assert state['current_file_gpa'] == state['file_gpa']
        assert attrs['/sbin/sharing-init']['attributes'] & 0x200000
    else:
        assert state['current_file_sha256'] != state['original_file_sha256']
        assert RAM_BASE <= state['current_file_gpa'] < RAM_BASE + (256 << 20)
        assert not attrs['/sbin/sharing-init']['attributes'] & 0x200000


def verify(root):
    manifest = load(root / 'evidence-sha256.json')
    for name, digest in manifest.items():
        assert hashlib.sha256((root / name).read_bytes()).hexdigest() == digest, name
    evidence = root / 'evidence'
    provenance = load(evidence / 'sharing-proof-provenance.json')
    assert not provenance['mismatches']
    cleanup = load(evidence / 'sharing-proof-cleanup.json')
    assert not cleanup['remaining_task_processes'] and not cleanup['remaining_namespaces']
    assert all(not row['alive'] for row in cleanup['vms'])
    summary = {}
    for run, export_mode in [('sharing-proof-full-01', 'full_logical'), ('sharing-proof-tracked-01', 'tracked_delta')]:
        base = evidence / run
        result = load(base / 'test.json')
        assert result['exit'] == 0 and result['ram_export_mode'] == export_mode
        assert '--- PASS: TestPmemSharingProofIntegration' in (base / 'test.log').read_text()
        run_summary = {'seconds': result['seconds'], 'modes': {}}
        a = load(base / 'run/A-checkpoint.json')
        for mode in ['B0', 'B1']:
            path = base / 'run'
            proof = load(path / f'{mode}-physical-sharing-proof.json')
            left, right = proof['left_after'], proof['right_after']
            guest(left, 1)
            guest(right, 0)
            assert left['ram_gpas'] == right['ram_gpas'] and left['file_gpa'] == right['file_gpa']
            for phase in ['before', 'after']:
                assert len(proof[phase]) == 3
                for sample in proof[phase]:
                    for side in ['left', 'right']:
                        for i, p in enumerate(sample[f'{side}_ram']):
                            page(p, left['ram_gpas'][i], RAM_BASE, 'rw-p')
                            assert ('/dev/nbd' in p['mapping']) == (mode == 'B1')
                        page(sample[f'{side}_rootfs'], left['file_gpa'], PMEM_BASE, 'r--s')
                    for i in range(3):
                        l, r = sample['left_ram'][i], sample['right_ram'][i]
                        changed = phase == 'after' and i in [1, 2]
                        assert (l['pfn'] != r['pfn']) == changed
                        assert l['file_page'] == (not changed) and r['file_page']
                    assert sample['left_rootfs']['pfn'] == sample['right_rootfs']['pfn']
            checkpoints = {}
            for generation, epoch in [('B', 1), ('C', 2)]:
                state = load(path / f'{mode}-{generation}-proof-restored.json')
                guest(state, epoch)
                assert state['pid'] == left['pid'], 'snapshot must retain the running process'
                assert state['file_gpa'] == left['file_gpa']
                c = load(path / f'{mode}-{generation}-checkpoint.json')
                assert c['native_same_frozen_state'] and c['ram_size'] == 256 << 20
                if c['tracked_delta']:
                    assert 0 < c['payload_size'] < c['ram_size']
                    assert c['payload_size'] == c['candidate_pages'] * PAGE
                else:
                    assert c['payload_size'] == c['ram_size'] and c['payload_sha256'] == c['ram_sha256']
                checkpoints[generation] = {k: c[k] for k in ['tracked_delta', 'payload_size', 'ram_size', 'ram_sha256']}
            if export_mode == 'tracked_delta':
                assert all(c['tracked_delta'] for c in checkpoints.values()), 'must actually test incremental snapshots for both RAM backends'
            source = load(path / f'{mode}-unchanged-sources.json')
            assert source['ram_sha256'] == a['ram_sha256']
            assert source['rootfs_sha256'] == source['input_rootfs_sha256'] == provenance['inputs']['fixture-02/lower.raw']['sha256']
            run_summary['modes'][mode] = {'checkpoints': checkpoints, 'shared_ram_pfns': [x['pfn'] for x in proof['before'][0]['left_ram']], 'shared_rootfs_pfn': proof['before'][0]['left_rootfs']['pfn']}
        control = load(base / 'run/different-backing-control.json')
        assert control['same_bytes'] and control['left_inode'] != control['right_inode']
        assert control['left_rootfs']['pfn'] != control['right_rootfs']['pfn']
        assert all(l['pfn'] != r['pfn'] for l, r in zip(control['left_ram'], control['right_ram']))
        summary[run] = run_summary
    return {'result': 'passed', 'evidence_files': len(manifest), 'cleaned_up_vms': len(cleanup['vms']), 'runs': summary}


if __name__ == '__main__':
    if len(sys.argv) != 2:
        raise SystemExit('usage: verify-sharing-proof.py DELIVERY_DIRECTORY')
    print(json.dumps(verify(Path(sys.argv[1])), indent=2))
