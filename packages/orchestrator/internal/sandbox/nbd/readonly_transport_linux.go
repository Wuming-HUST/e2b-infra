//go:build linux

package nbd

import (
	"errors"
	"fmt"
	"syscall"
)

// checkReadonlyTransport checks the actual socket instead of trusting an EOF
// handler to have been scheduled before Close. Peeking never consumes a kernel
// request. Any uncertain/closed socket quarantines the ID rather than issuing a
// disconnect using ownership that may already have disappeared.
func (d *DirectPathMount) checkReadonlyTransport() error {
	if len(d.socksServer) == 0 {
		return errors.New("readonly NBD has no owned service sockets")
	}
	for _, socket := range d.socksServer {
		conn, ok := socket.(interface {
			SyscallConn() (syscall.RawConn, error)
		})
		if !ok {
			return errors.New("readonly NBD service socket cannot prove transport health")
		}
		raw, err := conn.SyscallConn()
		if err != nil {
			return err
		}
		var peekErr error
		err = raw.Control(func(fd uintptr) {
			var b [1]byte
			for {
				n, _, err := syscall.Recvfrom(int(fd), b[:], syscall.MSG_PEEK|syscall.MSG_DONTWAIT)
				if errors.Is(err, syscall.EINTR) {
					continue
				}
				if errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EWOULDBLOCK) {
					return
				}
				if err != nil {
					peekErr = err
				} else if n == 0 {
					peekErr = errors.New("readonly NBD service socket reached EOF")
				}
				return
			}
		})
		if err != nil || peekErr != nil {
			return fmt.Errorf("readonly NBD transport health: %w", errors.Join(err, peekErr))
		}
	}
	return nil
}
