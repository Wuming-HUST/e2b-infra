package stresstest

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestStressDefaultAndRollback(t *testing.T) {
	t.Setenv(Env, "true")
	require.NoError(t, os.Unsetenv(Env))
	require.True(t, Enabled())
	require.Equal(t, -1, Limit(0))
	require.Zero(t, Timeout(time.Second))
	t.Setenv(Env, "false")
	require.False(t, Enabled())
	require.Equal(t, 7, Limit(7))
	require.Equal(t, time.Second, Timeout(time.Second))
}

func TestStressRetainsCallerCancellation(t *testing.T) {
	t.Setenv(Env, "true")
	parent, cancel := context.WithCancel(context.Background())
	child, release := WithTimeout(parent, time.Nanosecond)
	defer release()
	_, deadline := child.Deadline()
	require.False(t, deadline)
	cancel()
	require.ErrorIs(t, child.Err(), context.Canceled)
	parent, cancel = context.WithTimeout(context.Background(), time.Hour)
	defer cancel()
	child, release = WithTimeout(parent, time.Nanosecond)
	defer release()
	want, _ := parent.Deadline()
	got, ok := child.Deadline()
	require.True(t, ok)
	require.Equal(t, want, got)
}
