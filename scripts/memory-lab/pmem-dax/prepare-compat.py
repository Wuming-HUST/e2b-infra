#!/usr/bin/env python3
"""Create a disposable compatibility lower from the quiescent ext4 intermediate."""
import hashlib
import io
import json
from pathlib import Path
import platform
import subprocess
import sys
import tarfile


def sha(path):
    with path.open('rb') as stream:
        return hashlib.file_digest(stream, 'sha256').hexdigest()


def run(*args, allowed=(0,)):
    result = subprocess.run(args, check=False)
    if result.returncode not in allowed:
        raise RuntimeError((args, result.returncode))


def main(source, out):
    assert platform.machine() == 'aarch64'
    source = source.resolve(strict=True)
    before = sha(source)
    out.mkdir(mode=0o700)  # Must be new, never alter an existing run/template.
    run('e2fsck', '-f', '-n', str(source))
    image = out / 'lower.ext4'
    run('cp', '--reflink=auto', '--sparse=always', str(source), str(image))
    image.chmod(0o600)
    data = out / 'data.bin'
    data.write_bytes(bytes(range(256)) * 32)
    assembly = out / 'exec-loop.S'
    assembly.write_text('.global _start\n.text\n_start:\n mov x8, #124\n svc #0\n b _start\n')
    executable = out / 'exec-loop'
    run('gcc', '-nostdlib', '-static', '-Wl,-z,max-page-size=4096', '-o', str(executable), str(assembly))
    archive = out / 'install.tar'
    with tarfile.open(archive, 'w') as stream:
        info = tarfile.TarInfo('payload')
        payload = b'archive-payload'
        info.size, info.mode, info.mtime = len(payload), 0o640, 0
        stream.addfile(info, io.BytesIO(payload))
    commands = ['mkdir /pmem-compat', 'mkdir /pmem-compat/directory']
    for name in ['copyup', 'rename', 'unlink', 'truncate', 'network', 'directory/child']:
        commands.append(f'write {data} /pmem-compat/{name}')
    commands += [f'write {executable} /pmem-compat/exec-loop',
                 'set_inode_field /pmem-compat/exec-loop mode 0100755',
                 f'write {archive} /pmem-compat/install.tar']
    batch = out / 'debugfs.txt'
    batch.write_text('\n'.join(commands) + '\n')
    run('debugfs', '-w', '-f', str(batch), str(image))
    run('e2fsck', '-f', '-p', str(image), allowed=(0, 1))
    run('e2fsck', '-f', '-n', str(image))
    raw = out / 'lower.raw'
    run(sys.executable, str(Path(__file__).with_name('pack-pfn.py')), str(image), str(raw))
    raw.chmod(0o444)
    layout = {'format_version': 1, 'mode': 'pmem-dax-overlay',
              'lower': {'sha256': sha(raw), 'size': raw.stat().st_size},
              'guest_address': 1 << 38, 'data_offset': 2 << 20}
    (out / 'layout.json').write_text(json.dumps(layout, indent=2) + '\n')
    assert sha(source) == before
    (out / 'source.json').write_text(json.dumps({'source': str(source), 'source_sha256': before,
        'source_unchanged': True, 'fixture_data_sha256': sha(data), 'layout': layout}, indent=2) + '\n')


if __name__ == '__main__':
    main(Path(sys.argv[1]), Path(sys.argv[2]))
