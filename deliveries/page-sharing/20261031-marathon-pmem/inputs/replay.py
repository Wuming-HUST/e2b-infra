#!/usr/bin/env python3
"""Trace-driven synthetic memory replay, not re-execution of the original tools.

Uses all source turn/call IDs. Host-inserted checkpoints prove state continuity;
wall time here is NOT a workload performance result.
"""
import hashlib
import json
import mmap
import os
import resource
import struct
import subprocess
import time

cfg = json.load(open('/workspace/replay-config.json'))
trace_bytes = open('/workspace/trace.json', 'rb').read()
trace = json.loads(trace_bytes)
scale = cfg['time_scale'] / trace['time_scale']
state = mmap.mmap(-1, cfg['ws_mb'] << 20, flags=mmap.MAP_PRIVATE | mmap.MAP_ANONYMOUS)
state[:] = b'\xab' * len(state)
for off in range(0, len(state), 4096):
    struct.pack_into('<Q', state, off, off ^ 0x123456789ABCDEF0)
expected = hashlib.sha256(state).hexdigest()
root = '/workspace'
log = open(root + '/events.jsonl', 'w', buffering=1)
def mark(event, **kw):
    record = dict(event=event, mono=time.monotonic(), pid=os.getpid())
    record.update(kw)
    log.write(json.dumps(record) + '\n')
    log.flush(); os.fsync(log.fileno())
mark('start', trace_sha256=hashlib.sha256(trace_bytes).hexdigest(),
     source_sha256=trace['source_sha256'], normalized_sha256=trace['normalized_sha256'],
     turns=len(trace['turns']), source_time_scale=trace['time_scale'],
     replay_time_scale=cfg['time_scale'], evidence='trace-driven-synthetic-memory',
     checkpoint_turns=cfg['checkpoint_turns'], initial_sha256=expected,
     tool_work_policy='fixed-work-no-padding-v1')
tools = 0
for i, turn in enumerate(trace['turns']):
    if i in cfg['checkpoint_turns']:
        # Establish durable disk and anonymous live RAM at the same boundary.
        page = b'X' * 4096 if i == 10 else (bytes(4096) if i == 200 else b'R' * 4096)
        state[:4096] = page
        expected = hashlib.sha256(state).hexdigest()
        with open(root + '/zero-rewrite.bin', 'wb') as f:
            f.write(page); f.flush(); os.fsync(f.fileno())
        record = dict(turn=i, sha256=expected, pid=os.getpid(), disk_sha256=hashlib.sha256(page).hexdigest())
        with open(root + '/boundary.json', 'w') as f:
            json.dump(record, f); f.flush(); os.fsync(f.fileno())
        mark('checkpoint_ready', **record)
        go = root + '/continue-' + str(i)
        while not os.path.exists(go):
            time.sleep(.02)
        actual = hashlib.sha256(state).hexdigest()
        if actual != expected:
            raise RuntimeError('restored anonymous state mismatch')
        if json.load(open(root + '/boundary.json')) != record:
            raise RuntimeError('restored disk state mismatch')
        if hashlib.sha256(open(root + '/zero-rewrite.bin', 'rb').read()).hexdigest() != record['disk_sha256']:
            raise RuntimeError('zero/rewrite disk state mismatch')
        mark('checkpoint_resumed', turn=i, sha256=actual, disk_sha256=record['disk_sha256'], verify=True)
    time.sleep((turn.get('wait_s', 0) + turn.get('other_s', 0)) * scale)
    tool = turn.get('tool')
    if tool:
        # Touch every page using the original deterministic mtool allocation,
        # separate from retained state. It is a synthetic tool, not real rustc.
        for off in range(0, len(state), 4096):
            struct.pack_into('<Q', state, off, off ^ (i << 24) ^ 0x123456789ABCDEF0)
        expected = hashlib.sha256(state).hexdigest()
        cpu_before = resource.getrusage(resource.RUSAGE_CHILDREN)
        begin = time.monotonic()
        result = subprocess.run(['/usr/local/bin/memory-lab-mtool', str(tool['mb']),
                                 str(tool['dur'] * scale), root + '/corpus.bin', tool['kind']],
                                stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        if result.returncode:
            raise RuntimeError(f'tool failed {i}: {result.returncode} {result.stderr}')
        work = json.loads(result.stdout)
        expected_read = (16 << 20) if tool['kind'] in ('read', 'scan') else 0
        if (work['policy'] != 'fixed-work-no-padding-v1' or
            work['allocated_bytes'] != tool['mb'] << 20 or work['read_bytes'] != expected_read):
            raise RuntimeError(f'tool work mismatch: {work}')
        tool_wall = time.monotonic()-begin
        cpu_after = resource.getrusage(resource.RUSAGE_CHILDREN)
        tools += 1
        mark('tool_end', turn=i, source_step=turn['source_step'],
             call_ids=tool['call_ids'], kind=tool['kind'], modeled_mb=tool['mb'],
             seconds=tool_wall, user_cpu_s=cpu_after.ru_utime-cpu_before.ru_utime,
             system_cpu_s=cpu_after.ru_stime-cpu_before.ru_stime,
             trace_duration_s=tool['dur'] * scale, work_policy='fixed-work-no-padding-v1',
             work=work, corpus_bytes=expected_read, rc=result.returncode)
actual = hashlib.sha256(state).hexdigest()
if actual != expected:
    raise RuntimeError('final state mismatch')
mark('replay_done', tools=tools, turns=len(trace['turns']), final_sha256=actual, verify=True)
with open(root + '/result.json', 'w') as f:
    json.dump(dict(tools=tools, turns=len(trace['turns']), sha256=actual, pid=os.getpid(), verify=True), f)
    f.flush(); os.fsync(f.fileno())
