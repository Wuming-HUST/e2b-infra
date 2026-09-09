// Package stresstest defines the policy of the unrestricted overcommit branch.
// All services must use the same mode. This branch defaults to enabled.
package stresstest

import (
	"context"
	"fmt"
	"math"
	"os"
	"strconv"
	"time"
)

const Env = "E2B_UNRESTRICTED_STRESS"

func Enabled() bool {
	value, present := os.LookupEnv(Env)
	if !present {
		return true
	}
	enabled, err := strconv.ParseBool(value)
	if err != nil {
		panic(fmt.Sprintf("%s: %v", Env, err))
	}
	return enabled
}

// Limit uses the existing negative sentinel for unlimited count quotas.
func Limit(limit int) int {
	if Enabled() {
		return -1
	}
	return limit
}

// Timeout returns the net/http and NBD zero sentinel, not a context deadline.
func Timeout(timeout time.Duration) time.Duration {
	if Enabled() {
		return 0
	}
	return timeout
}

// WithTimeout retains caller cancellation/deadlines but adds no server deadline.
func WithTimeout(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if Enabled() {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, timeout)
}

// LifetimeHours removes the team lifetime cap while staying representable as a
// time.Duration in routing records and sandbox metadata. Explicit sandbox TTLs
// still control cleanup and must cover the experiment's measurement window.
func LifetimeHours(hours int64) int64 {
	if Enabled() {
		return math.MaxInt64 / int64(time.Hour)
	}
	return hours
}
