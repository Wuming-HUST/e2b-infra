#!/usr/bin/env python3
"""Audit saved PMEM trace receipts and independently recompute workload hashes.

Large frozen RAM artifacts stay on agent03; their full/delta comparison is an
on-target Go assertion. This auditor verifies the small exported raw evidence.
"""
import hashlib
import json
from pathlib import Path
import struct
import sys


def load(path):
    return json.loads(path.read_bytes())


def sha(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def state_hash(last_tool, boundary=None):
    data = bytearray(b'\xab' * (64 << 20))
    for off in range(0, len(data), 4096):
        struct.pack_into('<Q', data, off, off ^ ((last_tool or 0) << 24) ^ 0x123456789ABCDEF0)
    if boundary is not None:
        data[:4096] = {10: b'X', 200: b'\0', 700: b'R'}[boundary] * 4096
    return data


def verify(root):
    manifest = load(root / 'evidence-sha256.json')
    for name, expected in manifest.items():
        assert sha(root / name) == expected, name
    trace_path = root / 'inputs/swe-marathon-trace.json'
    trace_sha = sha(trace_path)
    assert trace_sha == '683d7fcc17c4edbd56ec6977293d3ec527a0ed4edc9cb1aa8c0f9935a200dcc5'
    trace = load(trace_path)
    assert sha(root / 'inputs/replay.py') == '3403029dffe5f00a226a6b74c7d0ff8ff423e7d3c48f5e5d77811e0421931add'
    assert sha(root / 'inputs/mtool.c') == 'a2a4425d7b26b5e5ad8779f572927f07f3b9fad97c05d787599eac38e89afa53'
    tools = [(i, t) for i, t in enumerate(trace['turns']) if t['tool']]
    assert len(trace['turns']) == 1016 and len(tools) == 571
    want_calls = [call for _, t in tools for call in t['tool']['call_ids']]
    assert len(want_calls) == 576
    cleanup = load(root / 'evidence/cleanup.json')
    assert not cleanup['remaining_task_processes'] and not cleanup['remaining_namespaces']
    assert all(not row['alive'] for row in cleanup['vms'])
    provenance = load(root / 'evidence/provenance.json')
    assert not provenance['mismatches']
    summary = {}
    for run, mode in [('marathon-full-01', 'full_logical'), ('marathon-tracked-01', 'tracked_delta')]:
        base = root / 'evidence' / run
        receipt = load(base / 'test.json')
        assert receipt['exit'] == 0 and receipt['ram_export_mode'] == mode
        assert '--- PASS: TestPmemMarathonIntegration' in (base / 'test.log').read_text()
        path = base / 'run'
        result = load(path / 'guest-result.json')
        events = [json.loads(line) for line in (path / 'guest-events.jsonl').read_bytes().splitlines()]
        starts = [e for e in events if e['event'] == 'start']
        assert len(starts) == 1 and starts[0]['trace_sha256'] == trace_sha
        assert starts[0]['checkpoint_turns'] == [10, 200, 700]
        actual_tools = [e for e in events if e['event'] == 'tool_end']
        assert len(actual_tools) == len(tools)
        assert [call for e in actual_tools for call in e['call_ids']] == want_calls
        for e, (turn, t) in zip(actual_tools, tools):
            expected = t['tool']
            assert e['turn'] == turn and e['source_step'] == t['source_step']
            assert e['rc'] == 0 and e['kind'] == expected['kind']
            assert e['modeled_mb'] == expected['mb']
            assert e['work']['allocated_bytes'] == expected['mb'] << 20
            assert e['work']['policy'] == 'fixed-work-no-padding-v1'
            assert e['work']['read_bytes'] == (16 << 20 if expected['kind'] in ['read', 'scan'] else 0)
        ready = [e for e in events if e['event'] == 'checkpoint_ready']
        resumed = [e for e in events if e['event'] == 'checkpoint_resumed']
        assert [e['turn'] for e in ready] == [10, 200, 700]
        assert [e['turn'] for e in resumed] == [10, 200, 700]
        for before, after in zip(ready, resumed):
            turn = before['turn']
            last = max(i for i, _ in tools if i < turn)
            expected = hashlib.sha256(state_hash(last, turn)).hexdigest()
            page = {10: b'X', 200: b'\0', 700: b'R'}[turn] * 4096
            assert before['sha256'] == after['sha256'] == expected
            assert before['disk_sha256'] == after['disk_sha256'] == hashlib.sha256(page).hexdigest()
            assert before['pid'] == after['pid'] == result['pid'] and after['verify']
        final_state = state_hash(tools[-1][0])
        final_state[8:4096] = b'R' * (4096 - 8)
        final_sha = hashlib.sha256(final_state).hexdigest()
        done = [e for e in events if e['event'] == 'replay_done']
        assert len(done) == 1
        assert done[0]['verify'] and result['verify']
        assert done[0]['final_sha256'] == result['sha256'] == final_sha
        assert done[0]['tools'] == result['tools'] == 571
        assert done[0]['turns'] == result['turns'] == 1016
        assert all(e['pid'] == result['pid'] for e in events)
        checkpoints = []
        # Uploaded metadata must survive the production cache restore unchanged.
        # A is a full export even when subsequent snapshots use tracked deltas.
        rootfs = None
        for stage in ['A', 'marathon-10', 'marathon-200', 'marathon-700']:
            c = load(path / f'{stage}-checkpoint.json')
            build_id = c['build_id']
            metadata = load(path / 'objects' / build_id / 'metadata.json')
            cached = list((path / 'B1/template-cache' / build_id / 'cache').glob('*/metadata.json'))
            assert len(cached) == 1 and load(cached[0]) == metadata
            assert metadata['version'] == 3 and metadata['template']['build_id'] == build_id
            checkpoint = metadata['checkpoint']
            assert checkpoint['format_version'] == 3
            assert checkpoint['logical_ram_size'] == c['ram_size'] == 1536 << 20
            assert checkpoint['ram_export_mode'] == ('tracked_delta' if c['tracked_delta'] else 'full_logical')
            if c['tracked_delta']:
                assert checkpoint['ram_payload_size'] == c['payload_size']
                assert checkpoint['ram_payload_sha256'] == c['payload_sha256']
                assert checkpoint['ram_tracking_version'] == 1
            else:
                assert checkpoint['logical_ram_sha256'] == c['ram_sha256'] == c['payload_sha256']
            if rootfs is None:
                rootfs = metadata['rootfs']
            assert metadata['rootfs'] == rootfs and rootfs['mode'] == 'pmem-dax-overlay'
        for turn in [10, 200, 700]:
            c = load(path / f'marathon-{turn}-checkpoint.json')
            assert c['native_same_frozen_state'] and c['ram_size'] == 1536 << 20
            assert c['tracked_delta'] == (mode == 'tracked_delta')
            if c['tracked_delta']:
                assert 0 < c['payload_size'] < c['ram_size']
                assert c['payload_size'] == c['candidate_pages'] * 4096
            else:
                assert c['payload_sha256'] == c['ram_sha256'] and c['payload_size'] == c['ram_size']
            checkpoints.append({k: c[k] for k in ['payload_size', 'ram_size', 'ram_sha256']})
        for stage in ['start', 'marathon-10', 'marathon-200', 'marathon-700']:
            backing = load(path / f'marathon-{stage}-backing.json')
            fields = backing['actual_ram_mapping'].split()
            assert fields[1] == 'rw-p' and fields[5] == backing['nbd_path']
            assert fields[5].startswith('/dev/nbd')
            pmem, guest = backing['pmem'], backing['guest_dax']
            assert pmem['read_only'] and pmem['guest_address'] <= guest['gpa'] < pmem['guest_address'] + pmem['size']
            for attrs in guest['statx'].values():
                assert attrs['attributes'] & 0x200000 and attrs['mask'] & 0x200000
        sources = load(path / 'marathon-unchanged-sources.json')
        assert sources['ram_sha256'] == load(path / 'A-checkpoint.json')['ram_sha256']
        assert sources['rootfs_sha256'] == sources['input_rootfs_sha256']
        summary[mode] = {'seconds': receipt['seconds'], 'turns': 1016, 'tools': 571, 'call_ids': 576,
                         'verified_resumes': 3, 'final_sha256': final_sha, 'checkpoints': checkpoints}
    return {'status': 'passed', 'evidence': 'trace-driven synthetic memory replay',
            'runs': summary, 'cleaned_up_vms': len(cleanup['vms'])}


if __name__ == '__main__':
    if len(sys.argv) != 2:
        raise SystemExit('usage: verify-marathon.py DELIVERY_DIRECTORY')
    print(json.dumps(verify(Path(sys.argv[1])), indent=2))
