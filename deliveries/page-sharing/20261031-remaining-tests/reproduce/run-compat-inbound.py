#!/usr/bin/env python3
"""Use a fresh campaign root; retain prior failed runs and their source copy."""
import importlib.util
import json
import os
from pathlib import Path

spec = importlib.util.spec_from_file_location('campaign', Path(__file__).with_name('run-target.py'))
campaign = importlib.util.module_from_spec(spec)
spec.loader.exec_module(campaign)
previous = campaign.d
campaign.d = campaign.b / ('remaining-20261031-' + os.environ.get('E2B_COMPAT_CAMPAIGN_SUFFIX', '02'))
campaign.report = 'campaign-result.json'
d, go = campaign.d, campaign.go
if campaign.run('build-01', [go, 'test', '-p', '2', '-c', '-o', str(d / 'sandbox.test'), './internal/sandbox']):
    raise SystemExit(1)
slot_base = int(os.environ.get('E2B_COMPAT_SLOT_BASE', '26500'))
for index, mode in enumerate(os.environ.get('E2B_COMPAT_MODES', 'full_logical,tracked_delta').split(',')):
    slot = str(slot_base + 20 * index)
    name = 'compat-' + mode + '-01'
    original = json.loads((previous / ('compat-' + mode + '-01') / 'test.json').read_text())
    env = dict(original['environment'], E2B_PMEM_INTEGRATION_DIR=str(d / name / 'run'),
               E2B_SHARING_SLOT_BASE=slot,
               E2B_SHARING_TRACE_FIXTURE=str(d / 'infra/scripts/memory-lab/page-sharing/biofabric-source-edit-slice.json'))
    campaign.run(name, [str(d / 'sandbox.test'), '-test.run', '^TestPmemCompatibilityIntegration$', '-test.v', '-test.timeout', '10m'], env)
