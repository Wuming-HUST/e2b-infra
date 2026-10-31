//go:build linux

// pmem-init is the static initramfs PID 1 for pmem-dax-overlay templates.
package main

import (
	"encoding/binary"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

type bootConfig struct {
	gpa, size, dataOffset uint64
	init                  string
}

func parseBoot(input string) (bootConfig, error) {
	c := bootConfig{init: "/sbin/init"}
	enabled := false
	for _, arg := range strings.Fields(input) {
		k, v, _ := strings.Cut(arg, "=")
		var err error
		switch k {
		case "e2b.pmem-rootfs":
			enabled = v == "1"
		case "e2b.pmem-gpa":
			c.gpa, err = strconv.ParseUint(v, 0, 64)
		case "e2b.pmem-size":
			c.size, err = strconv.ParseUint(v, 0, 64)
		case "e2b.pmem-data-offset":
			c.dataOffset, err = strconv.ParseUint(v, 0, 64)
		case "e2b.real-init":
			c.init = v
		}
		if err != nil {
			return c, err
		}
	}
	if !enabled || c.gpa < 1<<32 || c.size == 0 || c.gpa%(128<<20) != 0 || c.size%(128<<20) != 0 || c.gpa >= 1<<40 || c.size > (1<<40)-c.gpa || !filepath.IsAbs(c.init) || filepath.Clean(c.init) != c.init {
		return c, fmt.Errorf("invalid pmem boot layout/init")
	}
	if (c.dataOffset != 0 && c.dataOffset != 2<<20) || c.dataOffset >= c.size {
		return c, fmt.Errorf("invalid pmem namespace data offset")
	}
	return c, nil
}
func mkdir(path string) error { return os.MkdirAll(path, 0755) }
func mount(source, target, kind string, flags uintptr, data string) error {
	if err := mkdir(target); err != nil {
		return err
	}
	if err := syscall.Mount(source, target, kind, flags, data); err != nil {
		return fmt.Errorf("mount %s: %w", target, err)
	}
	return nil
}
func numericFile(path string) (uint64, error) {
	b, e := os.ReadFile(path)
	if e != nil {
		return 0, e
	}
	return strconv.ParseUint(strings.TrimSpace(string(b)), 0, 64)
}
func findLower(c bootConfig) (string, error) {
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		paths, _ := filepath.Glob("/sys/block/pmem*/device/resource")
		var matches []string
		for _, p := range paths {
			g, e := numericFile(p)
			// A PFN namespace exposes the data resource, after its offline
			// metadata prefix; a raw namespace's data offset is zero.
			if e != nil || g != c.gpa+c.dataOffset {
				continue
			}
			name := filepath.Base(filepath.Dir(filepath.Dir(p)))
			n, e := numericFile("/sys/block/" + name + "/size")
			if e == nil && n == (c.size-c.dataOffset)/512 {
				matches = append(matches, "/dev/"+name)
			}
		}
		if len(matches) == 1 {
			return matches[0], nil
		}
		if len(matches) > 1 {
			return "", fmt.Errorf("ambiguous pmem namespace layout")
		}
		time.Sleep(20 * time.Millisecond)
	}
	return "", fmt.Errorf("no pmem namespace matches GPA=%#x size=%d", c.gpa, c.size)
}
func validateExt4(path, label string, clean bool) error {
	f, e := os.Open(path)
	if e != nil {
		return e
	}
	defer f.Close()
	b := make([]byte, 1024)
	if _, e = f.ReadAt(b, 1024); e != nil {
		return e
	}
	if binary.LittleEndian.Uint16(b[56:58]) != 0xef53 || binary.LittleEndian.Uint32(b[24:28]) != 2 {
		return fmt.Errorf("%s requires ext4 with 4KiB blocks", path)
	}
	if label != "" && strings.TrimRight(string(b[120:136]), "\x00") != label {
		return fmt.Errorf("wrong private upper label")
	}
	if clean && binary.LittleEndian.Uint32(b[96:100])&4 != 0 {
		return fmt.Errorf("immutable lower requires offline journal recovery")
	}
	return nil
}

// Choose a real regular inode, never follow an absolute symlink out of lower.
func probeDAX(root string) error {
	var candidate string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			candidate = path
			return filepath.SkipAll
		}
		return nil
	})
	if err != nil {
		return err
	}
	if candidate == "" {
		return fmt.Errorf("lower has no regular DAX probe file")
	}
	var st unix.Statx_t
	if err = unix.Statx(unix.AT_FDCWD, candidate, unix.AT_SYMLINK_NOFOLLOW, unix.STATX_ALL, &st); err != nil {
		return err
	}
	if st.Attributes_mask&unix.STATX_ATTR_DAX == 0 || st.Attributes&unix.STATX_ATTR_DAX == 0 {
		return fmt.Errorf("lower file is not filesystem DAX: %s", candidate)
	}
	return nil
}

func boot() error {
	if os.Getpid() != 1 {
		return fmt.Errorf("must run as initramfs PID 1")
	}
	if os.Getpagesize() != 4096 {
		return fmt.Errorf("pmem format requires a 4KiB guest kernel")
	}
	if e := mount("proc", "/proc", "proc", 0, ""); e != nil {
		return e
	}
	if e := mount("sysfs", "/sys", "sysfs", 0, ""); e != nil {
		return e
	}
	if e := mount("devtmpfs", "/dev", "devtmpfs", 0, ""); e != nil {
		return e
	}
	command, e := os.ReadFile("/proc/cmdline")
	if e != nil {
		return e
	}
	c, e := parseBoot(string(command))
	if e != nil {
		return e
	}
	lower, e := findLower(c)
	if e != nil {
		return e
	}
	if e = validateExt4(lower, "", true); e != nil {
		return e
	}
	// The sole virtio-blk drive is the private upper/work image; validate its role.
	deadline := time.Now().Add(20 * time.Second)
	for {
		e = validateExt4("/dev/vda", "E2BUPPER", false)
		if e == nil {
			break
		}
		if time.Now().After(deadline) {
			return e
		}
		time.Sleep(20 * time.Millisecond)
	}
	if e = mount(lower, "/.e2b/lower", "ext4", syscall.MS_RDONLY, "noload,dax=always"); e != nil {
		return e
	}
	// DAX is mandatory. A successful mount without STATX_ATTR_DAX is a failure.
	if e = probeDAX("/.e2b/lower"); e != nil {
		return e
	}

	if e = mount("/dev/vda", "/.e2b/writable", "ext4", 0, ""); e != nil {
		return e
	}
	for _, d := range []string{"upper", "work"} {
		if e = mkdir("/.e2b/writable/" + d); e != nil {
			return e
		}
	}
	// Overlay upper/work share the same block filesystem and retain their state.
	if e = mount("overlay", "/newroot", "overlay", 0, "lowerdir=/.e2b/lower,upperdir=/.e2b/writable/upper,workdir=/.e2b/writable/work,index=off"); e != nil {
		return e
	}
	for _, p := range []string{"/.e2b/lower", "/.e2b/writable", "/dev", "/proc", "/sys"} {
		target := "/newroot" + p
		if e = mkdir(target); e != nil {
			return e
		}
		if e = syscall.Mount(p, target, "", syscall.MS_MOVE, ""); e != nil {
			return e
		}
	}
	log.Printf("PMEM_DAX_ROOTFS_READY gpa=%#x size=%d lower=%s real_init=%s", c.gpa, c.size, lower, c.init)
	_ = os.Remove("/init")
	if e = syscall.Chdir("/newroot"); e != nil {
		return e
	}
	if e = syscall.Mount(".", "/", "", syscall.MS_MOVE, ""); e != nil {
		return e
	}
	if e = syscall.Chroot("."); e != nil {
		return e
	}
	if e = syscall.Chdir("/"); e != nil {
		return e
	}
	return syscall.Exec(c.init, []string{c.init}, os.Environ())
}
func main() {
	if e := boot(); e != nil {
		log.Fatalf("PMEM_DAX_ROOTFS_FAILED: %v", e)
	}
}
