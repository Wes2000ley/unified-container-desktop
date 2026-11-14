//go:build !windows

package dockerproxy

import (
	"errors"
	"io"
	"net"
	"syscall"
)

func isClosedStreamError(err error) bool {
	return errors.Is(err, io.ErrClosedPipe) || errors.Is(err, net.ErrClosed) ||
		errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.ECONNRESET)
}
