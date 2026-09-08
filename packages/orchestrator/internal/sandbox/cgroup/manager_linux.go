//go:build linux

package cgroup

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// NewManager creates a v2 manager when /sys/fs/cgroup is a unified hierarchy.
// On v1/hybrid hosts it returns a nil Manager and no error, explicitly disabling
// per-sandbox v2 accounting. Detection and v2 initialization failures must not
// silently disable accounting.
func NewManager() (Manager, error) {
	return newManager(unix.Statfs)
}

func newManager(statfs func(string, *unix.Statfs_t) error) (Manager, error) {
	var stat unix.Statfs_t
	if err := statfs(cgroupV2MountPoint, &stat); err != nil {
		return nil, fmt.Errorf("detect cgroup filesystem: %w", err)
	}

	switch stat.Type {
	case unix.CGROUP2_SUPER_MAGIC:
		return &managerImpl{}, nil
	case unix.TMPFS_MAGIC, unix.CGROUP_SUPER_MAGIC:
		// v1/hybrid mounts controllers (and optionally unified/) below a tmpfs
		// root. CLONE_INTO_CGROUP cannot place a child into those v1 controllers.
		return nil, nil
	default:
		return nil, fmt.Errorf("unsupported cgroup filesystem type: %#x", stat.Type)
	}
}
