//go:build !mooncake

package storage

import (
	"context"
	"errors"
)

// errMooncakeNotBuilt is returned by the stub NewMooncakeStorageProvider when
// the binary was not compiled with the `mooncake` build tag.
var errMooncakeNotBuilt = errors.New("mooncake storage provider requires building with -tags mooncake")

// NewMooncakeStorageProvider is a stub used when the binary is built without
// the `mooncake` build tag. The Mooncake storage backend requires the CGo
// Mooncake libraries. The default `make build` in packages/orchestrator links
// the real backend; use `build-no-mooncake` to skip it.
func NewMooncakeStorageProvider(_ context.Context, _ string) (StorageProvider, error) {
	return nil, errMooncakeNotBuilt
}
