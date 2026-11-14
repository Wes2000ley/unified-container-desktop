//go:build windows

package local

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/Microsoft/go-winio"
)

// DockerRelay starts a random local pipe with access restricted to its creator.
// Docker CLI traffic is then forwarded through the authenticated controller.
func (c *Client) DockerRelay(ctx context.Context, engine string) (*DockerRelay, error) {
	if ctx == nil {
		return nil, errors.New("Docker relay context is required")
	}
	docker, err := c.dockerHandler(engine)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return startDockerRelay(ctx, docker)
}

func startDockerRelay(ctx context.Context, docker http.Handler) (*DockerRelay, error) {
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return nil, fmt.Errorf("create a private Docker relay name: %w", err)
	}
	name := `\\.\pipe\UCDDockerCLI-` + hex.EncodeToString(token[:])
	listener, err := winio.ListenPipe(name, &winio.PipeConfig{
		SecurityDescriptor: "D:P(A;;GA;;;SY)(A;;GA;;;BA)(A;;GA;;;OW)",
		MessageMode:        true,
	})
	if err != nil {
		return nil, fmt.Errorf("listen on the private Docker relay: %w", err)
	}
	server := &http.Server{
		Handler:           docker,
		ReadHeaderTimeout: 10 * time.Second,
		MaxHeaderBytes:    1 << 20,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	server.SetKeepAlivesEnabled(false)
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	select {
	case err := <-done:
		_ = listener.Close()
		if err == nil || errors.Is(err, http.ErrServerClosed) {
			return nil, errors.New("private Docker relay stopped before it was ready")
		}
		return nil, fmt.Errorf("start private Docker relay: %w", err)
	default:
	}
	return &DockerRelay{endpoint: `npipe:////./pipe/UCDDockerCLI-` + hex.EncodeToString(token[:]), server: server, done: done}, nil
}
