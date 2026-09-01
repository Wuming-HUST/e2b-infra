//go:build linux

package fc

import (
	"os/exec"
	"syscall"

	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/cgroup"
)

// applyCgroupFD configures CLONE_INTO_CGROUP so the child enters its sandbox
// cgroup atomically, before it can allocate memory or consume CPU.
func applyCgroupFD(cmd *exec.Cmd, cgroupFD int) {
	if cgroupFD == cgroup.NoCgroupFD {
		return
	}

	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.UseCgroupFD = true
	cmd.SysProcAttr.CgroupFD = cgroupFD
}
