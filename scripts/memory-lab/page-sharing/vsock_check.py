#!/usr/bin/env python3
"""Real active host/guest vsock traffic across frozen RAM + device restores."""
import argparse
from pathlib import Path
import tempfile
import socket
import struct
import threading
import time

import kvm_check as k


class Traffic:
    def __init__(self, path):
        self.path = str(path)
        self.stop = threading.Event()
        self.completed = 0
        self.reconnects = 0
        self.corrupt = []
        self.attempts = 0
        self.thread = threading.Thread(target=self.run)
        self.thread.start()

    def run(self):
        sequence = 0
        while not self.stop.is_set():
            self.attempts += 1
            try:
                with socket.socket(socket.AF_UNIX) as connection:
                    connection.settimeout(.5)
                    connection.connect(self.path)
                    connection.sendall(b'CONNECT 1234\n')
                    ack = b''
                    while not ack.endswith(b'\n'):
                        part = connection.recv(1)
                        if not part:
                            raise OSError('vsock handshake EOF')
                        ack += part
                        if len(ack) > 128:
                            raise OSError('invalid handshake')
                    assert ack.startswith(b'OK '), ack
                    while not self.stop.is_set():
                        sequence += 1
                        payload = bytearray((i * 17 + sequence) % 256 for i in range(65536))
                        struct.pack_into('<Q', payload, 0, sequence)
                        connection.sendall(payload)
                        reply = bytearray()
                        while len(reply) < len(payload):
                            part = connection.recv(len(payload) - len(reply))
                            if not part:
                                raise OSError('vsock EOF during transfer')
                            reply += part
                        if reply != payload:
                            self.corrupt.append(sequence)
                            return
                        self.completed += 1
            except (OSError, AssertionError):
                self.reconnects += 1
                self.stop.wait(.02)

    def close(self):
        self.stop.set()
        self.thread.join(5)
        assert not self.thread.is_alive()


def progress(vm, traffic, previous=0):
    deadline = time.monotonic() + 30
    while time.monotonic() < deadline:
        state = vm.command('state')
        assert state['corrupt'] == 0 and not traffic.corrupt
        if state['completed'] > previous + 10 and traffic.completed > 10:
            return state
        time.sleep(.05)
    raise AssertionError(('no restored vsock progress', state, traffic.completed, traffic.reconnects))


def freeze(vm, run, name, traffic):
    vm.api('PATCH', '/vm', {'state': 'Paused'})
    state = run / (name + '.state')
    vm.snapshot(state)
    attempts = traffic.attempts
    regions = vm.mappings()
    samples = []
    for sample in range(3):
        ram = run / (name + '-%d.ram' % sample)
        receipt = k.export_memory(vm, regions, ram)
        samples.append(receipt['sha256'])
        time.sleep(.2)
    assert len(set(samples)) == 1, samples
    vm.snapshot(run / (name + '.native.state'), run / (name + '.native.ram'))
    assert samples[0] == k.sha256_file(run / (name + '.native.ram'))
    assert traffic.attempts > attempts, 'host must continue connection attempts during freeze'
    row = {'stable_ram_samples': samples, 'native_ram_sha256': samples[0],
           'host_attempts_during_freeze': traffic.attempts - attempts,
           'host_echoes': traffic.completed, 'host_reconnects': traffic.reconnects}
    k.save_json(run / (name + '-freeze.json'), row)
    return state, ram


def main():
    parser = argparse.ArgumentParser()
    for name in ['firecracker', 'kernel', 'guest-binary', 'nbd-helper', 'output']:
        parser.add_argument('--' + name, required=True, type=Path)
    args = parser.parse_args()
    args.output = args.output.resolve()
    args.output.mkdir()
    run = args.output
    k.make_initrd(args.guest_binary, run / 'initrd.gz')
    socket_dir = Path(tempfile.mkdtemp(prefix='vsock-proof-'))
    vsock = socket_dir / 'vsock.sock'
    k.save_json(run / 'socket.json', {'path': str(vsock)})
    vms, helpers, streams = [], [], []
    try:
        seed = k.VM(args, 'seed', run)
        vms.append(seed)
        seed.api('PUT', '/machine-config', {'vcpu_count': 2, 'mem_size_mib': 128})
        seed.api('PUT', '/boot-source', {'kernel_image_path': str(args.kernel),
            'initrd_path': str(run / 'initrd.gz'), 'boot_args': 'console=ttyS0 reboot=k panic=1 pci=off'})
        seed.api('PUT', '/vsock', {'guest_cid': 3, 'uds_path': str(vsock)})
        seed.api('PUT', '/actions', {'action_type': 'InstanceStart'})
        seed.event('ready')
        traffic = Traffic(vsock)
        streams.append(traffic)
        initial = progress(seed, traffic)
        seed_state, seed_ram = freeze(seed, run, 'seed', traffic)
        traffic.close()
        streams.remove(traffic)
        seed.close()
        vms.remove(seed)
        vsock.unlink(missing_ok=True)
        variants = {}
        for mode in ['file', 'nbd']:
            backing, helper = seed_ram, None
            if mode == 'nbd':
                directory = run / 'source'
                directory.mkdir()
                helper = k.NBDHelper(args.nbd_helper, seed_ram, directory)
                helpers.append(helper)
                backing = Path(helper.ready['path'])
            vm = k.VM(args, mode, run)
            vms.append(vm)
            vm.restore(seed_state, backing)
            vm.api('PATCH', '/vm', {'state': 'Resumed'})
            traffic = Traffic(vsock)
            streams.append(traffic)
            checkpoints = []
            previous = initial['completed']
            for generation in range(3):
                state = progress(vm, traffic, previous)
                previous = state['completed']
                latest_state, latest_ram = freeze(vm, run, mode + '-' + str(generation), traffic)
                checkpoints.append(state)
                if generation < 2:
                    vm.api('PATCH', '/vm', {'state': 'Resumed'})
            traffic.close()
            streams.remove(traffic)
            vm.close()
            vms.remove(vm)
            vsock.unlink(missing_ok=True)
            restored = k.VM(args, mode + '-restored', run)
            vms.append(restored)
            restored.restore(latest_state, latest_ram)
            restored.api('PATCH', '/vm', {'state': 'Resumed'})
            resumed_traffic = Traffic(vsock)
            streams.append(resumed_traffic)
            final = progress(restored, resumed_traffic, previous)
            variants[mode] = {'checkpoints': checkpoints, 'restored': final,
                              'new_echoes_after_restore': resumed_traffic.completed}
            resumed_traffic.close()
            streams.remove(resumed_traffic)
            restored.close()
            vms.remove(restored)
            vsock.unlink(missing_ok=True)
            if helper:
                helper.close()
                helpers.remove(helper)
        k.save_json(run / 'result.json', {'status': 'passed', 'variants': variants,
            'contract': 'active connections reset; new connections and verified echoes progress after restore'})
    finally:
        for traffic in streams:
            traffic.close()
        for vm in vms:
            vm.close()
        for helper in helpers:
            helper.close()
        vsock.unlink(missing_ok=True)
        socket_dir.rmdir()


if __name__ == '__main__':
    main()
