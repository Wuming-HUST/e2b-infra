//go:build linux

package cgroup

import (
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func TestNewManagerCgroupMode(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		fsType   int64
		probeErr error
		enabled  bool
		wantErr  bool
	}{
		{name: "unified v2", fsType: unix.CGROUP2_SUPER_MAGIC, enabled: true},
		{name: "v1 or hybrid read-only tmpfs root", fsType: unix.TMPFS_MAGIC},
		{name: "v1 controller root", fsType: unix.CGROUP_SUPER_MAGIC},
		{name: "permission denied is not v1", probeErr: unix.EACCES, wantErr: true},
		{name: "missing mount is not v1", probeErr: unix.ENOENT, wantErr: true},
		{name: "IO failure is not v1", probeErr: unix.EIO, wantErr: true},
		{name: "unexpected filesystem is not v1", fsType: unix.EXT4_SUPER_MAGIC, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			mgr, err := newManager(func(path string, stat *unix.Statfs_t) error {
				require.Equal(t, cgroupV2MountPoint, path)
				stat.Type = tc.fsType
				stat.Flags = unix.ST_RDONLY // read-only v2 must still fail closed in Initialize
				return tc.probeErr
			})
			if tc.wantErr {
				require.Error(t, err)
				if tc.probeErr != nil {
					require.ErrorIs(t, err, tc.probeErr)
				}
			} else {
				require.NoError(t, err)
			}
			if tc.enabled {
				require.NotNil(t, mgr)
			} else {
				// Check the interface itself, not just its underlying pointer:
				// startup and createCgroup rely on mgr == nil.
				require.True(t, mgr == nil)
			}
		})
	}
}
