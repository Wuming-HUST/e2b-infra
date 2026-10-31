#!/usr/bin/env python3
"""Create a deterministic initramfs, including PID 1's initial console nodes."""
import gzip
import sys
from pathlib import Path

def make(binary,output):
    data=bytearray()
    entries=[('.',0o040755,b'',0,0),('dev',0o040755,b'',0,0),('proc',0o040755,b'',0,0),('sys',0o040755,b'',0,0),
             ('dev/console',0o020600,b'',5,1),('dev/null',0o020666,b'',1,3),('init',0o100755,Path(binary).read_bytes(),0,0),('TRAILER!!!',0,b'',0,0)]
    for index,(name,mode,content,major,minor) in enumerate(entries,1):
        encoded=name.encode()+b'\0'
        fields=[index,mode,0,0,1,0,len(content),0,0,major,minor,len(encoded),0]
        data.extend(b'070701'+b''.join(f'{value:08x}'.encode() for value in fields));data.extend(encoded);data.extend(b'\0'*(-len(data)%4));data.extend(content);data.extend(b'\0'*(-len(data)%4))
    with Path(output).open('xb') as f:
        with gzip.GzipFile(filename='',mode='wb',fileobj=f,mtime=0) as z:z.write(data)

if __name__=='__main__':
    if len(sys.argv)!=3:raise SystemExit('usage: make-initramfs.py STATIC_INIT NEW_OUTPUT.gz')
    make(sys.argv[1],sys.argv[2])
