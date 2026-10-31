#!/usr/bin/env python3
"""Repeat only failed compatibility cases after correcting the buffer oracle."""
import importlib.util
import json
from pathlib import Path
import time

spec = importlib.util.spec_from_file_location('campaign', Path(__file__).with_name('run-target.py'))
campaign = importlib.util.module_from_spec(spec)
spec.loader.exec_module(campaign)
campaign.report = 'retry-result.json'
b, d, go = campaign.b, campaign.d, campaign.go
pid = json.loads((d / 'campaign-driver.json').read_text())['pid']
deadline = time.monotonic() + 1000
while Path('/proc/%d/cmdline' % pid).exists():
    cmdline = Path('/proc/%d/cmdline' % pid).read_bytes()
    if b'run-target.py' not in cmdline:
        break
    if time.monotonic() > deadline:
        raise RuntimeError('previous campaign still running; refuse overlap')
    time.sleep(2)
if campaign.run('build-03', [go, 'test', '-p', '2', '-c', '-o', str(d / 'sandbox-03.test'), './internal/sandbox']):
    raise SystemExit(1)
for mode, slot in [('full_logical', '26440'), ('tracked_delta', '26460')]:
    name = 'compat-' + mode + '-02'
    original = json.loads((d / ('compat-' + mode + '-01') / 'test.json').read_text())
    env = dict(original['environment'], E2B_PMEM_INTEGRATION_DIR=str(d / name / 'run'), E2B_SHARING_SLOT_BASE=slot)
    campaign.run(name, [str(d / 'sandbox-03.test'), '-test.run', '^TestPmemCompatibilityIntegration$', '-test.v', '-test.timeout', '10m'], env)
