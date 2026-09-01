package placement

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"sync"

	"github.com/e2b-dev/infra/packages/api/internal/api"
	"github.com/e2b-dev/infra/packages/api/internal/orchestrator/nodemanager"
	"github.com/e2b-dev/infra/packages/shared/pkg/machineinfo"
)

// BestOfKConfig holds the configuration parameters for the placement algorithm
type BestOfKConfig struct {
	// R is the cluster-wide max over-commit ratio
	R float64
	// Alpha is the weight for CPU usage in the score calculation
	Alpha float64
	// K is the number of candidate nodes sampled per placement ("power of K choices")
	K int
	// TooManyStarting determines whether to skip nodes that are starting more than maxStartingInstancesPerNode instances
	TooManyStarting bool
	// CanFit determines whether to skip the node CanFit check
	CanFit bool
}

// DefaultBestOfKConfig returns the default placement configuration
func DefaultBestOfKConfig() BestOfKConfig {
	return BestOfKConfig{
		R:     4,
		K:     3,
		Alpha: 0.5,
	}
}

const bytesPerMiB = uint64(1024 * 1024)

func saturatingAddUint64(left, right uint64) uint64 {
	maxUint64 := ^uint64(0)
	if maxUint64-left < right {
		return maxUint64
	}

	return left + right
}

func memoryRequestBytes(memoryMiB int64) uint64 {
	if memoryMiB <= 0 {
		return 0
	}

	requestMiB := uint64(memoryMiB)
	maxUint64 := ^uint64(0)
	if requestMiB > maxUint64/bytesPerMiB {
		return maxUint64
	}

	return requestMiB * bytesPerMiB
}

func pendingMemoryBytes(node *nodemanager.Node) uint64 {
	var pending uint64
	for _, resources := range node.PlacementMetrics.InProgress() {
		pending = saturatingAddUint64(pending, memoryRequestBytes(resources.MiBMemory))
	}

	return pending
}

func memoryScores(metrics nodemanager.Metrics, pendingBytes, requestBytes uint64, overcommitRatio float64) (resident, committed float64) {
	residentBytes := saturatingAddUint64(metrics.MemoryUsedBytes, pendingBytes)
	residentBytes = saturatingAddUint64(residentBytes, requestBytes)
	resident = float64(residentBytes) / float64(metrics.MemoryTotalBytes)

	if overcommitRatio <= 0 || math.IsNaN(overcommitRatio) {
		return resident, math.MaxFloat64
	}

	committedBytes := saturatingAddUint64(metrics.MemoryAllocatedBytes, pendingBytes)
	committedBytes = saturatingAddUint64(committedBytes, requestBytes)
	committed = float64(committedBytes) / (float64(metrics.MemoryTotalBytes) * overcommitRatio)

	return resident, committed
}

// Score calculates the placement score for this node
func (b *BestOfK) Score(node *nodemanager.Node, resources nodemanager.SandboxResources, config BestOfKConfig) float64 {
	metrics := node.Metrics()

	// 1 CPU used = 100% CPU.
	usageAvg := float64(metrics.CpuPercent) / 100
	cpuCount := float64(metrics.CpuCount)
	if cpuCount == 0 {
		return math.MaxFloat64
	}

	totalCPUCapacity := config.R * cpuCount
	cpuScore := (float64(resources.CPUs) + float64(metrics.CpuAllocated) + config.Alpha*usageAvg) / totalCPUCapacity

	// Nodes that have not reported memory capacity retain the previous
	// CPU-only behavior.
	if metrics.MemoryTotalBytes == 0 {
		return cpuScore
	}

	residentScore, commitmentScore := memoryScores(
		metrics,
		pendingMemoryBytes(node),
		memoryRequestBytes(resources.MiBMemory),
		config.R,
	)
	memoryScore := math.Max(residentScore, commitmentScore)

	return math.Max(cpuScore, memoryScore)
}

// CanFit checks if the node can fit a new VM with the given quota
func (b *BestOfK) CanFit(node *nodemanager.Node, sandboxResources nodemanager.SandboxResources, config BestOfKConfig) bool {
	metrics := node.Metrics()

	cpuCount := float64(metrics.CpuCount)
	if cpuCount == 0 {
		return false
	}

	totalCPUCapacity := config.R * cpuCount
	if float64(metrics.CpuAllocated+uint32(sandboxResources.CPUs)) > totalCPUCapacity {
		return false
	}

	if metrics.MemoryTotalBytes == 0 {
		return true
	}

	pendingBytes := pendingMemoryBytes(node)
	requestBytes := memoryRequestBytes(sandboxResources.MiBMemory)
	residentScore, commitmentScore := memoryScores(metrics, pendingBytes, requestBytes, config.R)

	return residentScore <= 1 && commitmentScore <= 1
}

// BestOfK implements the fit-score-place algorithm
type BestOfK struct {
	config BestOfKConfig
	mu     sync.RWMutex
}

var _ Algorithm = &BestOfK{}

// NewBestOfK creates a new placement algorithm with the given config
func NewBestOfK(config BestOfKConfig) Algorithm {
	return &BestOfK{
		config: config,
	}
}

func (b *BestOfK) getConfig() BestOfKConfig {
	b.mu.RLock()
	defer b.mu.RUnlock()

	return b.config
}

// UpdateConfig updates the BestOfK algorithm configuration
func (b *BestOfK) UpdateConfig(config BestOfKConfig) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.config = config
}

// chooseNode selects the best node for placing a VM with the given quota
func (b *BestOfK) chooseNode(_ context.Context, nodes []*nodemanager.Node, excludedNodes map[string]struct{}, resources nodemanager.SandboxResources, buildMachineInfo machineinfo.MachineInfo) (bestNode *nodemanager.Node, err error) {
	// Fix the config, we want to dynamically update it
	config := b.getConfig()

	// Filter eligible nodes
	candidates := b.sample(nodes, config, excludedNodes, resources, buildMachineInfo)

	// Find the best node among candidates
	bestScore := math.MaxFloat64

	for _, node := range candidates {
		// Calculate score
		score := b.Score(node, resources, config)

		if score < bestScore {
			bestNode = node
			bestScore = score
		}
	}

	if bestNode == nil {
		return nil, fmt.Errorf("no node available")
	}

	return bestNode, nil
}

// sample returns up to k items chosen uniformly from those passing ok.
func (b *BestOfK) sample(items []*nodemanager.Node, config BestOfKConfig, excludedNodes map[string]struct{}, resources nodemanager.SandboxResources, buildMachineInfo machineinfo.MachineInfo) []*nodemanager.Node {
	if config.K <= 0 || len(items) == 0 {
		return nil
	}
	indices := make([]int, len(items))
	for i := range indices {
		indices[i] = i
	}

	candidates := make([]*nodemanager.Node, 0, config.K)
	remaining := len(indices) // active pool is indices[:remaining]

	for len(candidates) < config.K && remaining > 0 {
		// pick from the active pool
		j := rand.Intn(remaining)
		pick := indices[j]

		// remove j from pool
		indices[j], indices[remaining-1] = indices[remaining-1], indices[j]
		remaining--

		n := items[pick]

		// Excluded filter
		if _, ok := excludedNodes[n.ID]; ok {
			continue
		}

		// If the node is not ready, skip it
		if n.Status() != api.NodeStatusReady {
			continue
		}

		// Skip if node is not CPU compatible
		if !isNodeCPUCompatible(n, buildMachineInfo) {
			continue
		}

		if config.CanFit {
			if !b.CanFit(n, resources, config) {
				continue
			}
		}

		if config.TooManyStarting {
			// To prevent overloading the node
			if n.PlacementMetrics.InProgressCount() > maxStartingInstancesPerNode {
				continue
			}
		}

		candidates = append(candidates, n)
	}

	return candidates
}
