//go:build !linux

package fc

import "os/exec"

// applyCgroupFD is a no-op on non-Linux development hosts. Firecracker runs on
// Linux, where process_cgroup_linux.go performs atomic cgroup placement.
func applyCgroupFD(_ *exec.Cmd, _ int) {}
