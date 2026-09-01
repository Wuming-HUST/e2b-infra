//go:build linux

package fc

import (
	"os/exec"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/cgroup"
)

func TestApplyCgroupFD(t *testing.T) {
	t.Parallel()

	cmd := exec.Command("true")
	applyCgroupFD(cmd, 42)

	require.NotNil(t, cmd.SysProcAttr)
	assert.True(t, cmd.SysProcAttr.UseCgroupFD)
	assert.Equal(t, 42, cmd.SysProcAttr.CgroupFD)
}

func TestApplyCgroupFDSkipsMissingFD(t *testing.T) {
	t.Parallel()

	cmd := exec.Command("true")
	applyCgroupFD(cmd, cgroup.NoCgroupFD)

	assert.Nil(t, cmd.SysProcAttr)
}
