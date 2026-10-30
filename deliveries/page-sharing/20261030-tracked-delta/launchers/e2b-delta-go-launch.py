import subprocess,json,time
from pathlib import Path
base=Path('/home/coder/projects/yangshengxin/e2b/page-sharing-20261030/tracked-delta-01')
script='''#!/bin/sh
set -u
base=/home/coder/projects/yangshengxin/e2b/page-sharing-20261030/tracked-delta-01
export GOMAXPROCS=4
export GOCACHE=/home/coder/projects/yangshengxin/e2b/page-sharing-20261030/go-build
export GOMODCACHE=/home/coder/projects/yangshengxin/e2b/cache/go-mod
export GOPROXY=off
export GOSUMDB=off
export PATH=/home/coder/projects/yangshengxin/e2b/toolchains/go1.25.4/bin:/usr/local/bin:/usr/bin:/bin
cd "$base/infra/packages/orchestrator" || exit 92
go test -p 2 ./internal/cfg ./internal/sandbox/fc ./internal/sandbox/template ./internal/sandbox/uffd ./internal/sandbox/block ./internal/sandbox ./internal/sandbox/build ./internal/sandbox/nbd ./internal/sandbox/memorybacking ./internal/template/metadata ../shared/pkg/storage/header -run 'Test(Tracked|RAMExport|NoopMemory|ProcessMemory|ExportMemoryRanges|LogicalReadAt|ReadonlyDispatch|Dispatch|WriteFull|ReadonlyMount|Registry|HTTP|LocalSource|FileMemory|Materialize|Mapping|FactoryLifecycle|SnapshotPaths|Checkpoint|Pinned|LocalDiffOwnership|DiffPin|OwnedMemoryView|BootSourceOptionalInitrd)' -count=1 -v > "$base/go-tests.log" 2>&1
printf 'contracts\\t%s\\n' "$?" >> "$base/go-exit.tsv"
go test -p 2 -race ./internal/sandbox ./internal/sandbox/fc ./internal/sandbox/template ./internal/sandbox/memorybacking ./internal/sandbox/nbd -run 'Test(Tracked|Registry|ReadonlyDispatch|Dispatch|ReadonlyMount|FactoryLifecycle|SnapshotPaths|Checkpoint|Pinned)' -count=1 > "$base/go-race.log" 2>&1
printf 'race\\t%s\\n' "$?" >> "$base/go-exit.tsv"
go build -p 2 -o "$base/orchestrator" . > "$base/go-build.log" 2>&1
printf 'build\\t%s\\n' "$?" >> "$base/go-exit.tsv"
go test -p 2 -c -o "$base/factory-integration.test" ./internal/sandbox > "$base/go-integration-build.log" 2>&1
printf 'integration-build\\t%s\\n' "$?" >> "$base/go-exit.tsv"
date -u > "$base/go.done"
'''
(base/'go-driver.sh').write_text(script)
f=open(base/'go-driver.log','xb');p=subprocess.Popen(['sh',str(base/'go-driver.sh')],stdout=f,stderr=subprocess.STDOUT,start_new_session=True)
receipt={'pid':p.pid,'started':time.time(),'driver':str(base/'go-driver.sh')};(base/'go-driver.json').write_text(json.dumps(receipt,indent=2));print(json.dumps(receipt))
