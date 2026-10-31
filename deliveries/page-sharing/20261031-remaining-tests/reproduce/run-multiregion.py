#!/usr/bin/env python3
from pathlib import Path
import json
import os
import subprocess
import time

b = Path('/home/coder/projects/yangshengxin/e2b/page-sharing-20261030/pmem-dax-01')
d = b / 'multiregion-02'
env = dict(os.environ, CARGO_HOME=str(b.parent / 'cargo-home'), CARGO_TARGET_DIR=str(b / 'cargo-target'),
           CARGO_BUILD_JOBS='4', GOCACHE=str(b.parent / 'go-build'),
           GOMODCACHE='/home/coder/projects/yangshengxin/e2b/cache/go-mod', GOTOOLCHAIN='local',
           GOPROXY='off', GOSUMDB='off', CGO_ENABLED='0',
           PYTHONPATH=str(b / 'infra/scripts/memory-lab/page-sharing'))
commands = [
    ('split-build', ['cargo', 'build', '--locked', '--offline', '-p', 'vmm', '--example', 'split_snapshot', '-j', '4'], b / 'fc'),
    ('guest-build', ['/home/coder/projects/yangshengxin/e2b/toolchains/go1.25.4/bin/go', 'build', '-o', str(d / 'guest'),
                     str(b / 'infra/scripts/memory-lab/page-sharing/guest.go')], b / 'infra/packages/orchestrator'),
    ('test', ['python3', str(d / 'scripts/memory-lab/page-sharing/multiregion_check.py'),
              '--firecracker', str(b / 'cargo-target/debug/firecracker'),
              '--kernel', str(b / 'kernel-build/out/arch/arm64/boot/Image'),
              '--guest-binary', str(d / 'guest'), '--nbd-helper', str(b / 'remaining-20261031-01/source-helper'),
              '--split-tool', str(b / 'cargo-target/debug/examples/split_snapshot'), '--output', str(d / 'run')], d),
]
results = []
for name, cmd, cwd in commands:
    start = time.time()
    with (d / (name + '.log')).open('x') as log:
        p = subprocess.Popen(cmd, cwd=cwd, env=env, stdout=log, stderr=subprocess.STDOUT)
    (d / (name + '-pid.json')).write_text(json.dumps({'pid': p.pid, 'command': cmd}, indent=2) + '\n')
    code = p.wait()
    results.append({'name': name, 'command': cmd, 'exit': code, 'seconds': time.time() - start})
    (d / 'test.json').write_text(json.dumps(results, indent=2) + '\n')
    if code:
        break
