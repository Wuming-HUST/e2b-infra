package fc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
	"go.uber.org/zap/zapio"
	"golang.org/x/sync/errgroup"

	"github.com/e2b-dev/infra/packages/orchestrator/internal/cfg"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/network"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/rootfs"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/socket"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/template"
	"github.com/e2b-dev/infra/packages/shared/pkg/keys"
	"github.com/e2b-dev/infra/packages/shared/pkg/logger"
	sbxlogger "github.com/e2b-dev/infra/packages/shared/pkg/logger/sandbox"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage"
	"github.com/e2b-dev/infra/packages/shared/pkg/telemetry"
	"github.com/e2b-dev/infra/packages/shared/pkg/utils"
)

var tracer = otel.Tracer("github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/fc")

type ProcessOptions struct {
	// InitrdPath is an optional host path for kernels whose rootfs drivers are modules.
	// Empty keeps the existing kernel-only boot source.
	InitrdPath string

	// IoEngine is the io engine to use for the rootfs drive.
	IoEngine *string

	// InitScriptPath is the path to the init script that will be executed inside the VM on kernel start.
	InitScriptPath string

	// KernelLogs is a flag to enable kernel logs output to the process stdout.
	KernelLogs bool

	// SystemdToKernelLogs is a flag to enable systemd logs output to the console.
	// It enabled the kernel logs by default too.
	SystemdToKernelLogs bool

	// KvmClock is a flag to enable kvm-clock as the clocksource for the kernel.
	KvmClock bool

	// Stdout is the writer to which the process stdout will be written.
	Stdout io.Writer

	// Stderr is the writer to which the process stderr will be written.
	Stderr io.Writer
}

type Process struct {
	Versions Config

	cmd *exec.Cmd

	config                cfg.BuilderConfig
	firecrackerSocketPath string

	slot           *network.Slot
	rootfsProvider rootfs.Provider
	rootfsPath     string
	kernelPath     string
	files          *storage.SandboxFiles

	Exit *utils.ErrorOnce

	client *apiClient
}

// NoNumaNode signals that the Firecracker process should not be bound to a
// specific NUMA node (i.e. FC_BIND was not set or was invalid).
const NoNumaNode = -1

// NumaNodeFromEnv extracts the NUMA node to bind the Firecracker process to from
// the sandbox env vars (the FC_BIND variable, e.g. "FC_BIND=2"). It returns
// NoNumaNode when FC_BIND is unset, empty, or not a valid non-negative integer.
func NumaNodeFromEnv(vars map[string]string) int {
	raw, ok := vars[envVarFCBind]
	if !ok {
		return NoNumaNode
	}

	node, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || node < 0 {
		return NoNumaNode
	}

	return node
}

// envVarFCBind is the sandbox env var used to pin the Firecracker process to a
// NUMA node via `numactl --membind`.
const envVarFCBind = "FC_BIND"

func NewProcess(
	ctx context.Context,
	execCtx context.Context,
	config cfg.BuilderConfig,
	slot *network.Slot,
	files *storage.SandboxFiles,
	versions Config,
	rootfsProvider rootfs.Provider,
	rootfsPaths RootfsPaths,
	numaNode int,
) (*Process, error) {
	ctx, childSpan := tracer.Start(ctx, "initialize-fc", trace.WithAttributes(
		attribute.Int("sandbox.slot.index", slot.Idx),
	))
	defer childSpan.End()

	// Build the firecracker start script and get computed paths
	startBuilder := NewStartScriptBuilder(config)
	startScript, err := startBuilder.Build(versions, files, rootfsPaths, slot.NamespaceID())
	if err != nil {
		return nil, err
	}

	telemetry.SetAttributes(ctx,
		attribute.String("sandbox.cmd", startScript.Value),
	)

	_, err = os.Stat(versions.FirecrackerPath(config))
	if err != nil {
		return nil, fmt.Errorf("error stating firecracker binary: %w", err)
	}

	_, err = os.Stat(versions.HostKernelPath(config))
	if err != nil {
		return nil, fmt.Errorf("error stating kernel file: %w", err)
	}

	// Optionally pin the Firecracker process (and its guest memory allocations)
	// to a specific NUMA node via `numactl --membind`. The node is provided by
	// the FC_BIND sandbox env var and threaded down as numaNode. We wrap the
	// firecracker binary directly so the membind policy applies to the process
	// that actually allocates the guest memory.
	cmdValue := startScript.Value
	if numaNode >= 0 {
		cmdValue = strings.Replace(
			cmdValue,
			"/fc-versions/",
			fmt.Sprintf("/usr/bin/numactl --membind=%d --cpunodebind=%d /fc-versions/", numaNode, numaNode),
			1,
		)
	}
	// print the command for debugging
	zap.L().Sugar().Errorf("[NUMA] depending on env var: %d", numaNode)
	zap.L().Sugar().Errorf("[NUMA] final command: %s", cmdValue)
	cmd := exec.CommandContext(execCtx,
		"unshare",
		"-m",
		"--",
		"bash",
		"-c",
		cmdValue,
	)

	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid: true, // Create a new session
	}

	return &Process{
		Versions:              versions,
		Exit:                  utils.NewErrorOnce(),
		cmd:                   cmd,
		firecrackerSocketPath: files.SandboxFirecrackerSocketPath(),
		config:                config,
		client:                newApiClient(files.SandboxFirecrackerSocketPath()),
		rootfsProvider:        rootfsProvider,
		files:                 files,
		slot:                  slot,

		kernelPath: startScript.KernelPath,
		rootfsPath: startScript.RootfsPath,
	}, nil
}

func (p *Process) configure(
	ctx context.Context,
	sbxMetadata sbxlogger.LoggerMetadata,
	stdoutExternal io.Writer,
	stderrExternal io.Writer,
	cgroupFD int,
) error {
	ctx, childSpan := tracer.Start(ctx, "configure-fc")
	defer childSpan.End()

	stdoutWriter := &zapio.Writer{Log: sbxlogger.I(sbxMetadata).Logger.Detach(ctx), Level: zap.InfoLevel}
	stdoutWriters := []io.Writer{stdoutWriter}
	if stdoutExternal != nil {
		stdoutWriters = append(stdoutWriters, stdoutExternal)
	}
	p.cmd.Stdout = io.MultiWriter(stdoutWriters...)

	stderrWriter := &zapio.Writer{Log: sbxlogger.I(sbxMetadata).Logger.Detach(ctx), Level: zap.ErrorLevel}
	stderrWriters := []io.Writer{stderrWriter}
	if stderrExternal != nil {
		stderrWriters = append(stderrWriters, stderrExternal)
	}
	p.cmd.Stderr = io.MultiWriter(stderrWriters...)

	// The cgroup is created and owned by Sandbox. CLONE_INTO_CGROUP places
	// Firecracker atomically before it can allocate memory or consume CPU.
	applyCgroupFD(p.cmd, cgroupFD)

	err := p.cmd.Start()
	if err != nil {
		return fmt.Errorf("error starting fc process: %w", err)
	}

	startCtx, cancelStart := context.WithCancelCause(ctx)
	defer cancelStart(fmt.Errorf("fc finished starting"))

	go func() {
		defer stderrWriter.Close()
		defer stdoutWriter.Close()

		waitErr := p.cmd.Wait()
		if waitErr != nil {
			var exitErr *exec.ExitError
			if errors.As(waitErr, &exitErr) {
				// Check if the process was killed by a signal
				if status, ok := exitErr.Sys().(syscall.WaitStatus); ok && status.Signaled() && (status.Signal() == syscall.SIGKILL || status.Signal() == syscall.SIGTERM) {
					p.Exit.SetError(nil)

					return
				}
			}

			logger.L().Error(ctx, "error waiting for fc process", zap.Error(waitErr))

			errMsg := fmt.Errorf("error waiting for fc process: %w", waitErr)
			p.Exit.SetError(errMsg)

			cancelStart(errMsg)

			return
		}

		p.Exit.SetError(nil)
	}()

	// Wait for the FC process to start so we can use FC API
	err = socket.Wait(startCtx, p.firecrackerSocketPath)
	if err != nil {
		errMsg := fmt.Errorf("error waiting for fc socket: %w", err)

		fcStopErr := p.Stop(ctx)

		return errors.Join(errMsg, fcStopErr)
	}

	return nil
}

func (p *Process) Create(
	ctx context.Context,
	sbxMetadata sbxlogger.LoggerMetadata,
	vCPUCount int64,
	memoryMB int64,
	hugePages bool,
	options ProcessOptions,
	cgroupFD int,
) error {
	ctx, childSpan := tracer.Start(ctx, "create-fc")
	defer childSpan.End()

	if _, ok := p.rootfsProvider.(pmemBindingProvider); ok && options.InitrdPath == "" {
		return fmt.Errorf("pmem cold boot requires DAX initramfs")
	}

	// Symlink /dev/null to the rootfs link path, so we can start the FC process without the rootfs and then symlink the real rootfs.
	err := utils.SymlinkForce("/dev/null", p.files.SandboxCacheRootfsLinkPath(p.config.StorageConfig))
	if err != nil {
		return fmt.Errorf("error symlinking rootfs: %w", err)
	}

	err = p.configure(
		ctx,
		sbxMetadata,
		options.Stdout,
		options.Stderr,
		cgroupFD,
	)
	if err != nil {
		fcStopErr := p.Stop(ctx)

		return errors.Join(fmt.Errorf("error starting fc process: %w", err), fcStopErr)
	}

	// IPv4 configuration - format: [local_ip]::[gateway_ip]:[netmask]:hostname:iface:dhcp_option:[dns]
	ipv4 := fmt.Sprintf("%s::%s:%s:instance:%s:off:%s", p.slot.NamespaceIP(), p.slot.TapIPString(), p.slot.TapMaskString(), p.slot.VpeerName(), p.slot.TapName())
	args := KernelArgs{
		// Disable kernel logs for production to speed the FC operations
		// https://github.com/firecracker-microvm/firecracker/blob/main/docs/prod-host-setup.md#logging-and-performance
		"quiet":    "",
		"loglevel": "1",

		// Define kernel init path
		"init": options.InitScriptPath,

		// Networking IPv4 and IPv6
		"ip":            ipv4,
		"ipv6.disable":  "0",
		"ipv6.autoconf": "1",

		// Wait 1 second before exiting FC after panic or reboot
		"panic":  "1",
		"reboot": "k",
	}

	if runtime.GOARCH == "amd64" || runtime.GOARCH == "386" {
		args["pci"] = "off"
		args["i8042.nokbd"] = ""
		args["i8042.noaux"] = ""
		args["random.trust_cpu"] = "on"
		args["clocksource"] = "kvm-clock"
	}
	if options.SystemdToKernelLogs {
		args["systemd.journald.forward_to_console"] = ""
	}

	if options.KernelLogs || options.SystemdToKernelLogs {
		// Forward kernel logs to the ttyS0, which will be picked up by the stdout of FC process
		delete(args, "quiet")
		args["console"] = "ttyS0"
		args["loglevel"] = "5" // KERN_NOTICE
	}

	if _, ok := p.rootfsProvider.(pmemBindingProvider); ok {
		args["e2b.pmem-rootfs"] = "1"
		binding := p.rootfsProvider.(pmemBindingProvider).PmemBinding()
		args["e2b.pmem-gpa"] = strconv.FormatUint(binding.GuestAddress, 10)
		args["e2b.pmem-size"] = strconv.FormatUint(binding.Size, 10)
		args["e2b.pmem-data-offset"] = strconv.FormatUint(p.rootfsProvider.(pmemBindingProvider).PmemDataOffset(), 10)
		// The initramfs starts the original init after switching to the overlay.
		if options.InitScriptPath != "" {
			args["e2b.real-init"] = options.InitScriptPath
		}
		delete(args, "init")
	}
	kernelArgs := args.String()
	err = p.client.setBootSource(ctx, kernelArgs, p.kernelPath, options.InitrdPath)
	if err != nil {
		fcStopErr := p.Stop(ctx)

		return errors.Join(fmt.Errorf("error setting fc boot source %q): %w", p.kernelPath, err), fcStopErr)
	}
	telemetry.ReportEvent(ctx, "set fc boot source config")

	// Rootfs
	rootfsPath, err := p.rootfsProvider.Path()
	if err != nil {
		fcStopErr := p.Stop(ctx)

		return errors.Join(fmt.Errorf("error getting rootfs path: %w", err), fcStopErr)
	}
	telemetry.ReportEvent(ctx, "got rootfs path")

	err = utils.SymlinkForce(rootfsPath, p.files.SandboxCacheRootfsLinkPath(p.config.StorageConfig))
	if err != nil {
		fcStopErr := p.Stop(ctx)

		return errors.Join(fmt.Errorf("error symlinking rootfs: %w", err), fcStopErr)
	}
	telemetry.ReportEvent(ctx, "symlinked rootfs")

	err = p.client.setRootfsDrive(ctx, p.rootfsPath, options.IoEngine, !isPmemProvider(p.rootfsProvider))
	if err != nil {
		fcStopErr := p.Stop(ctx)

		return errors.Join(fmt.Errorf("error setting fc drivers config: %w", err), fcStopErr)
	}
	telemetry.ReportEvent(ctx, "set fc drivers config")

	if bound, ok := p.rootfsProvider.(pmemBindingProvider); ok {
		if err := p.client.setPmem(ctx, bound.PmemBinding()); err != nil {
			return errors.Join(err, p.Stop(ctx))
		}
	}

	// Network
	err = p.client.setNetworkInterface(ctx, p.slot.VpeerName(), p.slot.TapName(), p.slot.TapMAC())
	if err != nil {
		fcStopErr := p.Stop(ctx)

		return errors.Join(fmt.Errorf("error setting fc network config: %w", err), fcStopErr)
	}
	telemetry.ReportEvent(ctx, "set fc network config")

	err = p.client.setMachineConfig(ctx, vCPUCount, memoryMB, hugePages)
	if err != nil {
		fcStopErr := p.Stop(ctx)

		return errors.Join(fmt.Errorf("error setting fc machine config: %w", err), fcStopErr)
	}
	telemetry.ReportEvent(ctx, "set fc machine config")

	err = p.client.setEntropyDevice(ctx)
	if err != nil {
		fcStopErr := p.Stop(ctx)

		return errors.Join(fmt.Errorf("error setting fc entropy config: %w", err), fcStopErr)
	}
	telemetry.ReportEvent(ctx, "set fc entropy config")

	// Balloon device with Free Page Reporting (FPR) enabled. Gated by the
	// BALLOON_FPR env var (deploy-time) so it can be toggled without changing the
	// upper layers. Must be configured before the VM is started, since FPR is a
	// boot-time-only feature.
	if p.config.BalloonFPR {
		err = p.client.setBalloon(ctx)
		if err != nil {
			fcStopErr := p.Stop(ctx)

			return errors.Join(fmt.Errorf("error setting fc balloon config: %w", err), fcStopErr)
		}
		telemetry.ReportEvent(ctx, "set fc balloon config")
	}

	err = p.client.startVM(ctx)
	if err != nil {
		fcStopErr := p.Stop(ctx)

		return errors.Join(fmt.Errorf("error starting fc: %w", err), fcStopErr)
	}

	telemetry.ReportEvent(ctx, "started fc")

	return nil
}

func (p *Process) Resume(
	ctx context.Context,
	sbxMetadata sbxlogger.SandboxMetadata,
	memoryBackend SnapshotMemoryBackend,
	snapfile template.File,
	accessToken *string,
	cgroupFD int,
	traceID string,
) error {
	ctx, span := tracer.Start(ctx, "resume-fc")
	defer span.End()

	// Symlink /dev/null to the rootfs link path, so we can start the FC process without the rootfs and then symlink the real rootfs.
	err := utils.SymlinkForce("/dev/null", p.files.SandboxCacheRootfsLinkPath(p.config.StorageConfig))
	if err != nil {
		return fmt.Errorf("error symlinking rootfs: %w", err)
	}

	// create errgroup with context that handled socket wait + rootfs symlink
	eg, egCtx := errgroup.WithContext(ctx)

	eg.Go(func() error {
		phaseStart := time.Now()
		err := p.configure(
			egCtx,
			sbxMetadata,
			nil,
			nil,
			cgroupFD,
		)
		if err != nil {
			return fmt.Errorf("error starting fc process: %w", err)
		}
		zap.L().Sugar().Infof("[ResumeSandbox] configured fc cost: %.3f ms, traceID=%s", time.Since(phaseStart).Seconds()*1000, traceID)

		telemetry.ReportEvent(egCtx, "configured fc")

		return nil
	})

	if memoryBackend.Type == SnapshotMemoryBackendUffd {
		eg.Go(func() error {
			phaseStart := time.Now()
			err := socket.Wait(egCtx, memoryBackend.Path)
			if err != nil {
				return fmt.Errorf("error waiting for uffd socket: %w", err)
			}

			telemetry.ReportEvent(egCtx, "uffd socket ready")
			zap.L().Sugar().Infof("[ResumeSandbox] get uffd sock path cost: %.3f ms, traceID=%s", time.Since(phaseStart).Seconds()*1000, traceID)

			return nil
		})
	}

	eg.Go(func() error {
		phaseStart := time.Now()
		rootfsPath, err := p.rootfsProvider.Path()
		if err != nil {
			return fmt.Errorf("error getting rootfs path: %w", err)
		}

		err = utils.SymlinkForce(rootfsPath, p.files.SandboxCacheRootfsLinkPath(p.config.StorageConfig))
		if err != nil {
			return fmt.Errorf("error symlinking rootfs: %w", err)
		}

		telemetry.ReportEvent(egCtx, "symlinked rootfs")
		zap.L().Sugar().Infof("[ResumeSandbox] get rootfs path cost: %.3f ms, traceID=%s", time.Since(phaseStart).Seconds()*1000, traceID)

		return nil
	})

	err = eg.Wait()
	if err != nil {
		fcStopErr := p.Stop(ctx)

		return errors.Join(fmt.Errorf("error waiting for uffd socket or symlinking rootfs: %w", err), fcStopErr)
	}
	if bound, ok := p.rootfsProvider.(pmemBindingProvider); ok {
		binding := bound.PmemBinding()
		memoryBackend.Pmem = &binding
	}
	phase5Start := time.Now()
	err = p.client.loadSnapshot(
		ctx,
		memoryBackend,
		snapfile,
	)
	if err != nil {
		fcStopErr := p.Stop(ctx)

		return errors.Join(fmt.Errorf("error loading snapshot: %w", err), fcStopErr)
	}
	zap.L().Sugar().Infof("[ResumeSandbox] load snapshot cost: %.3f ms, traceID=%s", time.Since(phase5Start).Seconds()*1000, traceID)
	phase6Start := time.Now()

	err = p.client.resumeVM(ctx)
	if err != nil {
		fcStopErr := p.Stop(ctx)

		return errors.Join(fmt.Errorf("error resuming vm: %w", err), fcStopErr)
	}
	zap.L().Sugar().Infof("[ResumeSandbox] post resume cost: %.3f ms, traceID=%s", time.Since(phase6Start).Seconds()*1000, traceID)
	phase7Start := time.Now()

	meta := &MmdsMetadata{
		SandboxID:            sbxMetadata.SandboxID,
		TemplateID:           sbxMetadata.TemplateID,
		LogsCollectorAddress: fmt.Sprintf("http://%s/logs", p.config.NetworkConfig.OrchestratorInSandboxIPAddress),
	}

	if accessToken != nil && *accessToken != "" {
		meta.AccessTokenHash = keys.HashAccessToken(*accessToken)
	} else {
		meta.AccessTokenHash = keys.HashAccessToken("")
	}

	err = p.client.setMmds(ctx, meta)
	if err != nil {
		fcStopErr := p.Stop(ctx)

		return errors.Join(fmt.Errorf("error setting mmds: %w", err), fcStopErr)
	}
	zap.L().Sugar().Infof("[ResumeSandbox] set mmds cost: %.3f ms, traceID=%s", time.Since(phase7Start).Seconds()*1000, traceID)

	telemetry.SetAttributes(
		ctx,
		attribute.String("sandbox.cmd.dir", p.cmd.Dir),
		attribute.String("sandbox.cmd.path", p.cmd.Path),
	)

	return nil
}

func (p *Process) Pid() (int, error) {
	if p.cmd.Process == nil {
		return 0, fmt.Errorf("fc process not started")
	}

	return p.cmd.Process.Pid, nil
}

// getProcessState returns the state of the process.
// It's used to check if the process is in the D state, because gopsutil doesn't show that.
func getProcessState(ctx context.Context, pid int) (string, error) {
	output, err := exec.CommandContext(ctx, "ps", "-o", "stat=", "-p", fmt.Sprint(pid)).Output()
	if err != nil {
		return "", fmt.Errorf("error getting state of pid=%d: %w", pid, err)
	}

	state := strings.TrimSpace(string(output))

	return state, nil
}

func (p *Process) Stop(ctx context.Context) error {
	if p.cmd.Process == nil {
		return fmt.Errorf("fc process not started")
	}

	// Check if process has already exited.
	select {
	case <-p.Exit.Done():
		logger.L().Info(ctx, "fc process already exited", logger.WithSandboxID(p.files.SandboxID))

		return nil
	default:
	}

	// this function should never fail b/c a previous context was canceled.
	ctx = context.WithoutCancel(ctx)

	state, err := getProcessState(ctx, p.cmd.Process.Pid)
	if err != nil {
		logger.L().Warn(ctx, "failed to get fc process state", zap.Error(err), logger.WithSandboxID(p.files.SandboxID))
	} else if state == "D" {
		logger.L().Info(ctx, "fc process is in the D state before we call SIGTERM", logger.WithSandboxID(p.files.SandboxID))
	}

	err = p.cmd.Process.Signal(syscall.SIGTERM)
	if err != nil {
		if errors.Is(err, os.ErrProcessDone) {
			logger.L().Info(ctx, "fc process already exited", logger.WithSandboxID(p.files.SandboxID))

			return nil
		}

		logger.L().Warn(ctx, "failed to send SIGTERM to fc process", zap.Error(err), logger.WithSandboxID(p.files.SandboxID))
	}

	go func() {
		select {
		// Wait 10 sec for the FC process to exit, if it doesn't, send SIGKILL.
		case <-time.After(10 * time.Second):
			err := p.cmd.Process.Kill()
			if err != nil {
				logger.L().Warn(ctx, "failed to send SIGKILL to fc process", zap.Error(err), logger.WithSandboxID(p.files.SandboxID))
			} else {
				logger.L().Info(ctx, "sent SIGKILL to fc process because it was not responding to SIGTERM for 10 seconds", logger.WithSandboxID(p.files.SandboxID))
			}

			state, err := getProcessState(ctx, p.cmd.Process.Pid)
			if err != nil {
				logger.L().Warn(ctx, "failed to get fc process state after sending SIGKILL", zap.Error(err), logger.WithSandboxID(p.files.SandboxID))
			} else if state == "D" {
				logger.L().Info(ctx, "fc process is in the D state after we call SIGKILL", logger.WithSandboxID(p.files.SandboxID))
			}

		// If the FC process exited, we can return.
		case <-p.Exit.Done():
			return
		}
	}()

	return nil
}

func (p *Process) Pause(ctx context.Context) error {
	ctx, childSpan := tracer.Start(ctx, "pause-fc")
	defer childSpan.End()

	return p.client.pauseVM(ctx)
}

// CreateSnapshot VM needs to be paused before creating a snapshot.
func (p *Process) CreateSnapshot(ctx context.Context, snapfilePath string) error {
	ctx, childSpan := tracer.Start(ctx, "create-snapshot-fc")
	defer childSpan.End()

	return p.client.createSnapshot(ctx, snapfilePath)
}
