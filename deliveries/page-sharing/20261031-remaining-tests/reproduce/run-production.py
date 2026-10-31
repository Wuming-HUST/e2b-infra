#!/usr/bin/env python3
"""Production musl/real per-thread seccomp; run in a fresh source-copy campaign.

The caller must provision production-runtime-NN/infra and these scripts first.
No existing campaign or input is replaced. Jailer runtime is a separate test.
"""
import importlib.util
import json
import os
from pathlib import Path

spec = importlib.util.spec_from_file_location('campaign', Path(__file__).with_name('run-target.py'))
campaign = importlib.util.module_from_spec(spec)
spec.loader.exec_module(campaign)
original = campaign.d
campaign.d = campaign.b / ('production-runtime-' + os.environ.get('E2B_PRODUCTION_SUFFIX', '01'))
d, b, go = campaign.d, campaign.b, campaign.go
musl = b / 'musl-target/aarch64-unknown-linux-musl/release'
build_env = {'CARGO_HOME': str(b.parent / 'cargo-home'), 'CARGO_TARGET_DIR': str(b / 'musl-target'),
             'CFLAGS_aarch64_unknown_linux_musl': '-I' + str(b / 'production-build-04/uapi'),
             'LIBRARY_PATH': str(b / 'production-build-05/host-lib'), 'RUSTC': '/usr/bin/rustc'}
if campaign.run('jailer-build', ['/usr/bin/cargo', 'build', '--manifest-path', str(b / 'fc/Cargo.toml'),
        '--release', '--target', 'aarch64-unknown-linux-musl', '-p', 'jailer', '--locked', '-j', '4'], build_env):
    raise SystemExit(1)
if campaign.run('sandbox-build', [go, 'test', '-p', '2', '-c', '-o', str(d / 'sandbox.test'), './internal/sandbox']):
    raise SystemExit(1)
campaign.run('binary-type', ['file', str(musl / 'firecracker'), str(musl / 'jailer')])
import hashlib
(d / 'production-binary-sha256.json').write_text(json.dumps({
    str(path): hashlib.sha256(path.read_bytes()).hexdigest() for path in [musl / 'firecracker', musl / 'jailer']
}, indent=2) + '\n')
slot_base = int(os.environ.get('E2B_PRODUCTION_SLOT_BASE', '26800'))
for index, mode in enumerate(['full_logical', 'tracked_delta']):
    inputs = json.loads((original / ('compat-' + mode + '-01/test.json')).read_text())['environment']
    env = dict(inputs, E2B_SHARING_FC=str(musl / 'firecracker'), E2B_SHARING_REQUIRE_SECCOMP='1',
               E2B_PMEM_INTEGRATION_DIR=str(d / mode / 'run'), E2B_SHARING_SLOT_BASE=str(slot_base + 20 * index),
               E2B_SHARING_TRACE_FIXTURE=str(d / 'infra/scripts/memory-lab/page-sharing/biofabric-source-edit-slice.json'))
    campaign.run(mode, [str(d / 'sandbox.test'), '-test.run', '^TestPmemCompatibilityIntegration$',
                       '-test.v', '-test.timeout', '10m'], env)
