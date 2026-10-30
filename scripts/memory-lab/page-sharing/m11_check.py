#!/usr/bin/env python3
"""M11: real Async virtio-blk and TAP RX versus external checkpoint freeze.

Owns one TAP and only VM/helper children it starts. Keeps failed children unless
--cleanup-failed. A raw writable virtio disk is used, not an application rootfs.
The separate orchestrator/rootfs E2E remains required for application acceptance.
"""
import argparse
import gzip
import hashlib
import json
import os
from pathlib import Path
import platform
import shutil
import socket
import struct
import subprocess
import threading
import time
import kvm_check as k

CHUNK, SLOTS, WORKERS = 65536, 64, 4


def initrd_with_modules(binary, modules, destination):
    # Reuse the console-safe fixture archive and add a second newc archive in
    # one gzip stream. Linux initramfs explicitly permits concatenated archives.
    base = destination.with_suffix('.base.gz')
    k.make_initrd(binary, base)
    data = bytearray(gzip.decompress(base.read_bytes()))
    files = [('modules', 0o040755, b'')]
    for name in ('virtio_mmio','virtio_blk','failover','net_failover','virtio_net'):
        files.append(('modules/'+name+'.ko', 0o100644, (modules/(name+'.ko')).read_bytes()))
    files.append(('TRAILER!!!',0,b''))
    for index,(name,mode,content) in enumerate(files,100):
        encoded=name.encode()+b'\0'
        fields=[index,mode,0,0,1,0,len(content),0,0,0,0,len(encoded),0]
        data.extend(b'070701'+b''.join(f'{v:08x}'.encode() for v in fields))
        data.extend(encoded);data.extend(b'\0'*(-len(data)%4))
        data.extend(content);data.extend(b'\0'*(-len(data)%4))
    with gzip.open(destination,'wb') as stream:stream.write(data)
    base.unlink()


def disk_check(path):
    counts = [0] * WORKERS
    with path.open('rb', buffering=0) as source:
        for worker in range(WORKERS):
            for slot in range(SLOTS):
                record = source.read(CHUNK)
                if record == bytes(CHUNK):
                    continue
                if len(record) != CHUNK or record[:8] != b'M11BLK00':
                    raise AssertionError(f'disk torn/invalid header {worker=} {slot=}')
                owner, seq = struct.unpack_from('<QQ', record, 8)
                if owner != worker or seq % SLOTS != slot:
                    raise AssertionError(f'disk wrong slot {worker=} {slot=} {owner=} {seq=}')
                expected = bytearray((i * 17 + seq + worker * 29) % 256 for i in range(CHUNK))
                expected[:8] = b'M11BLK00'
                struct.pack_into('<QQ', expected, 8, worker, seq)
                if record != expected:
                    raise AssertionError(f'disk torn payload {worker=} {slot=} {seq=}')
                counts[worker] += 1
    if any(n == 0 for n in counts):
        raise AssertionError(f'no recorded disk I/O for some workers: {counts}')
    return {'valid_records_per_worker': counts, 'sha256': k.sha256_file(path)}


class Sender:
    def __init__(self, tap):
        self.stop = threading.Event()
        self.count = 0
        self.error = None
        self.sock = socket.socket(socket.AF_PACKET, socket.SOCK_RAW, socket.htons(3))
        self.sock.bind((tap, 0))
        self.thread = threading.Thread(target=self.send, daemon=True)
        self.thread.start()

    def send(self):
        try:
            while not self.stop.is_set():
                seq = self.count + 1
                payload = b'\x06\x00\x00\x00\x00\x01\x02\x00\x00\x00\x00\x02\x88\xb5M11NET00' + struct.pack('<Q', seq)
                payload += bytes((seq + i) % 256 for i in range(30, 1400))
                self.sock.send(payload)
                self.count += 1
                self.stop.wait(0.0005)
        except BaseException as e:
            self.error = repr(e)

    def close(self):
        self.stop.set()
        self.thread.join(timeout=5)
        self.sock.close()
        if self.error:
            raise RuntimeError(self.error)


def wait_progress(vm, previous=None):
    # Restored counters already include work done by the seed. A fresh sample
    # is required before checking actual post-restore progress.
    if previous is None:
        previous = vm.command('status')
    deadline = time.monotonic() + 30
    while time.monotonic() < deadline:
        got = vm.command('status')
        if got['errors']:
            raise AssertionError(got)
        if all(n > max(previous['completed'][i], 10) for i, n in enumerate(got['completed'])) and got['rx_packets'] > max(previous['rx_packets'], 10):
            return got
        time.sleep(0.05)
    raise TimeoutError(f'no block/network progress: {got}')


def frozen(vm, run, name, disk, sender):
    state = run / (name + '.state')
    # Sender stays active throughout pause, drain, state capture, export and
    # stable sampling; freeze must stop device RX writes as well as vCPUs.
    k.record_memory_stats(run, name+'-before-freeze', (vm,))
    before_packets = sender.count
    vm.api('PATCH', '/vm', {'state': 'Paused'})
    vm.snapshot(state)
    regions = vm.mappings()
    samples = []
    ram = None
    for index in range(3):
        ram = run / f'{name}-sample{index}.ram'
        receipt = k.export_memory(vm, regions, ram)
        samples.append({'ram_sha256': receipt['sha256'], 'disk_sha256': k.sha256_file(disk), 'sender_packets': sender.count, 'monotonic': time.monotonic()})
        time.sleep(0.15)
    if len({s['ram_sha256'] for s in samples}) != 1 or len({s['disk_sha256'] for s in samples}) != 1:
        raise AssertionError(f'RAM/disk mutated after external checkpoint freeze: {samples}')
    if sender.count <= before_packets + 10:
        raise AssertionError('network injector did not remain active during freeze')
    native = not os.environ.get('M11_DIAGNOSTIC_SKIP_NATIVE')
    if native:
        vm.snapshot(run / f'{name}.native.state', run / f'{name}.native.ram')
        if k.sha256_file(run / f'{name}.native.ram') != samples[0]['ram_sha256']:
            raise AssertionError('external export differs from same-frozen-state FC full dump')
    disk_receipt = disk_check(disk)
    disk_copy = run / f'{name}.disk'
    shutil.copyfile(disk, disk_copy)
    if k.sha256_file(disk_copy) != disk_receipt['sha256']:
        raise AssertionError('frozen block image copy mismatch')
    receipt = {'samples': samples, 'injected_during_freeze': sender.count - before_packets, 'disk_oracle': disk_receipt,
               'state_sha256': k.sha256_file(state), 'external_equals_native': True if native else None, 'ram': str(ram), 'state': str(state), 'disk': str(disk_copy)}
    k.save_json(run / f'{name}-freeze.json', receipt)
    return state, ram, disk_copy


def main():
    p = argparse.ArgumentParser()
    for name in ('firecracker','kernel','guest-binary','nbd-helper','module-dir','output'):
        p.add_argument('--'+name, required=True, type=Path)
    p.add_argument('--cleanup-failed', action='store_true')
    args = p.parse_args()
    for name in ('firecracker','kernel','guest_binary','nbd_helper'):
        setattr(args, name, getattr(args,name).resolve(strict=True))
    run = args.output.resolve(); run.mkdir(parents=True,exist_ok=False)
    tap = 'm11' + str(os.getpid())[-8:]
    vms, helpers, sender, tap_created, ok = [], [], None, False, False
    disk = run / 'active.disk'
    with disk.open('xb') as f: f.truncate(WORKERS*SLOTS*CHUNK)
    initrd_with_modules(args.guest_binary, args.module_dir, run / 'guest-initrd.gz')
    k.save_json(run / 'environment.json', {'uname':list(platform.uname()),'page_size':os.sysconf('SC_PAGE_SIZE'),'tap':tap,'fc_sha256':k.sha256_file(args.firecracker),'guest_sha256':k.sha256_file(args.guest_binary),'kernel_sha256':k.sha256_file(args.kernel),'requested_io_engine':'Async','ram_mib':128,'vcpus':2})
    try:
        subprocess.run(['ip','tuntap','add','dev',tap,'mode','tap'],check=True);tap_created=True
        subprocess.run(['ip','link','set',tap,'up'],check=True)
        vm = k.VM(args,'seed',run);vms.append(vm)
        vm.api('PUT','/machine-config',{'vcpu_count':2,'mem_size_mib':128})
        vm.api('PUT','/boot-source',{'kernel_image_path':str(args.kernel),'initrd_path':str(run/'guest-initrd.gz'),'boot_args':'console=ttyS0 reboot=k panic=1 pci=off'})
        vm.api('PUT','/drives/io',{'drive_id':'io','path_on_host':str(disk),'is_root_device':False,'is_read_only':False,'io_engine':'Async'})
        vm.api('PUT','/network-interfaces/io',{'iface_id':'io','guest_mac':'06:00:00:00:00:01','host_dev_name':tap})
        vm.api('PUT','/actions',{'action_type':'InstanceStart'})
        vm.event('ready');sender=Sender(tap)
        fd_links = {entry.name: os.readlink(entry) for entry in Path(f'/proc/{vm.process.pid}/fd').iterdir()}
        k.save_json(run/'seed-fd-links.json', fd_links)
        if not any('io_uring' in value for value in fd_links.values()):
            raise AssertionError('requested Async block engine has no live io_uring FD')
        k.save_json(run/'seed-progress.json',wait_progress(vm))
        seed_state,seed_ram,seed_disk=frozen(vm,run,'seed',disk,sender)
        vm.close();vms.remove(vm)
        results={}
        for variant in ('B0-file','B1-nbd'):
            shutil.copyfile(seed_disk,disk)
            backing=seed_ram
            helper=None
            if variant=='B1-nbd':
                helper_run=run/'shared-source';helper_run.mkdir()
                helper=k.NBDHelper(args.nbd_helper,seed_ram,helper_run);helpers.append(helper)
                if helper.ready['content_id'] != 'sha256:'+k.sha256_file(seed_ram) or helper.ready['logical_size'] != seed_ram.stat().st_size:
                    raise AssertionError('NBD ready identity/size differs from the frozen seed')
                backing=Path(helper.ready['path'])
            vm=k.VM(args,variant,run);vms.append(vm)
            vm.restore(seed_state,backing)
            vm.api('PATCH','/vm',{'state':'Resumed'})
            progress=[]
            for iteration in range(3):
                progress.append(wait_progress(vm,progress[-1] if progress else None))
                last_state,last_ram,last_disk=frozen(vm,run,f'{variant}-{iteration}',disk,sender)
                if iteration<2:vm.api('PATCH','/vm',{'state':'Resumed'})
            k.save_json(run/f'{variant}-progress.json',progress)
            vm.close();vms.remove(vm)
            restored=k.VM(args,variant+'-verify',run);vms.append(restored)
            restored.restore(last_state,last_ram);restored.api('PATCH','/vm',{'state':'Resumed'})
            after=wait_progress(restored,progress[-1]);k.save_json(run/f'{variant}-restored-progress.json',after)
            # Check a second frozen checkpoint after resumed pending I/O ran.
            frozen(restored,run,f'{variant}-restored',disk,sender)
            restored.close();vms.remove(restored)
            if helper:helper.close();helpers.remove(helper)
            results[variant]={'freeze_iterations':3,'restore_io_and_network_progress':True,'errors':after['errors']}
        k.save_json(run/'result.json',{'status':'diagnostic' if os.environ.get('M11_DIAGNOSTIC_SKIP_NATIVE') else 'passed','variants':results,'oracle':'3 full-RAM/disk stable samples per freeze, same-frozen-state FC full dump, every written raw-disk record, restored block and RX progress','scope':'Async virtio-blk raw disk + TAP network, not application rootfs trace or vsock','sender_packets':sender.count})
        ok=True
    finally:
        if ok or args.cleanup_failed:
            for vm in vms[:]: vm.close();vms.remove(vm)
            for helper in helpers[:]:helper.close();helpers.remove(helper)
            if sender:sender.close()
            if tap_created:subprocess.run(['ip','link','delete',tap],check=True)
        k.save_json(run/'cleanup.json',{'success':ok,'retained_vm_pids':[v.process.pid for v in vms],'retained_helper_pids':[h.process.pid for h in helpers],'tap_retained':tap_created and not(ok or args.cleanup_failed)})

if __name__=='__main__': main()
