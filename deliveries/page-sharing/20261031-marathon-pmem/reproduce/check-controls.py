#!/usr/bin/env python3
"""Challenge the offline auditor using copies; never alter original receipts."""
import hashlib
import importlib.util
import json
from pathlib import Path
import shutil
import sys
import tempfile


def write_json(path, value):
    path.write_text(json.dumps(value, indent=2) + '\n')


def mutate(root, control):
    run = root / 'evidence/marathon-tracked-01/run'
    if control == 'archive_file_integrity':
        (run / 'guest-result.json').write_text('{}\n')
        return
    if control in ['tool_call_order', 'restored_state_hash']:
        path = run / 'guest-events.jsonl'
        events = [json.loads(line) for line in path.read_text().splitlines()]
        if control == 'tool_call_order':
            event = next(e for e in events if e['event'] == 'tool_end')
            event['call_ids'][0] = 'invalid-call-id'
        else:
            event = next(e for e in events if e['event'] == 'checkpoint_resumed')
            event['sha256'] = '0' * 64
        path.write_text(''.join(json.dumps(e) + '\n' for e in events))
    elif control == 'delta_candidate_size':
        path = run / 'marathon-200-checkpoint.json'
        value = json.loads(path.read_text())
        value['candidate_pages'] += 1
        write_json(path, value)
    elif control == 'restored_metadata':
        path = next((run / 'B1/template-cache').glob('*/cache/*/metadata.json'))
        value = json.loads(path.read_text())
        value['checkpoint']['logical_ram_size'] += 4096
        write_json(path, value)
    else:
        raise ValueError(control)
    # Refresh the outer hash so these controls challenge semantic assertions.
    manifest_path = root / 'evidence-sha256.json'
    manifest = json.loads(manifest_path.read_text())
    manifest[path.relative_to(root).as_posix()] = hashlib.sha256(path.read_bytes()).hexdigest()
    write_json(manifest_path, manifest)


def main(root):
    spec = importlib.util.spec_from_file_location(
        'audit', root.parents[2] / 'scripts/memory-lab/pmem-dax/verify-marathon.py')
    audit = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(audit)
    assert audit.verify(root)['status'] == 'passed'
    results = {}
    for control in ['archive_file_integrity', 'tool_call_order', 'restored_state_hash',
                    'delta_candidate_size', 'restored_metadata']:
        with tempfile.TemporaryDirectory(prefix='pmem-audit-control-') as temp:
            copy = Path(temp) / 'delivery'
            shutil.copytree(root, copy)
            mutate(copy, control)
            try:
                audit.verify(copy)
            except AssertionError:
                results[control] = 'rejected'
            else:
                raise AssertionError(f'auditor accepted damaged evidence: {control}')
    print(json.dumps({'status': 'passed', 'positive_control': 'passed',
                      'negative_controls': results}, indent=2))


if __name__ == '__main__':
    main(Path(sys.argv[1]).resolve())
