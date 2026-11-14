//go:build windows

package dockerproxy

import (
	"errors"
	"io"
	"net"

	"golang.org/x/sys/windows"
)

func isClosedStreamError(err error) bool {
	return errors.Is(err, io.ErrClosedPipe) || errors.Is(err, net.ErrClosed) ||
		errors.Is(err, windows.ERROR_BROKEN_PIPE) ||
		errors.Is(err, windows.ERROR_NO_DATA) ||
		errors.Is(err, windows.ERROR_PIPE_NOT_CONNECTED)
}
