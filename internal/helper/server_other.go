//go:build !linux

package helper

import (
	"context"
	"errors"
)

// Serve is available only on Linux, where it connects to the Docker daemon.
func Serve(context.Context, Config) error {
	return errors.New("the Linux helper server must run on Linux")
}
