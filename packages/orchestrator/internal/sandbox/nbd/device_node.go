//go:build linux

package nbd

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// Dynamic NBD devices may not have a /dev entry yet (udev is asynchronous and
// minimal containers may not run it). Read the actual kernel device number;
// max_part changes minor numbering, so never derive it from the device index.
func ensureDevicePath(idx DeviceSlot) error {
	data, err := os.ReadFile(fmt.Sprintf("/sys/block/nbd%d/dev", idx))
	if err != nil {
		return fmt.Errorf("read NBD device number: %w", err)
	}
	parts := strings.Split(strings.TrimSpace(string(data)), ":")
	if len(parts) != 2 {
		return fmt.Errorf("invalid NBD device number: %q", data)
	}
	major, err := strconv.ParseUint(parts[0], 10, 32)
	if err != nil {
		return err
	}
	minor, err := strconv.ParseUint(parts[1], 10, 32)
	if err != nil {
		return err
	}
	dev := unix.Mkdev(uint32(major), uint32(minor))
	path := GetDevicePath(idx)
	var stat unix.Stat_t
	err = unix.Lstat(path, &stat)
	if errors.Is(err, os.ErrNotExist) {
		if err := unix.Mknod(path, unix.S_IFBLK|0600, int(dev)); err != nil && !errors.Is(err, unix.EEXIST) {
			return fmt.Errorf("create NBD device node: %w", err)
		}
		err = unix.Lstat(path, &stat)
	}
	if err != nil {
		return err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFBLK || uint64(stat.Rdev) != dev {
		return fmt.Errorf("NBD device node %s does not match kernel device %d:%d", path, major, minor)
	}
	return nil
}
