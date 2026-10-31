#!/usr/bin/env python3
"""Run independent sequential target tests and preserve every exit/PID receipt."""
from pathlib import Path
import hashlib
import json
import os
import subprocess
import sys
import time

b = Path('/home/coder/projects/yangshengxin/e2b/page-sharing-20261030/pmem-dax-01')
d = b / 'remaining-20261031-01'
go = '/home/coder/projects/yangshengxin/e2b/toolchains/go1.25.4/bin/go'
env = dict(os.environ, GOCACHE=str(b.parent / 'go-build'),
           GOMODCACHE='/home/coder/projects/yangshengxin/e2b/cache/go-mod', GOPROXY='off',
           GOSUMDB='off', GOMAXPROCS='4', GOTOOLCHAIN='local',
           PATH='/home/coder/projects/yangshengxin/e2b/page-sharing-20261030/factory-deps/usr/sbin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin')
results = []
report = "campaign-result.json"


def run(name, command, extra=None, timeout=900):
    out = d / name
    out.mkdir()
    start = time.time()
    with (out / 'test.log').open('x') as log:
        process = subprocess.Popen(command, cwd=d / 'infra/packages/orchestrator',
            env=dict(env, **(extra or {})), stdin=subprocess.DEVNULL, stdout=log, stderr=subprocess.STDOUT)
        (out / 'pid.json').write_text(json.dumps({'pid': process.pid, 'command': command}, indent=2) + '\n')
        try:
            code = process.wait(timeout=timeout)
        except subprocess.TimeoutExpired:
            # Only this direct test process; its own cleanup and receipts remain
            # authoritative for any FC children, which must be audited separately.
            process.terminate()
            code = process.wait(timeout=30)
    row = {'name': name, 'command': command, 'exit': code, 'seconds': time.time() - start,
           'environment': extra or {}}
    (out / 'test.json').write_text(json.dumps(row, indent=2) + '\n')
    results.append(row)
    (d / report).write_text(json.dumps(results, indent=2) + '\n')
    return code


def main():
    if run('build-02', [go, 'test', '-p', '2', '-c', '-o', str(d / 'sandbox-02.test'), './internal/sandbox']):
        raise SystemExit(1)
    for mode, slot in [('full_logical', '26400'), ('tracked_delta', '26420')]:
        name = 'compat-' + mode + '-01'
        inputs = dict(E2B_PMEM_INTEGRATION_DIR=str(d / name / 'run'),
            E2B_SHARING_FC=str(b / 'cargo-target/debug/firecracker'),
            E2B_SHARING_KERNEL=str(b / 'kernel-build/out/arch/arm64/boot/Image'),
            E2B_SHARING_ROOTFS=str(b / 'fixture-02/upper.ext4'),
            E2B_SHARING_INITRD=str(b / 'go-check-04/initrd.gz'),
            E2B_SHARING_TRACE_FIXTURE=str(d / 'infra/scripts/memory-lab/page-sharing/biofabric-source-edit-slice.json'),
            E2B_PMEM_LAYOUT=str(d / 'fixture/layout.json'), E2B_PMEM_LOWER=str(d / 'fixture/lower.raw'),
            E2B_SHARING_SLOT_BASE=slot, E2B_SHARING_RAM_EXPORT_MODE=mode)
        run(name, [str(d / 'sandbox-02.test'), '-test.run', '^TestPmemCompatibilityIntegration$', '-test.v', '-test.timeout', '10m'], inputs)

    namespace_script = '''from pathlib import Path
    import os,subprocess
    root=Path(os.environ['E2B_ENOSPC_PARENT']);mount=root/'tmpfs';mount.mkdir()
    subprocess.run(['mount','-t','tmpfs','-o','size=16m,nosuid,nodev','tmpfs',str(mount)],check=True)
    try:
     r=subprocess.run([os.environ['E2B_PMEM_TEST_BINARY'],'-test.run','^TestPmemMaterializeDiskFullIntegration$','-test.v'],env=dict(os.environ,E2B_PMEM_ENOSPC_DIR=str(mount)))
    finally:subprocess.run(['umount',str(mount)],check=True)
    raise SystemExit(r.returncode)
    '''
    run('disk-full-01', ['unshare', '--mount', '--propagation', 'private', 'python3', '-c', namespace_script],
        {'E2B_ENOSPC_PARENT': str(d / 'disk-full-01'), 'E2B_PMEM_TEST_BINARY': str(d / 'pmem.test')})
    run('race-01', [go, 'test', '-race', '-p', '2', '-count=1',
        './internal/sandbox/pmem', './internal/sandbox/fc', './internal/sandbox/template',
        './internal/sandbox/memorybacking', './internal/sandbox/nbd', './internal/template/metadata', './cmd/pmem-init'])
    run('vsock-build-01', [go, 'build', '-o', str(d / 'vsock-guest'), str(d / 'infra/scripts/memory-lab/page-sharing/vsock_guest.go')], {'CGO_ENABLED': '0'})
    run('helper-build-01', [go, 'build', '-o', str(d / 'source-helper'), './cmd/page-sharing-source'])
    if (d / 'vsock-guest').exists() and (d / 'source-helper').exists():
        run('vsock-01', ['python3', str(d / 'infra/scripts/memory-lab/page-sharing/vsock_check.py'),
            '--firecracker', str(b / 'cargo-target/debug/firecracker'), '--kernel', str(b / 'kernel-build/out/arch/arm64/boot/Image'),
            '--guest-binary', str(d / 'vsock-guest'), '--nbd-helper', str(d / 'source-helper'), '--output', str(d / 'vsock-01/run')])


if __name__ == "__main__":
    main()
