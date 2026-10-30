#!/usr/bin/env python3
"""Offline receipt/delivery integrity and independently hashed oracle checks."""
import hashlib
import json
from pathlib import Path

base = Path(__file__).resolve().parent
for line in (base / 'SHA256SUMS').read_text().splitlines():
    expected, name = line.split('  ', 1)
    assert hashlib.sha256((base / name).read_bytes()).hexdigest() == expected, name
receipts = base / 'receipts'
for name, expected in json.loads((receipts / 'receipt-sha256.json').read_text()).items():
    assert hashlib.sha256((receipts / name).read_bytes()).hexdigest() == expected, name
final = receipts / 'final-validation'
assert json.loads((final / 'source-sha256.json').read_text())['matched']
for name in ['api-complete', 'fmt-check', 'freeze-regressions', 'prepare-save', 'headers-full']:
    assert json.loads((final / (name + '-exit.json')).read_text())['returncode'] == 0, name
for line in (final / 'go-exit.tsv').read_text().splitlines():
    assert line.split('\t')[1] == '0', line
# The original API offline-cache failure is retained. Its successful retry is
# checked above; do not rewrite the earlier failure receipt.
for line in (final / 'fc-exit.tsv').read_text().splitlines():
    if not line.startswith('api\t'):
        assert line.split('\t')[1] == '0', line
large = {x['path'].split('/tracked-delta-01/', 1)[1]: x
         for x in json.loads((final / 'retained-large-artifacts.json').read_text())}
checks = 0
for name, entry in large.items():
    if name.endswith('.native.ram'):
        rebuilt = name.replace('.native.ram', '.ram')
        if rebuilt in large:
            assert entry['sha256'] == large[rebuilt]['sha256'], name
            checks += 1
for checkpoint in (receipts / 'factory-tracked-final').glob('*-checkpoint.json'):
    data = json.loads(checkpoint.read_text())
    native = 'factory-tracked-final/' + checkpoint.name.replace('-checkpoint.json', '-native.ram')
    assert large[native]['sha256'] == data['ram_sha256'], checkpoint
    assert large[native]['size'] == data['ram_size'], checkpoint
    checks += 1
for name in ['native-final', 'factory-tracked-final', 'm10-balloon-tracked-final',
             'm10-fpr-tracked-final', 'm11-tracked-final']:
    assert json.loads((receipts / (name + '-exit.json')).read_text())['returncode'] == 0, name
    result = json.loads((receipts / name / 'result.json').read_text())
    assert result.get('status', result.get('result')) == 'passed', name
print(f'Delivery hashes, source matching, test receipts and {checks} RAM oracle hashes verified.')
print('This verifies retained evidence; it does not rerun VMs or certify production acceptance.')
