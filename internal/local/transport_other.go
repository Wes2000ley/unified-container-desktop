//go:build !windows

package local

import (
	"context"
	"errors"

	"github.com/skyri/ucd/internal/api"
)

var errUnsupported = errors.New("local controller named pipes and Windows service lifecycle require Windows")

func newClient(string) *Client { return &Client{err: errUnsupported} }

func (*Client) DockerRelay(context.Context, string) (*DockerRelay, error) {
	return nil, errUnsupported
}

// Serve requires Windows named pipes.
func Serve(context.Context, api.Caller) error { return errUnsupported }

// RunService requires Windows services.
func RunService(func(context.Context) error) error { return errUnsupported }
