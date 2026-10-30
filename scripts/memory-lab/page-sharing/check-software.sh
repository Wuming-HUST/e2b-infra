#!/usr/bin/env bash
set -uo pipefail
# Run from an isolated copy. Supply Go caches explicitly when operating offline.
project_root=$(cd "$(dirname "$0")/../../.." && pwd)
result_dir=${1:?usage: check-software.sh NEW_RESULT_DIR}
mkdir "$result_dir" || exit 1
result_dir=$(cd "$result_dir" && pwd)
cd "$project_root/packages/orchestrator" || exit 1
export GOMAXPROCS=${GOMAXPROCS:-4}
status=0
run_check() {
    local name=$1
    shift
    "$@" >"$result_dir/$name.log" 2>&1
    local rc=$?
    printf '%s\t%d\n' "$name" "$rc" >>"$result_dir/exit-codes.tsv"
    if ((rc != 0)); then status=1; fi
}
run_check contracts go test -p 2 ./internal/sandbox ./internal/sandbox/memorybacking ./internal/sandbox/nbd ./internal/sandbox/template ./internal/sandbox/uffd ./internal/sandbox/uffd/memory ./internal/sandbox/fc ./internal/sandbox/build ./internal/sandbox/block -run 'Test(NoopMemory|ProcessMemory|ExportMemoryRanges|LogicalReadAt|ReadonlyDispatch|Dispatch|WriteFull|ReadonlyMount|Registry|HTTP|LocalSource|FileMemory|Materialize|Mapping|FactoryLifecycle|SnapshotPaths|Checkpoint|Pinned|LocalDiffOwnership|DiffPin|OwnedMemoryView)' -count=1 -v
run_check race go test -p 2 -race ./internal/sandbox/memorybacking ./internal/sandbox/nbd ./internal/sandbox ./internal/sandbox/build ./internal/sandbox/template -run 'Test(Registry|HTTP|LocalSource|ReadonlyDispatch|Dispatch|WriteFull|ReadonlyMount|FactoryLifecycle|SnapshotPaths|Checkpoint|Pinned|LocalDiffOwnership|DiffPin|OwnedMemoryView)' -count=1
run_check headers go test -p 2 ../shared/pkg/storage/header ./internal/template/metadata -count=1
run_check orchestrator go build -p 2 -o "$result_dir/orchestrator" .
run_check nbd-helper go build -p 2 -o "$result_dir/page-sharing-source" ./cmd/page-sharing-source
run_check nbd-test-binary go test -p 2 -c -o "$result_dir/nbd.test" ./internal/sandbox/nbd
run_check guest env GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o "$result_dir/guest-init" "$project_root/scripts/memory-lab/page-sharing/guest.go"
cat "$result_dir/exit-codes.tsv"
exit "$status"
