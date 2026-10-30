package sandbox

import (
	"errors"
	"fmt"
	"os"
)

func rejectExistingSnapshotPaths(paths ...string) error {
	for _, path := range paths {
		_, err := os.Lstat(path)
		if err == nil {
			return fmt.Errorf("snapshot destination already exists: %s", path)
		}
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("check snapshot destination %s: %w", path, err)
		}
	}
	return nil
}
