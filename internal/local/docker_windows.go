//go:build windows

package local

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/Microsoft/go-winio"
	"github.com/skyri/ucd/internal/api"
	"github.com/skyri/ucd/internal/dockerproxy"
	"golang.org/x/sys/windows"
)

// ServeDocker exposes the Linux Docker API on a local pipe. Windows checks the
// caller's effective token against the pipe DACL when the connection is opened.
func ServeDocker(ctx context.Context, handler http.Handler) error {
	if handler == nil {
		return errors.New("Linux Docker API handler is required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// Message-mode pipes retain byte-stream reads while exposing CloseWrite to
	// Docker's dial-stdio command, which requires a half-close-capable pipe.
	listener, err := winio.ListenPipe(dockerproxy.LinuxPipe, &winio.PipeConfig{SecurityDescriptor: dockerPipeSecurity(), MessageMode: true})
	if err != nil {
		return fmt.Errorf("listen on Linux Docker API pipe: %w", err)
	}
	return serveDockerListener(ctx, listener, handler)
}

// NewWindowsDockerHandler forwards brokered API requests to the private native
// Windows daemon pipe. The controller service process owns the pipe access.
func NewWindowsDockerHandler() http.Handler {
	target := &url.URL{Scheme: "http", Host: "ucd"}
	transport := &http.Transport{
		Proxy:             nil,
		DisableKeepAlives: true,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return winio.DialPipeContext(ctx, `\\.\pipe\UCDDocker`)
		},
	}
	return dockerproxy.New(target, transport)
}

func dockerPipeSecurity() string {
	// Stock Docker clients request GENERIC_READ|GENERIC_WRITE and deliberately
	// use anonymous pipe impersonation. The kernel DACL, not a process token or
	// client-provided identity, therefore authorizes each pipe connection.
	return "D:P(A;;GA;;;SY)(A;;GA;;;BA)(A;;GA;;;OW)"
}

func memberStrings(members []*windows.SID) []string {
	result := make([]string, len(members))
	for i, member := range members {
		result[i] = member.String()
	}
	return result
}

// SyncDockerAccess refreshes both Docker pipe DACLs so only SYSTEM, local
// administrators, and each pipe's service owner can connect. Container
// Operators use the authenticated controller broker, which checks current
// membership on each request.
func SyncDockerAccess(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	descriptor := dockerPipeSecurity()
	var failures []error
	for _, pipe := range []struct{ path, service string }{
		{dockerproxy.LinuxPipe, api.ServiceName},
		{`\\.\pipe\UCDDocker`, "UCDDocker"},
	} {
		if err := updateDockerPipeAccess(ctx, pipe.path, descriptor, func(conn net.Conn) error {
			return verifyPipeServer(conn, pipe.service)
		}); err != nil {
			// An engine may be stopped, starting, or between accepting clients.
			// Its next available instance is reconciled on the following pass.
			if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PIPE_BUSY) {
				continue
			}
			failures = append(failures, fmt.Errorf("refresh %s access: %w", pipe.path, err))
		}
	}
	return errors.Join(failures...)
}

func updateDockerPipeAccess(ctx context.Context, path, descriptor string, verify func(net.Conn) error) error {
	sd, err := windows.SecurityDescriptorFromString(descriptor)
	if err != nil {
		return fmt.Errorf("build Docker pipe security: %w", err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return fmt.Errorf("read Docker pipe DACL: %w", err)
	}
	if dacl == nil {
		return errors.New("Docker pipe DACL is missing")
	}
	dialCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	conn, err := winio.DialPipeAccessImpLevel(dialCtx, path,
		windows.WRITE_DAC|windows.READ_CONTROL|windows.FILE_READ_ATTRIBUTES, winio.PipeImpLevelAnonymous)
	if err != nil {
		return err
	}
	defer conn.Close()
	if err := verify(conn); err != nil {
		return fmt.Errorf("authenticate Docker pipe server: %w", err)
	}
	fd, ok := conn.(interface{ Fd() uintptr })
	if !ok {
		return errors.New("Docker pipe handle is unavailable")
	}
	return windows.SetSecurityInfo(windows.Handle(fd.Fd()), windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, dacl, nil)
}

func serveDockerListener(ctx context.Context, listener net.Listener, handler http.Handler) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	server := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       90 * time.Second,
		MaxHeaderBytes:    1 << 20,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	select {
	case <-ctx.Done():
		shutdownCtx, stop := context.WithTimeout(context.Background(), 15*time.Second)
		err := server.Shutdown(shutdownCtx)
		stop()
		if err != nil {
			_ = server.Close()
		}
		serveErr := <-done
		if err != nil {
			return fmt.Errorf("shut down Linux Docker API pipe: %w", err)
		}
		if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			return fmt.Errorf("serve Linux Docker API pipe: %w", serveErr)
		}
		return nil
	case err := <-done:
		cancel()
		_ = server.Close()
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serve Linux Docker API pipe: %w", err)
	}
}
