//go:build linux

// Standalone init for the M11 asynchronous block/network checkpoint oracle.
package main

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

const chunk = 65536
const slots = 64

var completed [4]atomic.Uint64
var stages [4]atomic.Uint64
var received atomic.Uint64
var netBytes atomic.Uint64
var failures atomic.Uint64

func emit(v any) { b, _ := json.Marshal(v); fmt.Printf("PSG %s\n", b) }
func die(e error) {
	emit(map[string]any{"event": "error", "error": e.Error()})
	for {
		syscall.Pause()
	}
}
func record(dst []byte, w int, s uint64) {
	for i := range dst {
		dst[i] = byte(uint64(i)*17 + s + uint64(w)*29)
	}
	copy(dst, "M11BLK00")
	binary.LittleEndian.PutUint64(dst[8:], uint64(w))
	binary.LittleEndian.PutUint64(dst[16:], s)
}
func worker(w int, fd int) {
	memory, e := syscall.Mmap(-1, 0, chunk*2, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_PRIVATE|syscall.MAP_ANON)
	if e != nil {
		die(e)
	}
	out, in := memory[:chunk], memory[chunk:]
	for s := uint64(1); ; s++ {
		record(out, w, s)
		off := int64((w*slots + int(s%slots)) * chunk)
		stages[w].Store(1)
		n, e := syscall.Pwrite(fd, out, off)
		if e != nil || n != chunk {
			die(fmt.Errorf("worker %d write %d: %d %v", w, s, n, e))
		}
		stages[w].Store(2)
		n, e = syscall.Pread(fd, in, off)
		if e != nil || n != chunk {
			die(fmt.Errorf("worker %d read %d: %d %v", w, s, n, e))
		}
		if !bytes.Equal(out, in) {
			failures.Add(1)
			die(fmt.Errorf("worker %d sequence %d readback mismatch", w, s))
		}
		completed[w].Store(s)
		stages[w].Store(3)
	}
}
func network() {
	fd, e := syscall.Socket(syscall.AF_PACKET, syscall.SOCK_RAW, 0x0300)
	if e != nil {
		die(e)
	}
	// The fixture has exactly one virtio network device. Resolve its index from sysfs.
	entries, e := os.ReadDir("/sys/class/net")
	if e != nil {
		die(e)
	}
	idx := 0
	for _, entry := range entries {
		if entry.Name() == "lo" {
			continue
		}
		b, e := os.ReadFile("/sys/class/net/" + entry.Name() + "/ifindex")
		if e != nil {
			die(e)
		}
		fmt.Sscanf(string(b), "%d", &idx)
		break
	}
	if idx == 0 {
		die(fmt.Errorf("no virtio net interface"))
	}
	// SIOCSIFFLAGS: bring the fixture interface up without external userspace tools.
	control, e := syscall.Socket(syscall.AF_INET, syscall.SOCK_DGRAM, 0)
	if e != nil {
		die(e)
	}
	var req [40]byte
	for _, entry := range entries {
		if entry.Name() != "lo" {
			copy(req[:16], entry.Name())
			break
		}
	}
	*(*uint16)(unsafe.Pointer(&req[16])) = syscall.IFF_UP | syscall.IFF_RUNNING
	_, _, eno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(control), syscall.SIOCSIFFLAGS, uintptr(unsafe.Pointer(&req[0])))
	if eno != 0 {
		die(eno)
	}
	syscall.Close(control)
	if e = syscall.Bind(fd, &syscall.SockaddrLinklayer{Protocol: 0x0300, Ifindex: idx}); e != nil {
		die(e)
	}
	b := make([]byte, 4096)
	for {
		n, _, e := syscall.Recvfrom(fd, b, 0)
		if e != nil {
			die(e)
		}
		if n < 30 || b[12] != 0x88 || b[13] != 0xb5 {
			continue
		}
		if string(b[14:22]) != "M11NET00" {
			failures.Add(1)
			continue
		}
		sequence := binary.LittleEndian.Uint64(b[22:30])
		for i := 30; i < n; i++ {
			if b[i] != byte(sequence+uint64(i)) {
				failures.Add(1)
				die(fmt.Errorf("network payload mismatch"))
			}
		}
		received.Add(1)
		netBytes.Add(uint64(n))
	}
}
func report(event string) {
	var c, s [4]uint64
	for i := range c {
		c[i] = completed[i].Load()
		s[i] = stages[i].Load()
	}
	emit(map[string]any{"event": event, "completed": c, "stages": s, "rx_packets": received.Load(), "rx_bytes": netBytes.Load(), "errors": failures.Load()})
}
func main() {
	runtime.GOMAXPROCS(4)
	for _, p := range []string{"/proc", "/sys", "/dev"} {
		if e := os.MkdirAll(p, 0755); e != nil {
			die(e)
		}
	}
	for _, m := range [][3]string{{"proc", "/proc", "proc"}, {"sysfs", "/sys", "sysfs"}, {"devtmpfs", "/dev", "devtmpfs"}} {
		if e := syscall.Mount(m[0], m[1], m[2], 0, ""); e != nil {
			die(e)
		}
	}
	f, e := os.OpenFile("/dev/console", os.O_RDWR, 0)
	if e != nil {
		die(e)
	}
	for fd := 0; fd < 3; fd++ {
		if fd != int(f.Fd()) {
			if e = syscall.Dup3(int(f.Fd()), fd, 0); e != nil {
				die(e)
			}
		}
	}
	var t syscall.Termios
	_, _, eno := syscall.Syscall(syscall.SYS_IOCTL, 0, syscall.TCGETS, uintptr(unsafe.Pointer(&t)))
	if eno == 0 {
		t.Lflag &^= syscall.ICANON | syscall.ECHO | syscall.ISIG
		t.Iflag &^= syscall.ICRNL | syscall.INLCR | syscall.IXON
		t.Oflag &^= syscall.OPOST
		t.Cc[syscall.VMIN], t.Cc[syscall.VTIME] = 1, 0
		syscall.Syscall(syscall.SYS_IOCTL, 0, syscall.TCSETS, uintptr(unsafe.Pointer(&t)))
	}
	for _, name := range []string{"virtio_mmio", "virtio_blk", "failover", "net_failover", "virtio_net"} {
		module, e := os.Open("/modules/" + name + ".ko")
		if e != nil {
			die(fmt.Errorf("module %s: %w", name, e))
		}
		params := []byte{0}
		_, _, eno := syscall.Syscall(syscall.SYS_FINIT_MODULE, module.Fd(), uintptr(unsafe.Pointer(&params[0])), 0)
		module.Close()
		if eno != 0 && eno != syscall.EEXIST {
			die(fmt.Errorf("module %s: %v", name, eno))
		}
	}
	for deadline := time.Now().Add(5 * time.Second); ; {
		if _, e := os.Stat("/dev/vda"); e == nil {
			break
		}
		if time.Now().After(deadline) {
			die(fmt.Errorf("virtio block node missing after module load"))
		}
		time.Sleep(10 * time.Millisecond)
	}
	fd, e := syscall.Open("/dev/vda", syscall.O_RDWR|syscall.O_DIRECT, 0)
	if e != nil {
		die(e)
	}
	for i := range completed {
		go worker(i, fd)
	}
	go network()
	report("ready")
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		switch strings.TrimSpace(scanner.Text()) {
		case "status":
			report("status")
		default:
			die(fmt.Errorf("unknown command"))
		}
	}
	die(fmt.Errorf("serial input ended: %v", scanner.Err()))
}
