#!/usr/bin/env python3
"""Run transport faults with real Factory VMs, each in a fresh directory."""
import importlib.util
import json
import os
from pathlib import Path

spec = importlib.util.spec_from_file_location('campaign', Path(__file__).with_name('run-target.py'))
campaign = importlib.util.module_from_spec(spec)
spec.loader.exec_module(campaign)
original = campaign.d
campaign.d = campaign.b / ('runtime-extra-' + os.environ.get('E2B_RUNTIME_EXTRA_SUFFIX', '01'))
d, go = campaign.d, campaign.go
if campaign.run('build-01', [go, 'test', '-p', '2', '-c', '-o', str(d / 'sandbox.test'), './internal/sandbox']):
    raise SystemExit(1)
inputs = json.loads((original / 'compat-full_logical-01/test.json').read_text())['environment']
for index, (name, test) in enumerate([('http-short', 'TestSharingRemoteShortReadIntegration'),
                                    ('http-delay', 'TestSharingRemoteDelayIntegration')]):
    env = dict(inputs, E2B_PMEM_INTEGRATION_DIR=str(d / name / 'run'),
               E2B_SHARING_SLOT_BASE=str(26700 + 20 * index),
               E2B_SHARING_TRACE_FIXTURE=str(d / 'infra/scripts/memory-lab/page-sharing/biofabric-source-edit-slice.json'))
    campaign.run(name, [str(d / 'sandbox.test'), '-test.run', '^' + test + '$', '-test.v', '-test.timeout', '5m'], env)
campaign.run('history-and-race', [go, 'test', '-race', '-p', '2', '-count=1', '-v',
             './internal/sandbox/build', './internal/sandbox/template', './internal/sandbox/memorybacking',
             './internal/sandbox/nbd', './internal/sandbox/fc'], timeout=300)
