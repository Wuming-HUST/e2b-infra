//go:build linux

package nbd

import (
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

// validateReadonlyReady verifies the kernel's active export, rather than
// assuming netlink Connected or a device pathname proves its properties.
func validateReadonlyReady(path string, logicalSize int64) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open readonly NBD for readiness: %w", err)
	}
	defer file.Close()
	readOnly, capacity, err := readonlyDeviceInfo(file)
	if err != nil {
		return err
	}
	if !readOnly {
		return fmt.Errorf("NBD %s is writable in the kernel", path)
	}
	if logicalSize < 0 || capacity != uint64(logicalSize) {
		return fmt.Errorf("NBD %s capacity %d does not match logical size %d", path, capacity, logicalSize)
	}
	return nil
}

func readonlyDeviceInfo(file *os.File) (readOnly bool, capacity uint64, err error) {
	ro, err := unix.IoctlGetInt(int(file.Fd()), unix.BLKROGET)
	if err != nil {
		return false, 0, fmt.Errorf("query NBD readonly flag: %w", err)
	}
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, file.Fd(), uintptr(unix.BLKGETSIZE64), uintptr(unsafe.Pointer(&capacity)))
	if errno != 0 {
		return false, 0, fmt.Errorf("query NBD capacity: %w", errno)
	}
	return ro == 1, capacity, nil
}
