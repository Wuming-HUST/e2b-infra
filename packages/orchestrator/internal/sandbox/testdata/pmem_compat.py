"""Persistent guest oracle; assertions run again after each real VM restore."""
import ctypes
import errno
import fcntl
import hashlib
import json
import mmap
import os
from pathlib import Path
import socket
import struct
import subprocess
import tarfile
import threading

P = 4096
ROOT = Path('/pmem-compat')
DATA = bytes(range(256)) * 32
libc = ctypes.CDLL(None, use_errno=True)
libc.mmap.restype = ctypes.c_void_p
libc.mmap.argtypes = [ctypes.c_void_p, ctypes.c_size_t, ctypes.c_int, ctypes.c_int, ctypes.c_int, ctypes.c_long]

def sha(data):
    return hashlib.sha256(data).hexdigest()

def gpa(address, pid='self'):
    with open('/proc/%s/pagemap' % pid, 'rb', buffering=0) as stream:
        stream.seek(address // P * 8)
        entry = struct.unpack('<Q', stream.read(8))[0]
    assert entry >> 63 and entry & ((1 << 55) - 1)
    return (entry & ((1 << 55) - 1)) * P + address % P

def map_read(path):
    fd = os.open(str(path), os.O_RDONLY)
    ptr = libc.mmap(None, len(DATA), mmap.PROT_READ, mmap.MAP_SHARED, fd, 0)
    assert ptr not in (None, ctypes.c_void_p(-1).value)
    assert ctypes.string_at(ptr, len(DATA)) == DATA
    return fd, ptr

# The same ordinary read() result resides in distinct per-VM writable buffers.
buffer = mmap.mmap(-1, len(DATA), flags=mmap.MAP_PRIVATE | mmap.MAP_ANONYMOUS)
with open(ROOT / 'copyup', 'rb', buffering=0) as source:
    assert source.readinto(buffer) == len(DATA)
assert buffer[:] == DATA
buffer_addr = ctypes.addressof(ctypes.c_char.from_buffer(buffer))
assert libc.mlock(ctypes.c_void_p(buffer_addr), ctypes.c_size_t(len(DATA))) == 0
opened = {name: map_read(ROOT / name) for name in ['copyup', 'rename', 'unlink']}
child = subprocess.Popen([str(ROOT / 'exec-loop')], stdin=subprocess.DEVNULL)
epoch = 0


def transfer(kind, address=None, port=None, accepted=None):
    """sendfile/splice source stays on DAX; host TCP covers virtio TX."""
    expected = DATA
    output = []
    listener = None
    if address is None and accepted is None:
        listener = socket.socket()
        listener.bind(('127.0.0.1', 0))
        listener.listen(1)
        address, port = listener.getsockname()
        def receive():
            connection, _ = listener.accept()
            with connection:
                output.append(b''.join(iter(lambda: connection.recv(65536), b'')))
        thread = threading.Thread(target=receive)
        thread.start()
    with (accepted if accepted is not None else socket.create_connection((address, port), timeout=10)) as connection:
        with open(ROOT / 'network', 'rb', buffering=0) as source:
            if kind == 'sendfile':
                off = 0
                while off < len(expected):
                    n = os.sendfile(connection.fileno(), source.fileno(), off, len(expected) - off)
                    assert n > 0
                    off += n
            else:
                r, w = os.pipe()
                try:
                    count = 0
                    while count < len(expected):
                        n = os.splice(source.fileno(), w, min(P, len(expected) - count))
                        assert n > 0
                        sent = 0
                        while sent < n:
                            m = os.splice(r, connection.fileno(), n - sent)
                            assert m > 0
                            sent += m
                        count += n
                finally:
                    os.close(r)
                    os.close(w)
        connection.shutdown(socket.SHUT_WR)
    if listener:
        thread.join(10)
        listener.close()
        assert not thread.is_alive() and output == [expected]
    return {'kind': kind, 'bytes': len(expected), 'sha256': sha(expected)}


transfer_thread = None
transfer_output = []


def transfer_start(kind):
    global transfer_thread, transfer_output
    assert transfer_thread is None
    listener = socket.socket()
    listener.settimeout(10)
    listener.bind(('0.0.0.0', 0))
    listener.listen(1)
    port = listener.getsockname()[1]
    transfer_output = []
    def send():
        try:
            with listener:
                connection, _ = listener.accept()
                connection.settimeout(10)
                transfer_output.append(transfer(kind, accepted=connection))
        except Exception as error:
            transfer_output.append({'error': repr(error)})
    transfer_thread = threading.Thread(target=send)
    transfer_thread.start()
    return {'port': port}


def transfer_finish():
    global transfer_thread
    assert transfer_thread is not None
    transfer_thread.join(15)
    assert not transfer_thread.is_alive()
    assert len(transfer_output) == 1 and 'error' not in transfer_output[0], transfer_output
    transfer_thread = None
    return transfer_output[0]


def check():
    assert child.poll() is None
    assert buffer[:] == (DATA if epoch == 0 else b'B' * P + DATA[P:])
    for name, (fd, ptr) in opened.items():
        # Ordinary OverlayFS revalidates a read FD against the copied-up real
        # file, while an existing mmap still retains its original lower VMA.
        expected_fd = b'C' * P + DATA[P:] if epoch and name == 'copyup' else DATA
        assert os.pread(fd, len(DATA), 0) == expected_fd, name
        assert ctypes.string_at(ptr, len(DATA)) == DATA, name
    assert (ROOT / 'network').read_bytes() == DATA
    if epoch == 0:
        for name in ['copyup', 'rename', 'unlink', 'truncate', 'directory/child']:
            assert (ROOT / name).read_bytes() == DATA, name
        for name in ['renamed', 'hardlink', 'new', 'renamed-dir']:
            assert not (ROOT / name).exists()
    else:
        assert (ROOT / 'copyup').read_bytes() == b'C' * P + DATA[P:] + b'append'
        assert (ROOT / 'truncate').read_bytes() == DATA[:123] + b'\0' * (3 * P - 123)
        assert (ROOT / 'new').read_bytes() == b'new-data'
        assert not (ROOT / 'rename').exists() and not (ROOT / 'unlink').exists()
        assert (ROOT / ('renamed' if epoch == 1 else 'hardlink')).read_bytes() == DATA
        assert epoch == 2 or os.stat(ROOT / 'renamed').st_ino == os.stat(ROOT / 'hardlink').st_ino
        assert os.readlink(ROOT / 'symlink') == 'renamed'
        assert os.getxattr(ROOT / 'hardlink', 'user.compat') == b'xattr'
        st = os.stat(ROOT / 'hardlink')
        assert st.st_mode & 0o777 == 0o640 and st.st_uid == 123 and st.st_gid == 456
        if directory_errno == 0:
            assert not (ROOT / 'directory').exists()
            assert (ROOT / 'renamed-dir/child').read_bytes() == DATA
        else:
            assert directory_errno == errno.EXDEV
            assert (ROOT / 'directory/child').read_bytes() == DATA
        assert (ROOT / 'installed/payload').read_bytes() == b'archive-payload'
    if epoch == 2:
        assert not (ROOT / 'renamed-again').exists()
        assert upper_map[:] == DATA and os.pread(upper_fd, len(DATA), 0) == DATA
    # Ensure the measured executable page belongs to a running program's text.
    executable = None
    for line in Path('/proc/%d/maps' % child.pid).read_text().splitlines():
        fields = line.split()
        if len(fields) >= 6 and fields[1].startswith('r-x') and fields[-1] == str(ROOT / 'exec-loop'):
            address = int(fields[0].split('-')[0], 16)
            try:
                executable = gpa(address, child.pid)
            except AssertionError:
                continue
            break
    assert executable is not None
    return {'epoch': epoch, 'pid': os.getpid(), 'exec_pid': child.pid,
            'exec_gpa': executable, 'buffer_gpa': gpa(buffer_addr),
            'buffer_sha256': sha(buffer[:]), 'original_sha256': sha(DATA),
            'open_mmap_gpas': {name: gpa(ptr) for name, (_, ptr) in opened.items()},
            'directory_rename_errno': directory_errno if epoch else None}


def mutate():
    global epoch, directory_errno
    assert epoch == 0
    buffer[:P] = b'B' * P
    with open(ROOT / 'copyup', 'r+b') as stream:
        stream.write(b'C' * P)
        stream.seek(0, os.SEEK_END)
        stream.write(b'append')
        stream.flush()
        os.fsync(stream.fileno())
    with open(ROOT / 'truncate', 'r+b') as stream:
        stream.truncate(123)
        stream.truncate(3 * P)
        stream.flush()
        os.fsync(stream.fileno())
    os.rename(ROOT / 'rename', ROOT / 'renamed')
    os.link(ROOT / 'renamed', ROOT / 'hardlink')
    os.symlink('renamed', ROOT / 'symlink')
    os.chmod(ROOT / 'renamed', 0o640)
    os.chown(ROOT / 'renamed', 123, 456)
    os.setxattr(ROOT / 'renamed', 'user.compat', b'xattr')
    os.unlink(ROOT / 'unlink')
    (ROOT / 'new').write_bytes(b'new-data')
    directory_errno = 0
    try:
        os.rename(ROOT / 'directory', ROOT / 'renamed-dir')
    except OSError as error:
        directory_errno = error.errno
    # Kernel-mediated locks must still exclude a distinct process after copy-up.
    with open(ROOT / 'renamed', 'rb') as locked:
        fcntl.flock(locked, fcntl.LOCK_EX | fcntl.LOCK_NB)
        program = "import fcntl,sys; f=open(sys.argv[1],'rb'); fcntl.flock(f,fcntl.LOCK_EX|fcntl.LOCK_NB)"
        result = subprocess.run(['python3', '-c', program, str(ROOT / 'renamed')], capture_output=True)
        assert result.returncode != 0 and b'BlockingIOError' in result.stderr
    # Installation-like unpack from immutable lower to the private upper.
    with tarfile.open(ROOT / 'install.tar') as archive:
        archive.extractall(ROOT / 'installed')
    epoch = 1


def mutate2():
    global epoch, upper_fd, upper_map
    assert epoch == 1
    upper_fd = os.open(ROOT / 'renamed', os.O_RDONLY)
    upper_map = mmap.mmap(upper_fd, len(DATA), flags=mmap.MAP_SHARED, prot=mmap.PROT_READ)
    os.rename(ROOT / 'renamed', ROOT / 'renamed-again')
    os.unlink(ROOT / 'renamed-again')
    # The hardlink keeps the inode alive; FD + mapping retain the same bytes.
    epoch = 2


server = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
server.bind('/run/pmem-compat.sock')
server.listen(4)
Path('/run/pmem-compat.ready').touch()
while True:
    connection, _ = server.accept()
    with connection:
        command = json.loads(connection.recv(4096))
        if command['op'] == 'mutate':
            mutate()
        elif command['op'] == 'mutate2':
            mutate2()
        elif command['op'] == 'transfer':
            connection.sendall(json.dumps(transfer(command['kind'], command.get('address'), command.get('port'))).encode() + b'\n')
            continue
        elif command['op'] == 'transfer-start':
            connection.sendall(json.dumps(transfer_start(command['kind'])).encode() + b'\n')
            continue
        elif command['op'] == 'transfer-finish':
            connection.sendall(json.dumps(transfer_finish()).encode() + b'\n')
            continue
        elif command['op'] == 'read-buffer':
            assert epoch == 0
            with open(ROOT / 'copyup', 'rb', buffering=0) as source:
                assert source.readinto(buffer) == len(DATA)
        else:
            assert command['op'] == 'read'
        connection.sendall(json.dumps(check()).encode() + b'\n')
