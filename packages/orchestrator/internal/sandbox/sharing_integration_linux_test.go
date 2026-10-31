//go:build linux

package sandbox

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/metric/noop"

	"github.com/e2b-dev/infra/packages/orchestrator/internal/cfg"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/block"
	blockmetrics "github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/block/metrics"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/fc"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/nbd"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/network"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/pmem"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/rootfs"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/template"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/template/metadata"
	"github.com/e2b-dev/infra/packages/shared/pkg/consts"
	featureflags "github.com/e2b-dev/infra/packages/shared/pkg/feature-flags"
	sharedgrpc "github.com/e2b-dev/infra/packages/shared/pkg/grpc"
	"github.com/e2b-dev/infra/packages/shared/pkg/grpc/envd/process"
	"github.com/e2b-dev/infra/packages/shared/pkg/grpc/envd/process/processconnect"
	"github.com/e2b-dev/infra/packages/shared/pkg/grpc/orchestrator"
	"github.com/e2b-dev/infra/packages/shared/pkg/logger"
	sbxlogger "github.com/e2b-dev/infra/packages/shared/pkg/logger/sandbox"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage/header"
	"github.com/e2b-dev/infra/packages/shared/pkg/stresstest"
)

// This is deliberately gated: it creates only its own VMs, NBD devices and
// high-numbered network slots. It uses real envd and the production Factory,
// Pause, upload and cache restore paths; it does not deploy platform services.
func TestSharingFactoryIntegration(t *testing.T) { //nolint:paralleltest
	runSharingFactoryIntegration(t, false)
}
func TestPmemFactoryIntegration(t *testing.T) { //nolint:paralleltest
	dir := os.Getenv("E2B_PMEM_INTEGRATION_DIR")
	if dir == "" {
		t.Skip("set E2B_PMEM_INTEGRATION_DIR and explicit pmem fixture inputs")
	}
	t.Setenv("E2B_SHARING_INTEGRATION_DIR", dir)
	runSharingFactoryIntegration(t, true)
}
func runSharingFactoryIntegration(t *testing.T, pmemMode bool) { //nolint:paralleltest
	proofMode := pmemMode && os.Getenv("E2B_SHARING_PROOF") == "1"
	compatMode := proofMode && os.Getenv("E2B_PMEM_COMPAT") == "1"
	marathonMode := pmemMode && os.Getenv("E2B_PMEM_MARATHON") == "1"
	remoteFailureMode := os.Getenv("E2B_SHARING_REMOTE_FAILURE")
	runDir := os.Getenv("E2B_SHARING_INTEGRATION_DIR")
	if runDir == "" {
		t.Skip("set E2B_SHARING_INTEGRATION_DIR and explicit fixture inputs on an isolated Linux KVM node")
	}
	require.Equal(t, 0, os.Geteuid(), "integration needs root for its own KVM/NBD/network resources")
	require.True(t, filepath.IsAbs(runDir))
	require.NoError(t, os.Mkdir(runDir, 0o700), "refuse to reuse an earlier run directory")
	input := func(name string) string {
		v := os.Getenv(name)
		require.True(t, filepath.IsAbs(v), "%s must be an absolute input path", name)
		_, err := os.Stat(v)
		require.NoError(t, err, name)
		return v
	}
	fcPath, kernel := input("E2B_SHARING_FC"), input("E2B_SHARING_KERNEL")
	rootfsPath, initrd := input("E2B_SHARING_ROOTFS"), input("E2B_SHARING_INITRD")
	fixturePath := input("E2B_SHARING_TRACE_FIXTURE")
	init := os.Getenv("E2B_SHARING_INIT")
	if init == "" {
		init = "/sbin/sharing-init"
	}
	fixtureBytes, err := os.ReadFile(fixturePath)
	require.NoError(t, err)
	var fixture sharingTraceFixture
	require.NoError(t, json.Unmarshal(fixtureBytes, &fixture))
	require.False(t, fixture.CompleteOriginalTrace)
	require.Equal(t, "6345988d0e84ca897b54a875fa6b4a4fe879b620ad095d3a61bfde825490d9e2", fixture.Source.SHA256)
	require.Len(t, fixture.Tools, 5)
	duration := 8 * time.Minute
	if marathonMode {
		duration = 15 * time.Minute
	}
	ctx, cancel := context.WithTimeout(context.Background(), duration)
	defer cancel()
	// Allocate NBD with kernel IndexAny; do not enumerate or retarget devices
	// that belong to another experiment on this shared node.
	t.Setenv(stresstest.Env, "true")
	sbxlogger.SetSandboxLoggerInternal(logger.NewNopLogger())
	sbxlogger.SetSandboxLoggerExternal(logger.NewNopLogger())
	flags := &featureflags.Client{}
	netConfig, err := network.ParseConfig()
	require.NoError(t, err)
	baseIndex := 25000
	if value := os.Getenv("E2B_SHARING_SLOT_BASE"); value != "" {
		baseIndex, err = strconv.Atoi(value)
		require.NoError(t, err)
	}
	store := &sharingNetworkStorage{config: netConfig, next: baseIndex, limit: baseIndex + 5}
	if proofMode || marathonMode {
		store.limit = baseIndex + 8
	}
	networkPool := network.NewPool(2, 1, store, netConfig)
	go networkPool.Populate(ctx)
	devices, err := nbd.NewDevicePool()
	require.NoError(t, err)
	go devices.Populate(ctx)
	t.Setenv("STORAGE_PROVIDER", "Local")
	t.Setenv("LOCAL_TEMPLATE_STORAGE_BASE_PATH", filepath.Join(runDir, "objects"))
	persistence, err := storage.GetTemplateStorageProvider(ctx, nil)
	require.NoError(t, err)
	var factories []*Factory
	var caches []*template.Cache
	var expectedFailureDrain *Factory
	t.Cleanup(func() {
		cleanCtx := context.Background()
		// Factory drain stops FC before releasing source leases and NBD devices.
		for _, factory := range factories {
			factory.BeginShutdown()
		}
		drained := true
		for _, factory := range factories {
			if err := factory.DrainMemoryBackings(cleanCtx); err != nil {
				if factory == expectedFailureDrain {
					// The fault test has already checked every process exit and
					// successful registry disconnect independently of this error.
					sharingReceipt(t, filepath.Join(runDir, "expected-failure-drain.json"), map[string]any{"error": err.Error()})
					continue
				}
				t.Errorf("factory drain failed; retain sources/pools for diagnosis: %v", err)
				drained = false
			}
		}
		if !drained {
			return
		}
		for _, cache := range caches {
			cache.Stop()
		}
		cancel()
		if err := networkPool.Close(cleanCtx); err != nil {
			t.Errorf("network cleanup: %v", err)
		}
		if err := devices.Close(cleanCtx); err != nil {
			t.Errorf("NBD cleanup: %v", err)
		}
	})
	exportMode := os.Getenv("E2B_SHARING_RAM_EXPORT_MODE")
	require.NoError(t, (cfg.BuilderConfig{RAMExportMode: exportMode}).ValidateRAMExportMode())
	configFor := func(mode string) cfg.Config {
		base := filepath.Join(runDir, mode)
		config := cfg.Config{BuilderConfig: cfg.BuilderConfig{
			FirecrackerNetnsExecHelper: "disabled", FirecrackerVersionsDir: filepath.Join(base, "fc"),
			HostKernelsDir: filepath.Join(base, "kernels"), SandboxDir: filepath.Join(runDir, "sandbox"),
			OrchestratorBaseDir: base, DefaultCacheDir: filepath.Join(base, "build-cache"),
			TemplatesDir: filepath.Join(base, "templates"), EnvdTimeout: 30 * time.Second,
			FileMemoryBackendEnabled: true, FileMemoryBackendDir: filepath.Join(base, "file-memory"),
			PmemRootfsEnabled: pmemMode, PmemRootfsCacheDir: filepath.Join(base, "pmem-lower"),
			SharedNBDMemoryEnabled: mode == "B1", RAMExportMode: exportMode, NetworkConfig: netConfig,
			StorageConfig: storage.Config{SandboxCacheDir: filepath.Join(base, "rootfs-cow"),
				SnapshotCacheDir: filepath.Join(base, "snapshot-cache"), TemplateCacheDir: filepath.Join(base, "template-cache")},
		}}
		for _, path := range []string{config.DefaultCacheDir, config.SandboxDir, config.StorageConfig.SandboxCacheDir,
			config.StorageConfig.SnapshotCacheDir, config.StorageConfig.TemplateCacheDir,
			filepath.Join(config.HostKernelsDir, "sharing"), filepath.Join(config.FirecrackerVersionsDir, "sharing")} {
			require.NoError(t, os.MkdirAll(path, 0o700))
		}
		require.NoError(t, os.Symlink(fcPath, filepath.Join(config.FirecrackerVersionsDir, "sharing", fc.FirecrackerBinaryName)))
		require.NoError(t, os.Symlink(kernel, filepath.Join(config.HostKernelsDir, "sharing", fc.SandboxKernelFile)))
		return config
	}
	makeFactory := func(config cfg.Config) (*Factory, *template.Cache) {
		metrics, err := blockmetrics.NewMetrics(noop.NewMeterProvider())
		require.NoError(t, err)
		cache, err := template.NewCache(config, flags, persistence, metrics)
		require.NoError(t, err)
		cache.Start(ctx)
		caches = append(caches, cache)
		factory := NewFactory(config.BuilderConfig, networkPool, devices, flags, nil, nil)
		factories = append(factories, factory)
		return factory, cache
	}
	b0Config := configFor("B0")
	b0, b0Cache := makeFactory(b0Config)
	b1, b1Cache := makeFactory(configFor("B1"))
	var controlFactory *Factory
	var controlCache *template.Cache
	if proofMode {
		controlFactory, controlCache = makeFactory(configFor("B2-control"))
	}
	config := Config{Vcpu: 1, RamMB: 256, HugePages: false, Network: &orchestrator.SandboxNetworkConfig{},
		Envd:              EnvdMetadata{Vars: map[string]string{}, Version: "1.0.0"},
		FirecrackerConfig: fc.Config{KernelVersion: "sharing", FirecrackerVersion: "sharing"}}
	if marathonMode {
		config.Vcpu, config.RamMB = 2, 1536
	}
	user, workdir, token := "root", "/workspace", "sharing-integration"
	config.Envd.DefaultUser, config.Envd.DefaultWorkdir, config.Envd.AccessToken = &user, &workdir, &token
	runtimeFor := func() RuntimeMetadata {
		id := uuid.NewString()[:8]
		return RuntimeMetadata{SandboxID: "sharing-" + id, ExecutionID: uuid.NewString(), TemplateID: "sharing-integration", TeamID: "sharing"}
	}
	// The initial disk is a read-only source. Only per-VM NBD COW files change.
	baseID := uuid.New()
	disk, err := block.NewLocal(rootfsPath, header.RootfsBlockSize, baseID)
	require.NoError(t, err)
	defer disk.Close()
	baseDiskObject, err := persistence.OpenSeekable(ctx, storage.TemplateFiles{BuildID: baseID.String()}.StorageRootfsPath(), storage.RootFSObjectType)
	require.NoError(t, err)
	require.NoError(t, baseDiskObject.StoreFile(ctx, rootfsPath))
	zeroRAM := filepath.Join(runDir, "cold-ram-size-only")
	z, err := os.OpenFile(zeroRAM, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	require.NoError(t, err)
	require.NoError(t, z.Truncate(int64(config.RamMB)*1024*1024))
	require.NoError(t, z.Close())
	ram, err := block.NewLocal(zeroRAM, 4096, baseID)
	require.NoError(t, err)
	defer ram.Close()
	baseFiles, err := (storage.TemplateFiles{BuildID: baseID.String()}).CacheFiles(b0Config.StorageConfig)
	require.NoError(t, err)
	var base template.Template = template.NewLocalTemplate(baseFiles, disk, ram)
	if pmemMode {
		layoutBytes, err := os.ReadFile(input("E2B_PMEM_LAYOUT"))
		require.NoError(t, err)
		var layout pmem.Layout
		require.NoError(t, json.Unmarshal(layoutBytes, &layout))
		require.NoError(t, layout.Validate())
		base, err = template.NewPmemTemplate(base, metadata.Template{Rootfs: &layout}, pmem.OpenLocal(input("E2B_PMEM_LOWER")))
		require.NoError(t, err)
	}
	serial, err := os.OpenFile(filepath.Join(runDir, "cold-serial.log"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	require.NoError(t, err)
	defer serial.Close()
	cold, err := b0.CreateSandbox(ctx, config, runtimeFor(), base, 10*time.Minute, "", fc.ProcessOptions{
		InitScriptPath: init, InitrdPath: initrd, KernelLogs: true, Stdout: serial, Stderr: serial}, nil)
	require.NoError(t, err)
	sharingRecordPID(t, runDir, "cold", cold)
	t.Log("cold VM created; waiting for real envd")
	require.NoError(t, cold.WaitForEnvd(ctx, time.Minute))
	if pmemMode {
		// Compare underlying mount options and real root before application work.
		result, err := sharingExec(ctx, cold, "cat /proc/mounts; test -d /.e2b/lower; test -d /.e2b/writable/upper")
		require.NoError(t, err)
		sharingReceipt(t, filepath.Join(runDir, "pmem-mounts.json"), map[string]any{"output": result})
	}
	sharingRunTools(t, ctx, cold, fixture.Tools[:2], runDir)
	sharingCheckFiles(t, ctx, cold, fixture.Checkpoints["after_writes"].ExpectedFiles)
	if pmemMode {
		sharingPmemWriteState(t, ctx, cold)
	}
	if proofMode {
		sharingProofStart(t, ctx, cold)
	}
	if compatMode {
		sharingCompatStart(t, ctx, cold)
	}
	pauseUpload := func(name string, sbx *Sandbox) string {
		t.Logf("%s: freeze, compare native RAM, production Pause and upload", name)
		_, err := sharingExec(ctx, sbx, "sync")
		require.NoError(t, err)
		sbx.Checks.Stop()
		require.NoError(t, sbx.process.Pause(ctx))
		prefix := filepath.Join(runDir, name)
		// External checkpoint freezes every device writer. The native full dump
		// and production Pause export below observe this same frozen RAM state.
		require.NoError(t, sbx.process.CreateSnapshot(ctx, prefix+"-freeze.snap"))
		tracked := false
		var candidateHash string
		var candidateSize int64
		var candidatePages uint
		if backend, ok := sbx.Resources.memory.(interface{ RAMExportMode() string }); ok && backend.RAMExportMode() == "tracked_delta" {
			tracked = true
			// Select and capture BEFORE the full oracle faults in cold RAM.
			candidates, err := sbx.Resources.memory.DiffMetadata(ctx, sbx.process)
			require.NoError(t, err)
			candidatePages = candidates.Dirty.Count()
			capture, err := sbx.process.ExportMemory(ctx, candidates.Dirty, prefix+"-candidates.ram", 4096)
			require.NoError(t, err)
			candidateHash, candidateSize = sharingHashFile(t, prefix+"-candidates.ram")
			require.NoError(t, capture.Close())
		}
		require.NoError(t, sharingFCRequest(ctx, sbx, "/snapshot/create", map[string]any{
			"snapshot_type": "Full", "snapshot_path": prefix + "-native.snap", "mem_file_path": prefix + "-native.ram"}))
		buildID := uuid.NewString()
		meta := metadata.Template{Version: metadata.CurrentVersion, Template: metadata.TemplateMetadata{
			BuildID: buildID, KernelVersion: "sharing", FirecrackerVersion: "sharing"}}
		snapshot, err := sbx.Pause(ctx, meta)
		require.NoError(t, err)
		defer snapshot.Close(context.Background())
		memPath, err := snapshot.MemfileDiff.CachePath()
		require.NoError(t, err)
		nativeHash, nativeSize := sharingHashFile(t, prefix+"-native.ram")
		exportedHash, exportedSize := sharingHashFile(t, memPath)
		if tracked {
			require.Equal(t, "tracked_delta", snapshot.RAMExportMode)
			require.Equal(t, int64(candidatePages)*4096, exportedSize)
			require.Equal(t, candidateSize, exportedSize)
			require.Equal(t, candidateHash, exportedHash, "production candidates changed after the full oracle read")
		} else {
			require.Equal(t, nativeSize, exportedSize)
			require.Equal(t, nativeHash, exportedHash, "native and production full_logical export differ")
		}
		require.NoError(t, snapshot.Upload(ctx, persistence, storage.TemplateFiles{BuildID: buildID}))
		if tracked {
			// Resolve through the production layered source: this reads both the
			// new packed payload and every inherited historical BuildID as needed.
			resolved, err := b1Cache.GetTemplate(ctx, buildID, false, false)
			require.NoError(t, err)
			require.NoError(t, template.ValidateCheckpoint(ctx, resolved))
			logical, err := resolved.Memfile(ctx)
			require.NoError(t, err)
			digest := sha256.New()
			buffer := make([]byte, 1<<20)
			for off := int64(0); off < nativeSize; off += int64(len(buffer)) {
				chunk := buffer[:min(int64(len(buffer)), nativeSize-off)]
				n, err := logical.ReadAt(ctx, chunk, off)
				require.NoError(t, err)
				require.Equal(t, len(chunk), n)
				_, _ = digest.Write(chunk)
			}
			require.Equal(t, nativeHash, hex.EncodeToString(digest.Sum(nil)), "parent + production delta differs from complete frozen RAM")
			require.Less(t, exportedSize, nativeSize, "test workload must demonstrate a real partial capture")
		}
		sharingReceipt(t, prefix+"-checkpoint.json", map[string]any{"build_id": buildID,
			"payload_sha256": exportedHash, "payload_size": exportedSize, "ram_sha256": nativeHash, "ram_size": nativeSize,
			"candidate_pages": candidatePages, "tracked_delta": tracked, "native_same_frozen_state": true})
		return buildID
	}
	aID := pauseUpload("A", cold)
	resume := func(name, buildID string, factory *Factory, cache *template.Cache) *Sandbox {
		t.Logf("%s: cache template and resume", name)
		// A completed snapshot is used as a reusable template here. Passing
		// isSnapshot=true deliberately disables the File backend in production.
		tmpl, err := cache.GetTemplate(ctx, buildID, false, false)
		require.NoError(t, err)
		var fileRAM string
		if factory != b1 {
			require.Eventually(t, func() bool {
				var ready bool
				fileRAM, ready = template.FileMemoryBackendPath(tmpl)
				return ready
			}, 30*time.Second, 10*time.Millisecond, "B0 File RAM must materialize before restore")
		}
		now := time.Now()
		sbx, err := factory.ResumeSandbox(ctx, tmpl, config, runtimeFor(), now, now.Add(10*time.Minute), nil)
		require.NoError(t, err)
		sharingRecordPID(t, runDir, name, sbx)
		if factory != b1 {
			_, leased := sbx.Resources.memory.(*leasedMemory)
			require.False(t, leased, "B0 must not use shared NBD")
			sharingCheckFileRAMMapping(t, runDir, name, sbx, fileRAM)
		}
		require.NoError(t, sbx.WaitForEnvd(ctx, time.Minute))
		if pmemMode {
			sharingPmemCheckState(t, ctx, sbx)
		}
		return sbx
	}
	if remoteFailureMode != "" {
		sharingRemoteFailure(t, ctx, runDir, remoteFailureMode, b1, devices, flags,
			func(name string) *Sandbox { return resume(name, aID, b1, b1Cache) })
		expectedFailureDrain = b1
		return
	}
	if marathonMode {
		current := resume("marathon-start", aID, b1, b1Cache)
		current = sharingMarathonReplay(t, ctx, runDir, current, pauseUpload, func(name, buildID string) *Sandbox {
			return resume(name, buildID, b1, b1Cache)
		})
		sharingProofUnchangedSources(t, ctx, runDir, "marathon", current, b1Cache, aID)
		require.NoError(t, current.Close(ctx))
		return
	}
	// Both modes start from the exact same A disk/RAM/state and use the same
	// parameters, tool payloads and byte oracles. No baseline consumes B1 markers.
	for _, mode := range []string{"B0", "B1"} {
		factory, cache := b0, b0Cache
		if mode == "B1" {
			factory, cache = b1, b1Cache
		}
		left := resume(mode+"-left", aID, factory, cache)
		sharingCheckFiles(t, ctx, left, fixture.Checkpoints["after_writes"].ExpectedFiles)
		var right *Sandbox
		if mode == "B1" || proofMode {
			right = resume(mode+"-right", aID, factory, cache)
		}
		if mode == "B1" {
			lm, ok := left.Resources.memory.(*leasedMemory)
			require.True(t, ok, "B1 must actually hold a shared NBD lease")
			rm, ok := right.Resources.memory.(*leasedMemory)
			require.True(t, ok)
			require.Equal(t, lm.lease.Path(), rm.lease.Path())
			require.True(t, strings.HasPrefix(lm.lease.Path(), "/dev/nbd"))
			if pmemMode {
				leftMap, leftPages := sharingPmemMapping(t, left)
				rightMap, rightPages := sharingPmemMapping(t, right)
				sharingCommonPmemPage(t, leftMap, rightMap, leftPages, rightPages)
				require.Equal(t, leftMap["path"], rightMap["path"])
				if leftMap["pfn"].(uint64) != 0 && rightMap["pfn"].(uint64) != 0 {
					require.Equal(t, leftMap["pfn"], rightMap["pfn"])
				}
				sharingReceipt(t, filepath.Join(runDir, "pmem-sharing.json"), map[string]any{"left": leftMap, "right": rightMap, "pfn_visible": leftMap["pfn"].(uint64) != 0 && rightMap["pfn"].(uint64) != 0})
			}
			sharingReceipt(t, filepath.Join(runDir, "B1-shared-backing.json"), map[string]any{
				"left": lm.lease.Path(), "right": rm.lease.Path(), "same_backing": true})
		}
		if proofMode {
			sharingProofPair(t, ctx, runDir, mode, left, right)
			if compatMode {
				sharingCompatPair(t, ctx, runDir, mode, left, right)
			}
			if mode == "B1" {
				control := resume("B2-control", aID, controlFactory, controlCache)
				sharingProofControl(t, ctx, runDir, right, control)
				require.NoError(t, control.Close(ctx))
			}
		}
		sharingRunTools(t, ctx, left, fixture.Tools[2:], runDir)
		sharingCheckFiles(t, ctx, left, fixture.Checkpoints["after_edits"].ExpectedFiles)
		if right != nil {
			sharingCheckFiles(t, ctx, right, fixture.Checkpoints["after_writes"].ExpectedFiles)
			if proofMode {
				sharingProofState(t, ctx, right, 0)
			}
		}
		bID := pauseUpload(mode+"-B", left)
		if right != nil {
			// Pausing/stopping left releases one lease; right must remain usable.
			sharingCheckFiles(t, ctx, right, fixture.Checkpoints["after_writes"].ExpectedFiles)
			if proofMode {
				sharingProofState(t, ctx, right, 0)
			}
		}
		second := resume(mode+"-B-restored", bID, factory, cache)
		sharingCheckFiles(t, ctx, second, fixture.Checkpoints["after_edits"].ExpectedFiles)
		if proofMode {
			sharingReceipt(t, filepath.Join(runDir, mode+"-B-proof-restored.json"), sharingProofState(t, ctx, second, 1))
			sharingProofCommand(t, ctx, second, "mutate2")
			sharingProofState(t, ctx, second, 2)
		}
		if compatMode {
			sharingReceipt(t, filepath.Join(runDir, mode+"-B-compat-restored.json"), sharingCompatState(t, ctx, second, "read", 1))
			sharingCompatState(t, ctx, second, "mutate2", 2)
			sharingCompatState(t, ctx, right, "read", 0)
		}
		if right != nil && mode == "B1" {
			oldRAM, oldOK := right.Resources.memory.(*leasedMemory)
			newRAM, newOK := second.Resources.memory.(*leasedMemory)
			require.True(t, oldOK && newOK)
			require.NotEqual(t, oldRAM.lease.Path(), newRAM.lease.Path(), "distinct live template identities need distinct NBD backing")
			sharingCheckFiles(t, ctx, right, fixture.Checkpoints["after_writes"].ExpectedFiles)
			sharingReceipt(t, filepath.Join(runDir, "B1-distinct-backing.json"), map[string]any{
				"A": oldRAM.lease.Path(), "B": newRAM.lease.Path(), "distinct_live_backings": true})
		}
		if right != nil {
			require.NoError(t, right.Close(ctx))
			sharingCheckFiles(t, ctx, second, fixture.Checkpoints["after_edits"].ExpectedFiles)
		}
		cID := pauseUpload(mode+"-C", second)
		third := resume(mode+"-C-restored", cID, factory, cache)
		sharingCheckFiles(t, ctx, third, fixture.Checkpoints["after_edits"].ExpectedFiles)
		if proofMode {
			sharingReceipt(t, filepath.Join(runDir, mode+"-C-proof-restored.json"), sharingProofState(t, ctx, third, 2))
			sharingProofUnchangedSources(t, ctx, runDir, mode, third, cache, aID)
		}
		if compatMode {
			sharingReceipt(t, filepath.Join(runDir, mode+"-C-compat-restored.json"), sharingCompatState(t, ctx, third, "read", 2))
			sharingCompatTransfers(t, ctx, third)
			for _, kind := range []string{"cpu", "raw-block"} {
				attacker := resume(mode+"-"+kind+"-attacker", aID, factory, cache)
				sharingCompatAttack(t, ctx, runDir, mode+"-"+kind, kind, attacker, third)
				require.NoError(t, attacker.Close(ctx))
			}
		}
		require.NoError(t, third.Close(ctx))
		sharingReceipt(t, filepath.Join(runDir, mode+"-result.json"), map[string]any{
			"result": "passed", "A": aID, "B": bID, "C": cID, "full_logical_native_oracle": true, "requested_ram_export_mode": exportMode,
			"trace_derived_source_edit_fixture": true, "complete_original_trace": false, "pmem_dax_rootfs": pmemMode})
	}
	sharingReceipt(t, filepath.Join(runDir, "result.json"), map[string]any{
		"result": "passed", "factory_create_resume_pause_upload_cache_restore": true,
		"B0_B1_same_source_edit_payloads": true, "complete_original_trace": false, "pmem_dax_rootfs": pmemMode})
}

type sharingTraceFixture struct {
	CompleteOriginalTrace bool `json:"complete_original_trace"`
	Source                struct {
		SHA256 string `json:"sha256"`
	} `json:"source"`
	Tools       []sharingTraceTool `json:"tools"`
	Checkpoints map[string]struct {
		ExpectedFiles []sharingExpectedFile `json:"expected_files"`
	} `json:"checkpoints"`
}
type sharingTraceTool struct {
	StepID int             `json:"step_id"`
	CallID string          `json:"call_id"`
	Name   string          `json:"name"`
	Params json.RawMessage `json:"params"`
}
type sharingExpectedFile struct {
	FilePath  string `json:"file_path"`
	SHA256    string `json:"sha256"`
	SizeBytes int    `json:"size_bytes"`
}

func sharingRunTools(t *testing.T, ctx context.Context, sbx *Sandbox, tools []sharingTraceTool, runDir string) {
	t.Helper()
	for _, tool := range tools {
		// Real envd launches /bin/sh and Python only transports the preserved
		// original tool arguments. Its exact-match Edit contract fails closed.
		program := `import base64,json,pathlib,sys
p=json.loads(base64.b64decode(sys.argv[1])); f=pathlib.Path(p["file_path"])
if sys.argv[2]=="Write":
 f.parent.mkdir(parents=True,exist_ok=True); f.write_bytes(p["content"].encode("utf-8"))
else:
 b=f.read_bytes(); old=p["old_string"].encode("utf-8"); new=p["new_string"].encode("utf-8")
 assert old and b.count(old)>0 and (p.get("replace_all",False) or b.count(old)==1), "ambiguous original Edit"
 f.write_bytes(b.replace(old,new) if p.get("replace_all",False) else b.replace(old,new,1))
`
		cmd := "python3 -c " + sharingQuote(program) + " " + sharingQuote(base64.StdEncoding.EncodeToString(tool.Params)) + " " + sharingQuote(tool.Name)
		_, err := sharingExec(ctx, sbx, cmd)
		require.NoError(t, err, "original step %d call %s", tool.StepID, tool.CallID)
		sharingReceipt(t, filepath.Join(runDir, sbx.Runtime.SandboxID+"-step-"+strconv.Itoa(tool.StepID)+".json"), map[string]any{
			"step_id": tool.StepID, "call_id": tool.CallID, "name": tool.Name, "end_event": true, "exit_code": 0})
	}
}

func sharingExec(ctx context.Context, sbx *Sandbox, command string) ([]byte, error) {
	hc := &http.Client{Transport: SandboxHttpTransport}
	client := processconnect.NewProcessClient(hc, fmt.Sprintf("http://%s:%d", sbx.Slot.HostIPString(), consts.DefaultEnvdServerPort))
	req := connect.NewRequest(&process.StartRequest{Process: &process.ProcessConfig{Cmd: "/bin/sh", Args: []string{"-c", command}}})
	sharedgrpc.SetUserHeader(req.Header(), "root")
	if sbx.Config.Envd.AccessToken != nil {
		req.Header().Set("X-Access-Token", *sbx.Config.Envd.AccessToken)
	}
	stream, err := client.Start(ctx, req)
	if err != nil {
		return nil, err
	}
	defer stream.Close()
	var stdout, stderr bytes.Buffer
	for stream.Receive() {
		event := stream.Msg().GetEvent()
		if event == nil {
			continue
		}
		switch e := event.GetEvent().(type) {
		case *process.ProcessEvent_Data:
			if e.Data != nil {
				stdout.Write(e.Data.GetStdout())
				stderr.Write(e.Data.GetStderr())
			}
		case *process.ProcessEvent_End:
			if e.End == nil || !e.End.GetExited() || e.End.GetExitCode() != 0 {
				return nil, fmt.Errorf("envd nonzero or incomplete End: %v stderr=%s", e.End, stderr.String())
			}
			return stdout.Bytes(), nil
		}
	}
	return nil, fmt.Errorf("envd stream ended without successful End: %v stderr=%s", stream.Err(), stderr.String())
}

func sharingCheckFiles(t *testing.T, ctx context.Context, sbx *Sandbox, files []sharingExpectedFile) {
	t.Helper()
	require.Len(t, files, 2)
	for _, expected := range files {
		encoded, err := sharingExec(ctx, sbx, "base64 "+sharingQuote(expected.FilePath))
		require.NoError(t, err)
		data, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(string(encoded)), ""))
		require.NoError(t, err)
		require.Len(t, data, expected.SizeBytes)
		digest := sha256.Sum256(data)
		require.Equal(t, expected.SHA256, hex.EncodeToString(digest[:]), expected.FilePath)
	}
}

func sharingFCRequest(ctx context.Context, sbx *Sandbox, path string, body any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", sbx.files.SandboxFirecrackerSocketPath())
	}}
	defer transport.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, "http://localhost"+path, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := (&http.Client{Transport: transport}).Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	payload, err := io.ReadAll(res.Body)
	if err != nil {
		return err
	}
	if res.StatusCode != http.StatusNoContent {
		return fmt.Errorf("FC %s status=%d body=%s", path, res.StatusCode, payload)
	}
	return nil
}

func sharingHashFile(t *testing.T, path string) (string, int64) {
	t.Helper()
	f, err := os.Open(path)
	require.NoError(t, err)
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	require.NoError(t, err)
	return hex.EncodeToString(h.Sum(nil)), n
}
func sharingQuote(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'" }
func sharingReceipt(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.MarshalIndent(value, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, append(data, '\n'), 0o600))
}
func sharingRecordPID(t *testing.T, dir, name string, sbx *Sandbox) {
	t.Helper()
	pid, err := sbx.process.Pid()
	require.NoError(t, err)
	sharingReceipt(t, filepath.Join(dir, name+"-pid.json"), map[string]any{"pid": pid, "sandbox_id": sbx.Runtime.SandboxID,
		"namespace": sbx.Slot.NamespaceID(), "api_socket": sbx.files.SandboxFirecrackerSocketPath()})
	if os.Getenv("E2B_SHARING_REQUIRE_SECCOMP") == "1" {
		sharingRequireSeccomp(t, dir, name, pid)
	}
}

func sharingCheckFileRAMMapping(t *testing.T, dir, name string, sbx *Sandbox, path string) {
	t.Helper()
	info, err := os.Stat(path)
	require.NoError(t, err)
	require.True(t, info.Mode().IsRegular(), "B0 RAM backing must be a regular file")
	stat, ok := info.Sys().(*syscall.Stat_t)
	require.True(t, ok)
	pid, err := sbx.process.Pid()
	require.NoError(t, err)
	maps, err := os.ReadFile(fmt.Sprintf("/proc/%d/maps", pid))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, name+"-ram-maps.log"), maps, 0o600))
	var matched string
	for _, line := range strings.Split(string(maps), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 6 || fields[1] != "rw-p" || strings.Join(fields[5:], " ") != path {
			continue
		}
		inode, err := strconv.ParseUint(fields[4], 10, 64)
		require.NoError(t, err)
		if inode == stat.Ino {
			matched = line
			break
		}
	}
	require.NotEmpty(t, matched, "B0 FC must actually map the expected File RAM inode MAP_PRIVATE")
	sharingReceipt(t, filepath.Join(dir, name+"-file-backing.json"), map[string]any{
		"path": path, "regular_file": true, "inode": stat.Ino, "actual_proc_mapping": matched})
}

type sharingNetworkStorage struct {
	mu          sync.Mutex
	config      network.Config
	next, limit int
}

func (s *sharingNetworkStorage) Acquire(ctx context.Context) (*network.Slot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.next >= s.limit {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	i := s.next
	s.next++
	for _, path := range []string{fmt.Sprintf("/var/run/netns/ns-%d", i), fmt.Sprintf("/sys/class/net/veth-%d", i)} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			return nil, fmt.Errorf("refuse occupied network slot %d: %s", i, path)
		}
	}
	return network.NewSlot(strconv.Itoa(i), i, s.config)
}
func (*sharingNetworkStorage) Release(*network.Slot) error { return nil }

func sharingPmemWriteState(t *testing.T, ctx context.Context, sbx *Sandbox) {
	t.Helper()
	script := `import os,stat
assert os.path.isfile('/pmem-fixture/original')
os.unlink('/pmem-fixture/original')
os.unlink('/pmem-fixture/opaque/lower');os.rmdir('/pmem-fixture/opaque');os.mkdir('/pmem-fixture/opaque')
with open('/pmem-fixture/opaque/private','w') as f:f.write('private-upper')
with open('/workspace/pmem-mode','w') as f:f.write('metadata')
os.chmod('/workspace/pmem-mode',0o640);os.chown('/workspace/pmem-mode',123,456)
os.setxattr('/workspace/pmem-mode','user.e2b-pmem',b'persisted-xattr')
os.rename('/workspace/pmem-mode','/workspace/pmem-renamed');os.symlink('pmem-renamed','/workspace/pmem-link')`
	_, err := sharingExec(ctx, sbx, "python3 -c "+sharingQuote(script))
	require.NoError(t, err)
	sharingPmemCheckState(t, ctx, sbx)
}
func sharingPmemCheckState(t *testing.T, ctx context.Context, sbx *Sandbox) {
	t.Helper()
	script := `import os,stat
assert not os.path.exists('/pmem-fixture/original')
assert os.path.isfile('/.e2b/lower/pmem-fixture/original')
assert not os.path.exists('/pmem-fixture/opaque/lower')
assert os.path.isfile('/.e2b/lower/pmem-fixture/opaque/lower')
assert open('/pmem-fixture/opaque/private').read()=='private-upper'
s=os.stat('/workspace/pmem-renamed');assert stat.S_IMODE(s.st_mode)==0o640 and s.st_uid==123 and s.st_gid==456
assert os.readlink('/workspace/pmem-link')=='pmem-renamed'
assert os.getxattr('/workspace/pmem-renamed','user.e2b-pmem')==b'persisted-xattr'
assert os.getxattr('/.e2b/writable/upper/pmem-fixture/opaque','trusted.overlay.opaque')==b'y'`
	_, err := sharingExec(ctx, sbx, "python3 -c "+sharingQuote(script))
	require.NoError(t, err)
}
func sharingPmemMapping(t *testing.T, sbx *Sandbox) (map[string]any, []byte) {
	t.Helper()
	provider, ok := sbx.rootfs.(*rootfs.PmemProvider)
	require.True(t, ok)
	binding := provider.PmemBinding()
	pid, err := sbx.process.Pid()
	require.NoError(t, err)
	maps, err := os.ReadFile(fmt.Sprintf("/proc/%d/maps", pid))
	require.NoError(t, err)
	var address uint64
	for _, line := range strings.Split(string(maps), "\n") {
		f := strings.Fields(line)
		if len(f) < 6 || f[1] != "r--s" || strings.Join(f[5:], " ") != binding.PathOnHost {
			continue
		}
		start, end, _ := strings.Cut(f[0], "-")
		limit, parseErr := strconv.ParseUint(end, 16, 64)
		require.NoError(t, parseErr)
		address, err = strconv.ParseUint(start, 16, 64)
		require.Equal(t, "00000000", f[2], "expected complete backing window at offset zero")
		require.Equal(t, binding.Size, limit-address)
		require.NoError(t, err)
		break
	}
	require.NotZero(t, address, "missing readonly shared pmem mapping")
	pagemap, err := os.Open(fmt.Sprintf("/proc/%d/pagemap", pid))
	require.NoError(t, err)
	defer pagemap.Close()
	// Restore need not touch namespace headers again. Inspect pages that guest
	// activity actually faulted into both mappings, without host prefaulting.
	pages := make([]byte, binding.Size/4096*8)
	_, err = pagemap.ReadAt(pages, int64(address/4096*8))
	require.NoError(t, err)
	return map[string]any{"path": binding.PathOnHost, "mapping_address": address, "pid": pid, "readonly_shared": true, "data_offset": provider.PmemDataOffset()}, pages
}

func sharingCommonPmemPage(t *testing.T, left, right map[string]any, lp, rp []byte) {
	t.Helper()
	require.Equal(t, len(lp), len(rp))
	require.Equal(t, left["data_offset"], right["data_offset"])
	for i := left["data_offset"].(uint64) / 4096 * 8; i < uint64(len(lp)); i += 8 {
		le, re := binary.LittleEndian.Uint64(lp[i:i+8]), binary.LittleEndian.Uint64(rp[i:i+8])
		if le&(1<<63) == 0 || re&(1<<63) == 0 {
			continue
		}
		for _, v := range []struct {
			info  map[string]any
			entry uint64
		}{{left, le}, {right, re}} {
			v.info["file_offset"] = i / 8 * 4096
			v.info["host_address"] = v.info["mapping_address"].(uint64) + i/8*4096
			v.info["entry"] = v.entry
			v.info["pfn"] = v.entry & ((1 << 55) - 1)
		}
		return
	}
	t.Fatal("no common guest-faulted filesystem page in readonly pmem mappings")
}
