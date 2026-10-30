//go:build linux

// Standalone /init for the isolated KVM RAM checkpoint oracle. Build with
// GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o guest-init guest.go.
package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"unsafe"
)

func emit(value any) {
	data, _ := json.Marshal(value)
	fmt.Printf("PSG %s\n", data)
}

func fatal(err error) {
	emit(map[string]any{"event": "error", "error": err.Error()})
	for {
		syscall.Pause()
	}
}

func main() {
	for _, dir := range []string{"/proc", "/dev", "/sys"} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			fatal(err)
		}
	}
	if err := syscall.Mount("proc", "/proc", "proc", 0, ""); err != nil {
		fatal(err)
	}
	if err := syscall.Mount("devtmpfs", "/dev", "devtmpfs", 0, ""); err != nil {
		fatal(err)
	}
	// Reattach stdio explicitly after mounting /dev. This also works if the
	// boot kernel could not open an initial console before executing /init.
	console, err := os.OpenFile("/dev/console", os.O_RDWR, 0)
	if err != nil {
		fatal(err)
	}
	defer console.Close()
	for fd := 0; fd < 3; fd++ {
		if fd != int(console.Fd()) {
			if err := syscall.Dup3(int(console.Fd()), fd, 0); err != nil {
				fatal(err)
			}
		}
	}
	var term syscall.Termios
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, os.Stdin.Fd(), syscall.TCGETS, uintptr(unsafe.Pointer(&term))); errno == 0 {
		term.Lflag &^= syscall.ICANON | syscall.ECHO | syscall.ISIG
		term.Iflag &^= syscall.ICRNL | syscall.INLCR | syscall.IXON
		term.Oflag &^= syscall.OPOST
		term.Cc[syscall.VMIN], term.Cc[syscall.VTIME] = 1, 0
		_, _, _ = syscall.Syscall(syscall.SYS_IOCTL, os.Stdin.Fd(), syscall.TCSETS, uintptr(unsafe.Pointer(&term)))
	}
	pageSize := os.Getpagesize()
	pages, err := syscall.Mmap(-1, 0, 4*pageSize, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_PRIVATE|syscall.MAP_ANON)
	if err != nil {
		fatal(err)
	}
	for i := range pages {
		pages[i] = byte(0x31 + i/pageSize)
	}
	pagemap, err := os.Open("/proc/self/pagemap")
	if err != nil {
		fatal(err)
	}
	gpas := make([]uint64, 4)
	for i := range gpas {
		var entry [8]byte
		addr := uintptr(unsafe.Pointer(&pages[i*pageSize]))
		if _, err := pagemap.ReadAt(entry[:], int64(addr/uintptr(pageSize))*8); err != nil {
			fatal(err)
		}
		value := binary.LittleEndian.Uint64(entry[:])
		pfn := value & ((uint64(1) << 55) - 1)
		if value&(uint64(1)<<63) == 0 || pfn == 0 {
			fatal(fmt.Errorf("guest pagemap does not expose present PFN for marker %d", i))
		}
		gpas[i] = pfn * uint64(pageSize)
	}
	pagemap.Close()
	emit(map[string]any{"event": "ready", "page_size": pageSize, "gpas": gpas})
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		args := strings.Fields(scanner.Text())
		if len(args) == 0 {
			continue
		}
		switch args[0] {
		case "read":
			hashes := make([]string, 4)
			for i := range hashes {
				sum := sha256.Sum256(pages[i*pageSize : (i+1)*pageSize])
				hashes[i] = hex.EncodeToString(sum[:])
			}
			emit(map[string]any{"event": "read", "sha256": hashes})
		case "write", "zero":
			if len(args) < 2 {
				emit(map[string]any{"event": "command_error", "error": "missing page index"})
				continue
			}
			index, err := strconv.Atoi(args[1])
			if err != nil || index < 0 || index >= 4 {
				emit(map[string]any{"event": "command_error", "error": "invalid page index"})
				continue
			}
			value := 0
			if args[0] == "write" {
				if len(args) != 3 {
					emit(map[string]any{"event": "command_error", "error": "missing byte value"})
					continue
				}
				value, err = strconv.Atoi(args[2])
				if err != nil || value < 0 || value > 255 {
					emit(map[string]any{"event": "command_error", "error": "invalid byte value"})
					continue
				}
			}
			for j := index * pageSize; j < (index+1)*pageSize; j++ {
				pages[j] = byte(value)
			}
			emit(map[string]any{"event": args[0], "page": index, "value": value})
		default:
			emit(map[string]any{"event": "command_error", "error": "unknown command"})
		}
	}
	fatal(fmt.Errorf("serial input ended: %v", scanner.Err()))
}
