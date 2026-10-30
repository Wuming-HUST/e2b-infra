#!/usr/bin/env python3
"""Isolated 128 MiB Firecracker File/MAP_PRIVATE checkpoint oracle.

This validates a native KVM mechanism and an independent process_vm_readv
exporter; it is not an orchestrator deployment or trace replay acceptance test.
Only children created by this run are stopped. Failed children are retained,
with their PID receipts, unless --cleanup-failed is explicitly supplied.
"""
import argparse
import ctypes
import csv
import errno
import gzip
import hashlib
import http.client
import json
import mmap
import os
from pathlib import Path
import platform
import pty
import select
import socket
import struct
import subprocess
import tempfile
import time
import tty


class IOVec(ctypes.Structure):
    _fields_ = [("base", ctypes.c_void_p), ("length", ctypes.c_size_t)]


def save_json(path, value):
    temp = path.with_name(path.name + ".tmp")
    with temp.open("w") as stream:
        json.dump(value, stream, indent=2)
        stream.flush()
        os.fsync(stream.fileno())
    temp.replace(path)


def sha256_file(path):
    digest = hashlib.sha256()
    with open(path, "rb", buffering=0) as stream:
        for data in iter(lambda: stream.read(8 << 20), b""):
            digest.update(data)
    return digest.hexdigest()


def make_initrd(binary, destination):
    data = bytearray()
    for index, (name, mode, content, major, minor) in enumerate([
        (".", 0o040755, b"", 0, 0), ("dev", 0o040755, b"", 0, 0),
        ("proc", 0o040755, b"", 0, 0), ("sys", 0o040755, b"", 0, 0),
        # The kernel opens PID 1's initial stdio BEFORE /init can mount devtmpfs.
        ("dev/console", 0o020600, b"", 5, 1),
        ("dev/null", 0o020666, b"", 1, 3),
        ("init", 0o100755, Path(binary).read_bytes(), 0, 0),
        ("TRAILER!!!", 0, b"", 0, 0),
    ], 1):
        encoded = name.encode() + b"\0"
        fields = [index, mode, 0, 0, 1, 0, len(content), 0, 0, major, minor, len(encoded), 0]
        data.extend(b"070701" + b"".join(f"{value:08x}".encode() for value in fields))
        data.extend(encoded)
        data.extend(b"\0" * (-len(data) % 4))
        data.extend(content)
        data.extend(b"\0" * (-len(data) % 4))
    with gzip.open(destination, "wb") as stream:
        stream.write(data)


class VM:
    def __init__(self, args, name, run):
        self.name, self.run = name, run
        self.directory = run / name
        self.directory.mkdir()
        self.socket_dir = Path(tempfile.mkdtemp(prefix="psg-"))
        self.api_path = self.socket_dir / "fc.sock"
        self.master, slave = pty.openpty()
        tty.setraw(slave)
        self.buffer = bytearray()
        self.log = (self.directory / "serial.log").open("ab", buffering=0)
        self.process = subprocess.Popen(
            [str(args.firecracker), "--api-sock", str(self.api_path)],
            stdin=slave, stdout=slave, stderr=slave, close_fds=True)
        os.close(slave)
        save_json(self.directory / "pid.json", {"pid": self.process.pid, "api_socket": str(self.api_path)})
        deadline = time.monotonic() + 15
        while not self.api_path.exists():
            if self.process.poll() is not None or time.monotonic() > deadline:
                self.drain()
                raise RuntimeError(f"{name}: Firecracker API failed; inspect {self.directory}")
            self.drain(0.05)

    def api(self, method, path, body=None):
        connection = http.client.HTTPConnection("localhost", timeout=20)
        connection.sock = socket.socket(socket.AF_UNIX)
        connection.sock.settimeout(20)
        connection.sock.connect(str(self.api_path))
        payload = None if body is None else json.dumps(body)
        try:
            connection.request(method, path, payload, {"Content-Type": "application/json"})
            response = connection.getresponse()
            data = response.read()
            with (self.directory / "api.jsonl").open("a") as receipt:
                receipt.write(json.dumps({"method": method, "path": path, "body": body,
                                          "status": response.status, "response": data.decode(errors="replace")}) + "\n")
            if response.status >= 300:
                raise RuntimeError(f"{self.name}: {path}: {response.status}: {data!r}")
            return json.loads(data) if data else None
        finally:
            connection.close()

    def drain(self, timeout=0):
        if select.select([self.master], [], [], timeout)[0]:
            try:
                chunk = os.read(self.master, 65536)
            except OSError as error:
                if error.errno == errno.EIO:
                    return False
                raise
            self.log.write(chunk)
            self.buffer.extend(chunk)
            return bool(chunk)
        return False

    def event(self, event, timeout=60):
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            while b"\n" in self.buffer:
                line, _, remainder = self.buffer.partition(b"\n")
                self.buffer = bytearray(remainder)
                at = line.find(b"PSG ")
                if at < 0:
                    continue
                value = json.loads(line[at + 4:].strip())
                if value["event"] in ("error", "command_error"):
                    raise RuntimeError(f"{self.name}: guest error {value}")
                if value["event"] == event:
                    return value
            if self.process.poll() is not None:
                raise RuntimeError(f"{self.name}: exited {self.process.returncode}")
            self.drain(0.1)
        raise TimeoutError(f"{self.name}: no guest event {event}")

    def command(self, command):
        os.write(self.master, command.encode() + b"\n")
        return self.event(command.split()[0])

    def restore(self, state, backing):
        self.api("PUT", "/snapshot/load", {
            "snapshot_path": str(state), "mem_backend": {"backend_type": "File", "backend_path": str(backing)},
            "enable_diff_snapshots": False, "resume_vm": False})

    def snapshot(self, state, ram=None):
        body = {"snapshot_type": "Full", "snapshot_path": str(state)}
        if ram is not None:
            body["mem_file_path"] = str(ram)
        self.api("PUT", "/snapshot/create", body)

    def mappings(self):
        value = self.api("GET", "/memory/mappings")
        regions = sorted(value["mappings"], key=lambda item: item["offset"])
        offset = 0
        for region in regions:
            if region["offset"] != offset or region["size"] <= 0 or region["base_host_virt_addr"] < 0:
                raise RuntimeError(f"invalid logical RAM mapping: {regions}")
            offset += region["size"]
        if not offset:
            raise RuntimeError("empty guest RAM mapping")
        save_json(self.directory / "mappings.json", regions)
        return regions

    def close(self):
        self.process.terminate()
        try:
            self.process.wait(timeout=10)
        except subprocess.TimeoutExpired:
            self.process.kill()
            self.process.wait(timeout=10)
        while self.drain():
            pass
        self.log.close()
        os.close(self.master)
        self.api_path.unlink(missing_ok=True)
        self.socket_dir.rmdir()


class NBDHelper:
    def __init__(self, binary, source, run):
        self.directory = run / "nbd-helper"
        self.directory.mkdir()
        self.log = (self.directory / "stderr.log").open("wb", buffering=0)
        self.stdout_log = (self.directory / "stdout.jsonl").open("wb", buffering=0)
        self.buffer = bytearray()
        self.process = subprocess.Popen([str(binary), "--source", str(source), "--layout-id", "fixture-128m-low-ram-4k-v1"],
                                        stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=self.log)
        save_json(self.directory / "pid.json", {"pid": self.process.pid})
        self.ready = self.event("ready")
        save_json(self.directory / "ready.json", self.ready)

    def event(self, name, timeout=60):
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            while b"\n" in self.buffer:
                line, _, remaining = self.buffer.partition(b"\n")
                self.buffer = bytearray(remaining)
                try:
                    event = json.loads(line)
                except (ValueError, UnicodeDecodeError):
                    continue
                if event.get("event") in ("error", "failure", "quarantined"):
                    raise RuntimeError(f"NBD helper: {event}")
                if event.get("event") == name:
                    return event
            # A short-lived helper can emit its final 'closed' receipt and exit
            # before poll() runs. Drain pipe bytes first, then decide whether an
            # exit happened without the requested receipt.
            if select.select([self.process.stdout], [], [], 0.1)[0]:
                data = os.read(self.process.stdout.fileno(), 65536)
                self.stdout_log.write(data)
                self.buffer.extend(data)
                if data:
                    continue
            if self.process.poll() is not None:
                raise RuntimeError(f"NBD helper exited {self.process.returncode} before {name}")
        raise TimeoutError(f"NBD helper did not emit {name}; retain its PID/device receipt")

    def release_one(self):
        self.process.stdin.write(b"release-one\n")
        self.process.stdin.flush()
        return self.event("lease_released")

    def close(self):
        self.process.terminate()
        self.event("closed", timeout=45)
        self.process.wait(timeout=10)
        if self.process.returncode:
            raise RuntimeError(f"NBD helper teardown failed: {self.process.returncode}")
        self.log.close()
        self.stdout_log.close()


def sample_shared_pfns(left, left_regions, right, right_regions, offsets, changed=()):
    samples = []
    for _ in range(3):
        pair = [[host_page_entry(vm.process.pid, host_address(regions, off)) for off in offsets]
                for vm, regions in ((left, left_regions), (right, right_regions))]
        for index, (a, b) in enumerate(zip(*pair)):
            if not all(entry["present"] and not entry["swapped"] and entry["pfn"] for entry in (a, b)):
                raise RuntimeError("PFN evidence unavailable: both markers must be present with visible nonzero PFNs")
            if (a["pfn"] == b["pfn"]) == (index in changed):
                raise AssertionError(f"unexpected sharing/COW for marker {index}: {a}, {b}")
        samples.append(pair)
        time.sleep(0.02)
    if not all(sample == samples[0] for sample in samples):
        raise RuntimeError("PFN sampling was unstable; cannot claim physical sharing")
    return samples


def record_memory_stats(run, phase, vms):
    """Sample before the full RAM oracle faults otherwise unused pages in."""
    rows = []
    for vm in vms:
        pid = vm.process.pid
        values = {}
        for filename in ("smaps_rollup", "status"):
            for line in Path(f"/proc/{pid}/{filename}").read_text().splitlines():
                fields = line.split()
                if fields and fields[0] in ("Rss:", "Pss:", "Private_Dirty:", "Shared_Clean:", "Swap:", "VmSwap:"):
                    values[fields[0].rstrip(":") + "_kib"] = int(fields[1])
        fields = Path(f"/proc/{pid}/stat").read_text().rsplit(")", 1)[1].split()
        values.update(minor_faults=int(fields[7]), major_faults=int(fields[9]))
        rows.append({"phase": phase, "vm": vm.name, "pid": pid, **values})
    path = run / "memory-metrics.csv"
    exists = path.exists()
    with path.open("a", newline="") as stream:
        writer = csv.DictWriter(stream, fieldnames=list(rows[0]))
        if not exists:
            writer.writeheader()
        writer.writerows(rows)


def check_guest_markers(vm, page_size, values):
    actual = vm.command("read")
    expected = [hashlib.sha256(bytes([value]) * page_size).hexdigest() for value in values]
    if actual["sha256"] != expected:
        raise AssertionError(f"{vm.name}: marker mismatch: {actual}")
    return actual


def shared_nbd_check(args, run, offsets, page_size, vms, helpers):
    if page_size != 4096 or os.sysconf("SC_PAGE_SIZE") != 4096:
        raise RuntimeError("first-stage shared NBD fixture requires 4096-byte host and guest pages")
    helper = NBDHelper(args.nbd_helper.resolve(strict=True), run / "A.ram", run)
    helpers.append(helper)
    if helper.ready["content_id"] != "sha256:" + sha256_file(run / "A.ram"):
        raise AssertionError("NBD helper identity differs from immutable snapshot A")
    left = VM(args, "nbd-left", run)
    vms.append(left)
    right = VM(args, "nbd-right", run)
    vms.append(right)
    backing = Path(helper.ready["path"])
    for vm in (left, right):
        vm.restore(run / "A.state", backing)
        vm.api("PATCH", "/vm", {"state": "Resumed"})
        # First marker operation in BOTH restored guests is a read.
        check_guest_markers(vm, page_size, [0x31, 0x32, 0x33, 0x34])
        vm.api("PATCH", "/vm", {"state": "Paused"})
    left_regions, right_regions = left.mappings(), right.mappings()
    shared = sample_shared_pfns(left, left_regions, right, right_regions, offsets)
    save_json(run / "M02-two-vm-shared-pfns.json", shared)
    record_memory_stats(run, "shared-before-full-oracle", (left, right))
    left.api("PATCH", "/vm", {"state": "Resumed"})
    left.command("write 1 165")
    left.command("zero 2")
    left.api("PATCH", "/vm", {"state": "Paused"})
    cow = sample_shared_pfns(left, left_regions, right, right_regions, offsets, changed=(1, 2))
    save_json(run / "M03-two-vm-cow-pfns.json", cow)
    record_memory_stats(run, "private-before-full-oracle", (left, right))
    left.snapshot(run / "NBD.private.state")
    pageout = None
    if args.attempt_pageout:
        pageout = attempt_private_pageout(left, left_regions, offsets, page_size, run)
        record_memory_stats(run, "after-pageout-before-full-oracle", (left, right))
    private = export_memory(left, left_regions, run / "NBD.private.ram")
    check_marker_file(run / "NBD.private.ram", offsets, page_size, [0x31, 165, 0, 0x34])
    left.snapshot(run / "NBD.private.native.state", run / "NBD.private.native.ram")
    if private["sha256"] != sha256_file(run / "NBD.private.native.ram"):
        raise AssertionError("NBD private RAM export differs from FC native oracle")
    left.close()
    vms.remove(left)
    save_json(run / "M05-release-one.json", helper.release_one())
    # Closing the first VM and releasing its lease must not affect the other.
    right.api("PATCH", "/vm", {"state": "Resumed"})
    check_guest_markers(right, page_size, [0x31, 0x32, 0x33, 0x34])
    right.api("PATCH", "/vm", {"state": "Paused"})
    right.snapshot(run / "NBD.unmodified.state")
    unmodified = export_memory(right, right_regions, run / "NBD.unmodified.ram")
    check_marker_file(run / "NBD.unmodified.ram", offsets, page_size, [0x31, 0x32, 0x33, 0x34])
    check_marker_file(run / "A.ram", offsets, page_size, [0x31, 0x32, 0x33, 0x34])
    if helper.ready["content_id"] != "sha256:" + sha256_file(run / "A.ram"):
        raise AssertionError("private writes altered immutable source A")
    right.close()
    vms.remove(right)
    helper.close()
    helpers.remove(helper)
    restored = VM(args, "nbd-private-restored", run)
    vms.append(restored)
    restored.restore(run / "NBD.private.state", run / "NBD.private.ram")
    restored.api("PATCH", "/vm", {"state": "Resumed"})
    check_guest_markers(restored, page_size, [0x31, 165, 0, 0x34])
    restored.close()
    vms.remove(restored)
    if pageout is not None:
        pageout["oracles"] = {"exported_marker_bytes": "passed", "native_full_ram_hash": "passed",
                              "restored_guest_markers": "passed"}
        if pageout["status"] == "swapped_pending_oracles":
            pageout.update(status="mechanism_only", reason="both private markers actually swapped; export/native/restore oracles passed")
        save_json(run / "M09-pageout.json", pageout)
    return {"M02": "three stable two-VM shared PFN samples passed", "M03": "private write/zero PFN separation and isolation passed",
            "M05": "one lease release preserves other VM; final drain confirmed", "private": private, "unmodified": unmodified,
            **({"M09": pageout} if pageout is not None else {})}


def host_page_entry(pid, address):
    with open(f"/proc/{pid}/pagemap", "rb", buffering=0) as stream:
        stream.seek(address // os.sysconf("SC_PAGE_SIZE") * 8)
        value, = struct.unpack("<Q", stream.read(8))
    return {"present": bool(value >> 63), "swapped": bool((value >> 62) & 1),
            "pfn": value & ((1 << 55) - 1), "exclusive": bool((value >> 56) & 1),
            "file_or_shared_anon": bool((value >> 61) & 1)}


def attempt_private_pageout(vm, regions, offsets, page_size, run):
    """One bounded advisory request, only to this run's frozen COW pages.

    A successful syscall is not evidence of swap. Pagemap polling never reads
    guest bytes; the existing full exporter is the first subsequent fault-in.
    """
    receipt = {"status": "unaccepted", "pid": vm.process.pid, "vm": vm.name,
               "advice": "MADV_PAGEOUT", "advice_value": 0x15, "flags": 0,
               "syscall_provenance": "golang.org/x/sys v0.40.0 unix/zsysnum_linux_{arm64,amd64}.go and zerrors_linux.go",
               "scope": "two COW marker pages in this run's nbd-left only; no pressure or system configuration changes",
               "requested_bytes": 2 * page_size, "samples": []}

    def finish(reason):
        receipt["reason"] = reason
        save_json(run / "M09-pageout.json", receipt)
        return receipt

    if vm.name != "nbd-left" or vm.run != run or vm.process.poll() is not None:
        return finish("target is not this run's live nbd-left child; no request issued")
    machine = platform.machine()
    # Verified against the architecture-specific x/sys generated syscall files.
    syscall_numbers = {"aarch64": 440, "arm64": 440, "x86_64": 440, "amd64": 440}
    if machine not in syscall_numbers or not hasattr(os, "pidfd_open"):
        return finish("unsupported architecture or Python pidfd_open; no request issued")
    addresses = [host_address(regions, offsets[index]) for index in (1, 2)]
    receipt["addresses"] = addresses
    if page_size != os.sysconf("SC_PAGE_SIZE") or len(set(addresses)) != 2 or any(address % page_size for address in addresses):
        return finish("marker page size/alignment/distinctness precondition failed; no request issued")
    try:
        # These pages already passed two-VM PFN COW separation. Require private
        # anonymous exclusive PTEs too, so advice cannot target source cache.
        before = [host_page_entry(vm.process.pid, address) for address in addresses]
        receipt["before"] = before
        if not all(entry["present"] and not entry["swapped"] and entry["exclusive"]
                   and not entry["file_or_shared_anon"] for entry in before):
            return finish("both markers were not present exclusive anonymous COW pages; no request issued")
        # This Popen child cannot be PID-reused without first being reaped; do
        # not wait/poll again between this identity check and opening its pidfd.
        pidfd = os.pidfd_open(vm.process.pid, 0)
        try:
            vectors = (IOVec * len(addresses))(*(IOVec(address, page_size) for address in addresses))
            libc = ctypes.CDLL(None, use_errno=True)
            syscall = libc.syscall
            syscall.restype = ctypes.c_ssize_t
            ctypes.set_errno(0)
            count = syscall(ctypes.c_long(syscall_numbers[machine]), ctypes.c_int(pidfd),
                            ctypes.byref(vectors), ctypes.c_size_t(len(addresses)),
                            ctypes.c_int(0x15), ctypes.c_ulong(0))
            receipt.update(syscall_number=syscall_numbers[machine], returned_bytes=count)
            if count < 0:
                error = ctypes.get_errno()
                receipt.update(errno=error, error=os.strerror(error))
                return finish("process_madvise rejected; swap precondition unaccepted")
            # MADV_PAGEOUT is advisory and may return before I/O completes.
            # Bound observation to two seconds; never create memory pressure.
            deadline = time.monotonic() + 2
            while True:
                entries = [host_page_entry(vm.process.pid, address) for address in addresses]
                receipt["samples"].append(entries)
                if all(entry["swapped"] and not entry["present"] for entry in entries):
                    receipt["status"] = "swapped_pending_oracles"
                    return finish("both marker PTEs swapped; full export/native/restore oracles still pending")
                if time.monotonic() >= deadline:
                    return finish("both marker PTEs did not actually swap; syscall success is insufficient")
                time.sleep(0.05)
        finally:
            os.close(pidfd)
    except OSError as error:
        receipt.update(errno=error.errno, error=str(error))
        return finish("pidfd/pageout/pagemap unavailable; swap precondition unaccepted")


def file_page_residency(path, offsets):
    # A separate MAP_PRIVATE mapping queries backing-cache residency without
    # reading the marker bytes. An absent FC PTE alone does not establish the
    # old mincore failure: a hot backing cache would still report resident.
    page_size = os.sysconf("SC_PAGE_SIZE")
    with path.open("rb") as stream:
        length = os.fstat(stream.fileno()).st_size
        with mmap.mmap(stream.fileno(), length, access=mmap.ACCESS_COPY) as mapping:
            address = ctypes.addressof(ctypes.c_char.from_buffer(mapping))
            vector = (ctypes.c_ubyte * ((length + page_size - 1) // page_size))()
            libc = ctypes.CDLL(None, use_errno=True)
            if libc.mincore(ctypes.c_void_p(address), ctypes.c_size_t(length), vector):
                raise OSError(ctypes.get_errno(), "mincore failed")
            return [bool(vector[offset // page_size] & 1) for offset in offsets]


def host_address(regions, logical_offset):
    for region in regions:
        if region["offset"] <= logical_offset < region["offset"] + region["size"]:
            return region["base_host_virt_addr"] + logical_offset - region["offset"]
    raise RuntimeError(f"logical offset {logical_offset} outside RAM")


def export_memory(vm, regions, destination):
    libc = ctypes.CDLL(None, use_errno=True)
    readv = libc.process_vm_readv
    readv.argtypes = [ctypes.c_int, ctypes.POINTER(IOVec), ctypes.c_ulong,
                      ctypes.POINTER(IOVec), ctypes.c_ulong, ctypes.c_ulong]
    readv.restype = ctypes.c_ssize_t
    digest, total, started = hashlib.sha256(), 0, time.monotonic()
    temporary = destination.with_name(destination.name + ".tmp")
    try:
        with temporary.open("xb", buffering=0) as output:
            for region in regions:
                position = 0
                while position < region["size"]:
                    length = min(8 << 20, region["size"] - position)
                    buffer = ctypes.create_string_buffer(length)
                    copied, retries = 0, 0
                    while copied < length:
                        local = IOVec(ctypes.addressof(buffer) + copied, length - copied)
                        remote = IOVec(region["base_host_virt_addr"] + position + copied, length - copied)
                        count = readv(vm.process.pid, ctypes.byref(local), 1, ctypes.byref(remote), 1, 0)
                        if count < 0:
                            error = ctypes.get_errno()
                            if error == errno.EINTR:
                                continue
                            if error in (errno.ENOMEM, errno.EAGAIN) and retries < 3:
                                retries += 1
                                time.sleep(0.1)
                                continue
                            raise OSError(error, os.strerror(error))
                        if count == 0 or count > length - copied:
                            raise RuntimeError("process_vm_readv made invalid/no progress")
                        copied += count
                        retries = 0
                    data = buffer.raw
                    digest.update(data)
                    view = memoryview(data)
                    while view:
                        count = output.write(view)
                        if not count:
                            raise RuntimeError("snapshot output made no progress")
                        view = view[count:]
                    total += length
                    position += length
            os.fsync(output.fileno())
        temporary.replace(destination)
    except BaseException:
        temporary.unlink(missing_ok=True)
        raise
    receipt = {"mode": "full_logical", "sha256": digest.hexdigest(), "bytes": total,
               "seconds": time.monotonic() - started, "pid": vm.process.pid}
    save_json(destination.with_suffix(".json"), receipt)
    return receipt


def check_marker_file(path, offsets, page_size, values):
    with path.open("rb") as stream:
        for offset, value in zip(offsets, values):
            stream.seek(offset)
            actual = stream.read(page_size)
            if actual != bytes([value]) * page_size:
                raise AssertionError(f"{path}: marker offset {offset} differs from byte {value}")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--firecracker", required=True, type=Path)
    parser.add_argument("--kernel", required=True, type=Path)
    parser.add_argument("--guest-binary", required=True, type=Path)
    parser.add_argument("--output", required=True, type=Path)
    parser.add_argument("--boot-args", default="console=ttyS0 reboot=k panic=1 pci=off")
    parser.add_argument("--ram-base-gpa", type=lambda value: int(value, 0), default=None)
    parser.add_argument("--cleanup-failed", action="store_true")
    parser.add_argument("--nbd-helper", type=Path, help="optionally add two-VM shared NBD PFN/COW/lease checks after the regular-file oracle")
    parser.add_argument("--attempt-pageout", action="store_true",
                        help="with --nbd-helper, request MADV_PAGEOUT on two own COW pages; requires observed swap and later oracles")
    args = parser.parse_args()
    if platform.system() != "Linux":
        parser.error("KVM oracle requires Linux")
    if args.attempt_pageout and not args.nbd_helper:
        parser.error("--attempt-pageout requires --nbd-helper")
    for name in ("firecracker", "kernel", "guest_binary"):
        setattr(args, name, getattr(args, name).resolve(strict=True))
    args.output = args.output.resolve()
    args.output.mkdir(parents=True, exist_ok=False)
    if args.ram_base_gpa is None:
        args.ram_base_gpa = 0x80000000 if platform.machine() in ("aarch64", "arm64") else 0
    # RAM is deliberately below the x86 MMIO hole; GPA-base is an explicit
    # test-fixture input, never used for general multi-region export ordering.
    run, vms, helpers, success = args.output, [], [], False
    save_json(run / "environment.json", {"uname": list(platform.uname()), "host_page_size": os.sysconf("SC_PAGE_SIZE"),
              "ram_base_gpa": args.ram_base_gpa, "fc_sha256": sha256_file(args.firecracker),
              "kernel_sha256": sha256_file(args.kernel), "guest_sha256": sha256_file(args.guest_binary)})
    make_initrd(args.guest_binary, run / "guest-initrd.gz")
    try:
        initial = VM(args, "initial", run)
        vms.append(initial)
        initial.api("PUT", "/machine-config", {"vcpu_count": 1, "mem_size_mib": 128})
        initial.api("PUT", "/boot-source", {"kernel_image_path": str(args.kernel),
                    "initrd_path": str(run / "guest-initrd.gz"), "boot_args": args.boot_args})
        initial.api("PUT", "/actions", {"action_type": "InstanceStart"})
        ready = initial.event("ready")
        save_json(run / "guest-ready.json", ready)
        offsets = [gpa - args.ram_base_gpa for gpa in ready["gpas"]]
        page_size = ready["page_size"]
        if any(offset < 0 or offset + page_size > 128 << 20 for offset in offsets):
            raise RuntimeError("marker GPAs do not fit the explicitly configured low-RAM fixture")
        initial.api("PATCH", "/vm", {"state": "Paused"})
        initial.snapshot(run / "A.state", run / "A.ram")
        check_marker_file(run / "A.ram", offsets, page_size, [0x31, 0x32, 0x33, 0x34])
        initial.close()
        vms.remove(initial)
        # Evict only this run's clean regular-file source. Verify the result;
        # advisory eviction succeeding is not proof that residency changed.
        with (run / "A.ram").open("rb") as source:
            os.fsync(source.fileno())
            os.posix_fadvise(source.fileno(), 0, 0, os.POSIX_FADV_DONTNEED)
        cold = file_page_residency(run / "A.ram", offsets)
        save_json(run / "M01-source-residency-before-restore.json", cold)
        if cold[0]:
            raise RuntimeError("M01 source cache eviction did not occur; absent FC PTE alone is insufficient")
        second = VM(args, "unaccessed-restore", run)
        vms.append(second)
        second.restore(run / "A.state", run / "A.ram")
        regions = second.mappings()
        second.snapshot(run / "B.state")
        before = [host_page_entry(second.process.pid, host_address(regions, offset)) for offset in offsets]
        save_json(run / "M01-before-export-pagemap.json", before)
        cold = file_page_residency(run / "A.ram", offsets)
        save_json(run / "M01-source-residency-before-export.json", cold)
        if cold[0]:
            raise RuntimeError("M01 source became resident before export; precondition not established")
        if before[0]["present"] or before[0]["swapped"]:
            raise RuntimeError("M01 absent-PTE precondition did not hold; do not claim unaccessed-page acceptance")
        b_receipt = export_memory(second, regions, run / "B.ram")
        # Restore deliberately changes the 16-byte VMGenID in guest RAM before
        # any vCPU executes. Compare whole RAM against a native dump of this
        # SAME frozen restored checkpoint, rather than requiring A == B.
        second.snapshot(run / "B.native.state", run / "B.native.ram")
        if sha256_file(run / "B.native.ram") != b_receipt["sha256"]:
            raise AssertionError("unexecuted B export differs from FC native full-memory oracle")
        save_json(run / "A-to-B-hashes.json", {"A": sha256_file(run / "A.ram"), "B": b_receipt["sha256"],
                  "note": "FC restores a new VMGenID before vCPU execution; cross-generation whole-RAM equality is not required"})
        check_marker_file(run / "B.ram", offsets, page_size, [0x31, 0x32, 0x33, 0x34])
        second.close()
        vms.remove(second)
        third = VM(args, "mutated-restore", run)
        vms.append(third)
        third.restore(run / "B.state", run / "B.ram")
        third.api("PATCH", "/vm", {"state": "Resumed"})
        third.command("write 1 165")
        third.command("zero 2")
        third.api("PATCH", "/vm", {"state": "Paused"})
        third.snapshot(run / "C.state")
        c_receipt = export_memory(third, third.mappings(), run / "C.ram")
        check_marker_file(run / "C.ram", offsets, page_size, [0x31, 165, 0, 0x34])
        # Independent FC-native full dump of the SAME still-frozen checkpoint.
        third.snapshot(run / "C.native.state", run / "C.native.ram")
        if sha256_file(run / "C.native.ram") != c_receipt["sha256"]:
            raise AssertionError("process exporter differs from FC native full-memory oracle")
        check_marker_file(run / "A.ram", offsets, page_size, [0x31, 0x32, 0x33, 0x34])
        third.close()
        vms.remove(third)
        final = VM(args, "final-restore", run)
        vms.append(final)
        final.restore(run / "C.state", run / "C.ram")
        final.api("PATCH", "/vm", {"state": "Resumed"})
        observed = final.command("read")
        expected = [hashlib.sha256(bytes([value]) * page_size).hexdigest() for value in (0x31, 165, 0, 0x34)]
        if observed["sha256"] != expected:
            raise AssertionError(f"restored guest marker oracle mismatch: {observed}")
        result = {"status": "passed", "M01": "native KVM File mechanism passed",
                  "M07": "native KVM A-B-C marker and full-memory oracle passed", "final_guest": observed,
                  "B": b_receipt, "C": c_receipt, "not_tested": ["orchestrator E2E", "shared NBD", "PFN sharing", "swap", "trace"]}
        final.close()
        vms.remove(final)
        if args.nbd_helper:
            result["shared_nbd"] = shared_nbd_check(args, run, offsets, page_size, vms, helpers)
            result["not_tested"] = ["orchestrator E2E", "swap", "pressure", "fault injection", "trace"]
            if args.attempt_pageout:
                result["M09"] = result["shared_nbd"]["M09"]
                if result["M09"]["status"] == "mechanism_only":
                    result["not_tested"].remove("swap")
                else:
                    result["status"] = "unaccepted"
        save_json(run / "result.json", result)
        success = True
    except BaseException as error:
        save_json(run / "failure.json", {"status": "failed", "error": repr(error),
                  "children": [{"pid": vm.process.pid, "name": vm.name, "exited": vm.process.poll()} for vm in vms],
                  "helpers": [{"pid": helper.process.pid, "exited": helper.process.poll()} for helper in helpers]})
        raise
    finally:
        if success or args.cleanup_failed:
            for vm in vms:
                vm.close()
            for helper in helpers:
                helper.close()
    print(json.dumps({"status": result["status"], "evidence": str(run)}))


if __name__ == "__main__":
    main()
