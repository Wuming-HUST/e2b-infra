"""Same-kernel, non-DAX OverlayFS reference for FD/mmap and directory rename."""
import ctypes
import errno
import json
import mmap
import os
from pathlib import Path
import subprocess

root = Path('/run/pmem-overlay-baseline')
root.mkdir()
subprocess.run(['mount', '-t', 'tmpfs', '-o', 'size=4m', 'tmpfs', str(root)], check=True)
mounted = False
try:
    for name in ['lower', 'upper', 'work', 'merged']:
        (root / name).mkdir()
    (root / 'lower/file').write_bytes(b'A' * 4096)
    (root / 'lower/directory').mkdir()
    (root / 'lower/directory/child').write_bytes(b'child')
    subprocess.run(['mount', '-t', 'overlay', 'overlay', '-o',
                    'lowerdir=%s,upperdir=%s,workdir=%s' %
                    (root / 'lower', root / 'upper', root / 'work'), str(root / 'merged')], check=True)
    mounted = True
    fd = os.open(root / 'merged/file', os.O_RDONLY)
    try:
        with mmap.mmap(fd, 4096, flags=mmap.MAP_SHARED, prot=mmap.PROT_READ) as mapped:
            assert mapped[:] == b'A' * 4096
            with open(root / 'merged/file', 'r+b') as stream:
                stream.write(b'C' * 4096)
                stream.flush()
                os.fsync(stream.fileno())
            assert os.pread(fd, 4096, 0) == b'C' * 4096
            assert mapped[:] == b'A' * 4096
    finally:
        os.close(fd)
    directory_errno = 0
    try:
        os.rename(root / 'merged/directory', root / 'merged/renamed-dir')
    except OSError as error:
        directory_errno = error.errno
    assert directory_errno in (0, errno.EXDEV)
    # statx distinguishes this control's ordinary cached lower from DAX lower.
    libc = ctypes.CDLL(None, use_errno=True)
    statx = ctypes.create_string_buffer(256)
    assert libc.statx(-100, str(root / 'lower/file').encode(), 0, 0x7ff, statx) == 0
    attributes = int.from_bytes(statx.raw[8:16], 'little')
    assert not attributes & 0x200000
    print(json.dumps({'non_dax': True, 'open_fd_after_copyup': 'upper',
                      'open_mmap_after_copyup': 'lower', 'directory_rename_errno': directory_errno}))
finally:
    if mounted:
        subprocess.run(['umount', str(root / 'merged')], check=True)
    subprocess.run(['umount', str(root)], check=True)
    root.rmdir()
