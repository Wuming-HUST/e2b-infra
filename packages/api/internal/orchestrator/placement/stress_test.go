package placement

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/e2b-dev/infra/packages/api/internal/api"
	"github.com/e2b-dev/infra/packages/api/internal/orchestrator/nodemanager"
	"github.com/e2b-dev/infra/packages/shared/pkg/grpc/orchestrator"
	"github.com/e2b-dev/infra/packages/shared/pkg/machineinfo"
)

func TestStressPlacementPastAllAdmissionLimits(t *testing.T) {
	t.Setenv("E2B_UNRESTRICTED_STRESS", "true")
	node := nodemanager.NewTestNode("full", api.NodeStatusReady, 10000, 1,
		nodemanager.WithMemoryMetrics(1<<50, 1<<40, 1<<30))
	for i := 0; i < 100; i++ {
		node.PlacementMetrics.StartPlacing(fmt.Sprint(i), nodemanager.SandboxResources{CPUs: 64, MiBMemory: 1 << 20})
	}
	config := DefaultBestOfKConfig()
	config.CanFit = true
	config.TooManyStarting = true
	algo := NewBestOfK(config).(*BestOfK)
	request := nodemanager.SandboxResources{CPUs: 64, MiBMemory: 1 << 20}
	require.False(t, algo.CanFit(node, request, config), "fixture must exceed legacy limits")
	got, err := algo.chooseNode(context.Background(), []*nodemanager.Node{node}, nil, request, machineinfo.MachineInfo{})
	require.NoError(t, err)
	require.Same(t, node, got)
	// Explicit exclusion still prevents placement.
	_, err = algo.chooseNode(context.Background(), []*nodemanager.Node{node}, map[string]struct{}{node.ID: {}}, request, machineinfo.MachineInfo{})
	require.Error(t, err)
}

func TestStressPreservesFirstAllocationError(t *testing.T) {
	t.Setenv("E2B_UNRESTRICTED_STRESS", "true")
	node := nodemanager.NewTestNode("oom", api.NodeStatusReady, 1, 4,
		nodemanager.WithSandboxCreateError(status.Error(codes.ResourceExhausted, "cannot allocate memory")))
	_, err := PlaceSandbox(t.Context(), NewBestOfK(DefaultBestOfKConfig()), []*nodemanager.Node{node}, nil,
		&orchestrator.SandboxCreateRequest{Sandbox: &orchestrator.SandboxConfig{SandboxId: "oom-test", Vcpu: 1, RamMb: 128}}, machineinfo.MachineInfo{})
	require.ErrorContains(t, err, "cannot allocate memory")
	require.Zero(t, node.PlacementMetrics.InProgressCount())
}

func TestStressAttemptsDegradedNode(t *testing.T) {
	t.Setenv("E2B_UNRESTRICTED_STRESS", "true")
	node := nodemanager.NewTestNode("slow-health", api.NodeStatusUnhealthy, 10000, 1)
	got, err := NewBestOfK(DefaultBestOfKConfig()).chooseNode(t.Context(), []*nodemanager.Node{node}, nil, nodemanager.SandboxResources{CPUs: 8, MiBMemory: 8192}, machineinfo.MachineInfo{})
	require.NoError(t, err)
	require.Same(t, node, got)
}
