//go:build linux

// Standalone init: active vsock resets are expected at snapshot boundaries.
package main

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync/atomic"
	"syscall"

	"golang.org/x/sys/unix"
)

var completed, resets, corrupt atomic.Uint64

func emitVsock(event string) {
	value := map[string]any{"event": event, "completed": completed.Load(), "resets": resets.Load(), "corrupt": corrupt.Load()}
	data, _ := json.Marshal(value)
	fmt.Printf("PSG %s\n", data)
}

func main() {
	_ = os.MkdirAll("/proc", 0755)
	_ = syscall.Mount("proc", "/proc", "proc", 0, "")
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM, 0)
	if err != nil {
		panic(err)
	}
	if err = unix.Bind(fd, &unix.SockaddrVM{CID: unix.VMADDR_CID_ANY, Port: 1234}); err != nil {
		panic(err)
	}
	if err = unix.Listen(fd, 16); err != nil {
		panic(err)
	}
	go func() {
		for {
			accepted, _, err := unix.Accept(fd)
			if err != nil {
				panic(err)
			}
			file := os.NewFile(uintptr(accepted), "vsock")
			data := make([]byte, 65536)
			for {
				if _, err = io.ReadFull(file, data); err != nil {
					resets.Add(1)
					break
				}
				sequence := binary.LittleEndian.Uint64(data)
				valid := true
				for i := 8; i < len(data); i++ {
					if data[i] != byte(uint64(i)*17+sequence) {
						valid = false
						break
					}
				}
				if !valid {
					corrupt.Add(1)
					break
				}
				completed.Add(1)
				for sent := 0; sent < len(data); {
					n, e := file.Write(data[sent:])
					if e != nil {
						err = e
						break
					}
					sent += n
				}
				if err != nil {
					resets.Add(1)
					break
				}
			}
			_ = file.Close()
		}
	}()
	emitVsock("ready")
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		switch scanner.Text() {
		case "state":
			emitVsock("state")
		default:
			emitVsock("command_error")
		}
	}
}
