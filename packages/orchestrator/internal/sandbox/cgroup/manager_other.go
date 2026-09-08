//go:build !linux

package cgroup

// NewManager disables cgroup accounting on non-Linux development hosts.
// Firecracker and cgroup process placement are only supported on Linux.
func NewManager() (Manager, error) {
	return nil, nil
}
