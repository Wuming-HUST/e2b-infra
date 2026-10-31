//go:build linux

package sandbox

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// A container can already report Seccomp=2 before Firecracker installs its
// policy. Require an additional filter on every API/VMM/vCPU thread, using the
// test process as the inherited-policy baseline.
func sharingRequireSeccomp(t *testing.T, dir, name string, pid int) {
	t.Helper()
	status := func(path string) map[string]string {
		t.Helper()
		data, err := os.ReadFile(path)
		require.NoError(t, err)
		fields := make(map[string]string)
		for _, line := range strings.Split(string(data), "\n") {
			key, value, found := strings.Cut(line, ":")
			if found && (key == "Name" || key == "Seccomp" || key == "Seccomp_filters" || key == "NoNewPrivs" || key == "Uid" || key == "Gid") {
				fields[key] = strings.TrimSpace(value)
			}
		}
		return fields
	}
	baseline := status("/proc/self/status")
	inherited, err := strconv.Atoi(baseline["Seccomp_filters"])
	require.NoError(t, err, "kernel must expose inherited filter count")
	entries, err := os.ReadDir(fmt.Sprintf("/proc/%d/task", pid))
	require.NoError(t, err)
	threads := make(map[string]map[string]string)
	api, vcpu := false, false
	for _, entry := range entries {
		fields := status(fmt.Sprintf("/proc/%d/task/%s/status", pid, entry.Name()))
		threads[entry.Name()] = fields
		api = api || fields["Name"] == "fc_api"
		vcpu = vcpu || strings.HasPrefix(fields["Name"], "fc_vcpu")
	}
	sharingReceipt(t, filepath.Join(dir, name+"-seccomp.json"), map[string]any{
		"pid": pid, "baseline": baseline, "threads": threads,
	})
	require.True(t, api, "inspect the real FC API thread")
	require.True(t, vcpu, "inspect the real FC vCPU thread")
	for tid, fields := range threads {
		require.Equal(t, "2", fields["Seccomp"], "thread %s", tid)
		filters, err := strconv.Atoi(fields["Seccomp_filters"])
		require.NoError(t, err)
		require.Greater(t, filters, inherited, "thread %s must add the FC policy beyond inherited container filters", tid)
	}
}
