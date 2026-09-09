package server

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sync/semaphore"
)

func TestStressStartupDoesNotAcquireOrReleaseSemaphore(t *testing.T) {
	t.Setenv("E2B_UNRESTRICTED_STRESS", "true")
	s := &Server{startingSandboxes: semaphore.NewWeighted(0)}
	require.NoError(t, s.waitForAcquire(t.Context()))
	require.NotPanics(t, s.releaseStarting)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	require.ErrorIs(t, s.waitForAcquire(ctx), context.Canceled)
}
