#!/usr/bin/env python3
"""Stream owned campaign evidence to this machine; verify every archived byte.

No remote temporary archive or deletion. --all-artifacts preserves large raw
RAM images as well; use only after confirming that the named campaigns stopped.
"""
import argparse
import hashlib
import json
from pathlib import Path
import shlex
import subprocess
import tarfile

REMOTE = r'''
import hashlib, io, json, os, stat, sys, tarfile
from pathlib import Path
base=Path('/home/coder/projects/yangshengxin/e2b/page-sharing-20261030/pmem-dax-01')
campaigns=json.loads(sys.argv[1]); limit=int(sys.argv[2]); rows=[]
class HashedReader:
 def __init__(self, f): self.f=f; self.digest=hashlib.sha256()
 def read(self, size):
  data=self.f.read(size); self.digest.update(data); return data
with tarfile.open(fileobj=sys.stdout.buffer,mode='w|gz') as archive:
 for name in campaigns:
  if '/' in name or name.startswith('.') or not (base/name).is_dir():
   raise RuntimeError('Invalid or absent campaign '+name)
  for root, dirs, files in os.walk(base/name,followlinks=False):
   # Test/source copies and build caches are represented by source hash receipts.
   dirs[:]=sorted(x for x in dirs if x not in ['infra','.git','target','host-lib','uapi'])
   for filename in sorted(files):
    path=Path(root)/filename; before=path.lstat()
    if not stat.S_ISREG(before.st_mode): continue
    relative=str(path.relative_to(base))
    if before.st_size>limit:
     rows.append({'path':relative,'archived':False,'size':before.st_size}); continue
    with path.open('rb') as stream:
     hashed=HashedReader(stream); archive.addfile(archive.gettarinfo(str(path),arcname=relative),hashed)
    after=path.stat()
    if (before.st_size,before.st_mtime_ns,before.st_ino)!=(after.st_size,after.st_mtime_ns,after.st_ino):
     raise RuntimeError('File changed during collection '+relative)
    rows.append({'path':relative,'archived':True,'size':before.st_size,'sha256':hashed.digest.hexdigest()})
 data=json.dumps({'remote_base':str(base),'campaigns':campaigns,'files':rows},indent=2).encode()+b'\n'
 info=tarfile.TarInfo('collection-manifest.json'); info.size=len(data); archive.addfile(info,io.BytesIO(data))
'''


def verify(path):
    observed, manifest = {}, None
    with tarfile.open(path, 'r|gz') as archive:
        for member in archive:
            assert member.isfile() and member.name not in observed
            stream = archive.extractfile(member)
            if member.name == 'collection-manifest.json':
                assert manifest is None
                manifest = json.load(stream)
                continue
            digest = hashlib.sha256()
            size = 0
            for chunk in iter(lambda: stream.read(8 << 20), b''):
                digest.update(chunk)
                size += len(chunk)
            observed[member.name] = {'size': size, 'sha256': digest.hexdigest()}
    assert manifest is not None
    expected = {row['path']: {'size': row['size'], 'sha256': row['sha256']}
                for row in manifest['files'] if row['archived']}
    assert observed == expected, 'Archive content does not match remote byte manifest'
    return manifest


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--output', type=Path, required=True)
    parser.add_argument('--all-artifacts', action='store_true')
    parser.add_argument('--verify-only', action='store_true')
    parser.add_argument('campaigns', nargs='*')
    args = parser.parse_args()
    if not args.verify_only:
        assert args.campaigns
        # A successful archive is never replaced; interrupted transfers retain .partial.
        assert not args.output.exists()
        part = args.output.with_suffix(args.output.suffix + '.partial')
        args.output.parent.mkdir(parents=True, exist_ok=True)
        limit = (1 << 63) - 1 if args.all_artifacts else 32 << 20
        inner = 'sudo -n python3 -c ' + shlex.quote(REMOTE) + ' ' + shlex.quote(json.dumps(args.campaigns)) + ' ' + str(limit)
        outer = 'ssh -o BatchMode=yes -o ConnectTimeout=15 agent03.coder ' + shlex.quote(inner)
        with part.open('xb') as output, part.with_suffix(part.suffix + '.stderr').open('xb') as log:
            result = subprocess.run(['ssh', '-o', 'BatchMode=yes', '-o', 'ConnectTimeout=15', 'H-arch', outer],
                                    stdout=output, stderr=log)
        assert result.returncode == 0, 'SSH failed; partial archive and stderr retained'
        manifest = verify(part)
        part.rename(args.output)
    else:
        manifest = verify(args.output)
    digest = hashlib.sha256()
    with args.output.open('rb') as stream:
        for chunk in iter(lambda: stream.read(8 << 20), b''):
            digest.update(chunk)
    receipt = {'archive': args.output.name, 'sha256': digest.hexdigest(),
               'verified_files': sum(row['archived'] for row in manifest['files']),
               'manifest': manifest}
    destination = args.output.with_suffix(args.output.suffix + '.verified.json')
    destination.write_text(json.dumps(receipt, indent=2) + '\n')
    print(json.dumps({key: receipt[key] for key in ['archive', 'sha256', 'verified_files']}))


if __name__ == '__main__':
    main()
