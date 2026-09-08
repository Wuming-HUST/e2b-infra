package sandbox

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/cgroup"
)

type failingCgroupManager struct {
	err error
}

func (m failingCgroupManager) Initialize(context.Context) error {
	return nil
}

func (m failingCgroupManager) Create(context.Context, string) (*cgroup.CgroupHandle, error) {
	return nil, m.err
}

func TestCreateCgroupFailsClosed(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("cgroup unavailable")
	handle, fd, err := createCgroup(
		t.Context(),
		failingCgroupManager{err: wantErr},
		"sandbox-id",
		NewCleanup(),
	)

	require.Nil(t, handle)
	require.Equal(t, cgroup.NoCgroupFD, fd)
	require.ErrorIs(t, err, wantErr)
}

func TestCreateCgroupDisabled(t *testing.T) {
	t.Parallel()

	handle, fd, err := createCgroup(t.Context(), nil, "sandbox-id", NewCleanup())
	require.NoError(t, err)
	require.Nil(t, handle)
	require.Equal(t, cgroup.NoCgroupFD, fd)
}
