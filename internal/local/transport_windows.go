//go:build windows

package local

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"runtime"
	"sync"
	"time"

	"github.com/Microsoft/go-winio"
	"github.com/skyri/ucd/internal/api"
	"golang.org/x/sys/windows"
)

// Clients need data and read-attributes access. Generic write also grants the
// right to create another pipe instance, which must stay with the server.
// Owner Rights permits a non-elevated foreground server to create instances.
const pipeSecurity = "D:P(A;;GA;;;SY)(A;;GA;;;BA)(A;;GA;;;OW)(A;;0x00100083;;;AU)"
const pipeClientAccess = windows.FILE_READ_DATA | windows.FILE_WRITE_DATA | windows.FILE_READ_ATTRIBUTES | windows.SYNCHRONIZE

var impersonateNamedPipeClient = windows.NewLazySystemDLL("advapi32.dll").NewProc("ImpersonateNamedPipeClient")

func newClient(path string) *Client {
	return newPipeClient(path, verifyController)
}

func newPipeClient(path string, verify func(net.Conn) error) *Client {
	transport := &http.Transport{
		Proxy:                 nil,
		DisableKeepAlives:     true,
		ResponseHeaderTimeout: 30 * time.Minute,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			// Identification lets the server inspect the caller's effective token
			// without giving it authority to act as that caller.
			dialCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			conn, err := winio.DialPipeAccessImpLevel(dialCtx, path, pipeClientAccess, winio.PipeImpLevelIdentification)
			if err != nil {
				return nil, err
			}
			if err := verify(conn); err != nil {
				_ = conn.Close()
				return nil, fmt.Errorf("authenticate local controller: %w", err)
			}
			return conn, nil
		},
	}
	return &Client{
		transport: transport,
		http: &http.Client{
			Transport: transport,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return errors.New("local controller redirects are not allowed")
			},
		},
	}
}

// Serve listens for authenticated local calls until ctx is canceled.
func Serve(ctx context.Context, caller api.Caller) error {
	return serve(ctx, api.PipeName, caller)
}

// ServeWithDocker serves authenticated controller calls and Docker API proxy
// requests on the machine controller pipe. Docker requests are authorized for
// the current administrator or Container Operators membership on every HTTP
// request, including read-only methods.
func ServeWithDocker(ctx context.Context, caller api.Caller, docker http.Handler) error {
	return serveWithDocker(ctx, api.PipeName, caller, docker)
}

func serve(ctx context.Context, path string, caller api.Caller) error {
	return serveWithDocker(ctx, path, caller, nil)
}

func serveWithDocker(ctx context.Context, path string, caller api.Caller, docker http.Handler) error {
	if caller == nil {
		return errors.New("local controller caller is required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// Docker's upgraded streams require named-pipe half-close so stdin EOF can
	// reach the daemon while stdout/stderr continue in the opposite direction.
	listener, err := listenControllerPipe(path, docker != nil)
	if err != nil {
		return fmt.Errorf("listen on local controller pipe: %w", err)
	}
	return serveListenerWithDocker(ctx, listener, caller, docker)
}

func listenControllerPipe(path string, dockerEnabled bool) (net.Listener, error) {
	// go-winio creates the first pipe instance exclusively and sets
	// FILE_PIPE_REJECT_REMOTE_CLIENTS on every instance. Message mode is needed
	// only for Docker streams and leaves the HTTP connection byte-stream based.
	return winio.ListenPipe(path, &winio.PipeConfig{
		SecurityDescriptor: pipeSecurity,
		MessageMode:        dockerEnabled,
	})
}

func serveListener(ctx context.Context, listener net.Listener, caller api.Caller) error {
	return serveListenerWithDocker(ctx, listener, caller, nil)
}

func serveListenerWithDocker(ctx context.Context, listener net.Listener, caller api.Caller, docker http.Handler) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	listener = newPeerListener(listener)
	readTimeout, writeTimeout, maxHeaderBytes := 2*time.Minute, 30*time.Minute, 16<<10
	if docker != nil {
		// Docker request/response streams include large builds and interactive
		// hijacked connections, whose duration is controlled by the CLI caller.
		readTimeout, writeTimeout, maxHeaderBytes = 0, 0, 1<<20
	}
	server := &http.Server{
		Handler:           handlerWithDocker(caller, contextPrincipal, docker),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
		BaseContext:       func(net.Listener) context.Context { return ctx },
		ConnContext: func(ctx context.Context, conn net.Conn) context.Context {
			return context.WithValue(ctx, peerKey{}, conn.(*peerConn))
		},
	}
	server.SetKeepAlivesEnabled(false)
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
			return fmt.Errorf("shut down local controller: %w", err)
		}
		if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			return fmt.Errorf("serve local controller: %w", serveErr)
		}
		return nil
	case err := <-done:
		cancel()
		_ = server.Close()
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serve local controller: %w", err)
	}
}

type peerKey struct{}

func contextPrincipal(ctx context.Context) (api.Principal, error) {
	conn, ok := ctx.Value(peerKey{}).(*peerConn)
	if !ok {
		return api.Principal{}, errors.New("pipe connection is unavailable")
	}
	return conn.principal, conn.authErr
}

type peerListener struct {
	net.Listener
	slots chan struct{}
	done  chan struct{}
	once  sync.Once
}

func newPeerListener(listener net.Listener) *peerListener {
	return &peerListener{Listener: listener, slots: make(chan struct{}, 32), done: make(chan struct{})}
}

func (l *peerListener) Accept() (net.Conn, error) {
	select {
	case l.slots <- struct{}{}:
	case <-l.done:
		return nil, net.ErrClosed
	}
	conn, err := l.Listener.Accept()
	if err != nil {
		<-l.slots
		return nil, err
	}
	return &peerConn{Conn: conn, release: func() { <-l.slots }}, nil
}

func (l *peerListener) Close() error {
	l.once.Do(func() { close(l.done) })
	return l.Listener.Close()
}

type peerConn struct {
	net.Conn
	release   func()
	closeOnce sync.Once
	authOnce  sync.Once
	principal api.Principal
	authErr   error
}

func (c *peerConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	if n > 0 {
		// Capture the security context of the first message before net/http can
		// process it or perform a background read. Connections are single-use.
		c.authOnce.Do(func() { c.principal, c.authErr = pipePrincipal(c.Conn) })
	}
	return n, err
}

func (c *peerConn) Close() error {
	err := c.Conn.Close()
	c.closeOnce.Do(c.release)
	return err
}

func pipePrincipal(conn net.Conn) (api.Principal, error) {
	fd, ok := conn.(interface{ Fd() uintptr })
	if !ok {
		return api.Principal{}, errors.New("named pipe handle is unavailable")
	}
	handle := windows.Handle(fd.Fd())
	var pid uint32
	if err := windows.GetNamedPipeClientProcessId(handle, &pid); err != nil {
		return api.Principal{}, fmt.Errorf("get pipe client process: %w", err)
	}
	if pid == 0 {
		return api.Principal{}, errors.New("pipe client process ID is unavailable")
	}
	// If RevertToSelf ever fails, this dedicated, locked goroutine exits
	// without unlocking, causing Go to retire the contaminated OS thread.
	type result struct {
		principal api.Principal
		err       error
	}
	done := make(chan result, 1)
	go func() {
		p, err := principalFromPipe(handle)
		done <- result{p, err}
	}()
	r := <-done
	return r.principal, r.err
}

func principalFromPipe(handle windows.Handle) (api.Principal, error) {
	runtime.LockOSThread()
	impersonating := false
	defer func() {
		if !impersonating {
			runtime.UnlockOSThread()
		}
	}()
	r, _, callErr := impersonateNamedPipeClient.Call(uintptr(handle))
	if r == 0 {
		return api.Principal{}, fmt.Errorf("impersonate pipe client: %w", callErr)
	}
	impersonating = true
	var token windows.Token
	openErr := windows.OpenThreadToken(windows.CurrentThread(), windows.TOKEN_QUERY, true, &token)
	revertErr := windows.RevertToSelf()
	if revertErr != nil {
		if token != 0 {
			_ = token.Close()
		}
		return api.Principal{}, fmt.Errorf("revert pipe client impersonation: %w", revertErr)
	}
	impersonating = false
	if openErr != nil {
		return api.Principal{}, fmt.Errorf("open pipe client token: %w", openErr)
	}
	defer token.Close()
	return principalFromToken(token)
}

func principalFromToken(token windows.Token) (api.Principal, error) {
	user, err := token.GetTokenUser()
	if err != nil {
		return api.Principal{}, fmt.Errorf("read pipe client identity: %w", err)
	}
	name, domain, _, err := user.User.Sid.LookupAccount("")
	if err != nil {
		return api.Principal{}, fmt.Errorf("resolve pipe client identity: %w", err)
	}
	if domain != "" {
		name = domain + `\` + name
	}
	p := api.Principal{SID: user.User.Sid.String(), Name: name, Role: "viewer"}
	adminSID, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return api.Principal{}, fmt.Errorf("resolve administrators group: %w", err)
	}
	admin, err := token.IsMember(adminSID)
	if err != nil {
		return api.Principal{}, fmt.Errorf("check administrators membership: %w", err)
	}
	if admin {
		p.Role = "administrator"
		return p, nil
	}
	members, err := operatorMembers()
	if err != nil {
		return api.Principal{}, err
	}
	operator, err := isCurrentOperator(token, members)
	if err != nil {
		return api.Principal{}, fmt.Errorf("check operators membership: %w", err)
	}
	if operator {
		p.Role = "operator"
	}
	return p, nil
}
