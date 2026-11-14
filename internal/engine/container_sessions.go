package engine

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/moby/moby/client"
	"github.com/skyri/ucd/internal/api"
)

const (
	maxSessions      = 64
	maxSessionBuffer = 1 << 20
	sessionIdleTime  = 10 * time.Minute
)

// execSession owns a stream and blocks its producer when the client stops reading.
type execSession struct {
	mu         sync.Mutex
	space      *sync.Cond
	buffer     bytes.Buffer
	done       bool
	closed     bool
	err        error
	exitCode   *int
	lastActive time.Time
	timer      *time.Timer
	stopOnce   sync.Once
	stopSource func()
	cancel     context.CancelFunc
	execID     string
	input      io.Writer
	inputMu    sync.Mutex
	finished   chan struct{}
	kind       string
}

func newStreamContext(ctx context.Context) (context.Context, context.CancelFunc, func()) {
	streamCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	stop := context.AfterFunc(ctx, cancel)
	return streamCtx, cancel, func() { stop() }
}

func (s *execSession) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	written := 0
	for len(p) > 0 {
		for s.buffer.Len() == maxSessionBuffer && !s.closed {
			s.space.Wait()
		}
		if s.closed {
			return written, io.ErrClosedPipe
		}
		n := min(len(p), maxSessionBuffer-s.buffer.Len())
		_, _ = s.buffer.Write(p[:n])
		p = p[n:]
		written += n
	}
	return written, nil
}

func (s *execSession) stop() {
	s.mu.Lock()
	s.closed = true
	s.space.Broadcast()
	if s.timer != nil {
		s.timer.Stop()
	}
	s.mu.Unlock()
	s.stopOnce.Do(func() { s.cancel(); s.stopSource() })
}

func (b *Backend) addOutputSession(closeSource func(), cancel context.CancelFunc, pump func(io.Writer) error, finish func() (*int, error), execID string, input io.Writer, kind string) (string, error) {
	id := rand.Text()
	s := &execSession{lastActive: time.Now(), stopSource: closeSource, cancel: cancel, execID: execID, input: input, finished: make(chan struct{}), kind: kind}
	s.space = sync.NewCond(&s.mu)
	b.sessionMu.Lock()
	if len(b.sessions) >= maxSessions {
		b.sessionMu.Unlock()
		s.stop()
		close(s.finished)
		return "", fmt.Errorf("too many open container and log sessions")
	}
	if b.sessions == nil {
		b.sessions = make(map[string]*execSession)
	}
	b.sessions[id] = s
	s.timer = time.AfterFunc(sessionIdleTime, func() { b.expireOutputSession(id, s) })
	b.sessionMu.Unlock()
	go func() {
		defer close(s.finished)
		err := pump(s)
		var exitCode *int
		if err == nil && finish != nil {
			exitCode, err = finish()
		}
		s.stopOnce.Do(func() { s.cancel(); s.stopSource() })
		s.mu.Lock()
		s.done, s.err, s.exitCode = true, err, exitCode
		s.mu.Unlock()
	}()
	return id, nil
}

func (b *Backend) expireOutputSession(id string, s *execSession) {
	b.sessionMu.Lock()
	if b.sessions[id] != s {
		b.sessionMu.Unlock()
		return
	}
	s.mu.Lock()
	remaining := sessionIdleTime - time.Since(s.lastActive)
	if remaining > 0 {
		s.timer.Reset(remaining)
		s.mu.Unlock()
		b.sessionMu.Unlock()
		return
	}
	delete(b.sessions, id)
	s.mu.Unlock()
	b.sessionMu.Unlock()
	s.stop()
	<-s.finished
}

func (b *Backend) findOutputSession(id, kind string) (*execSession, error) {
	b.sessionMu.Lock()
	s := b.sessions[id]
	if s != nil && s.kind != kind {
		s = nil
	}
	if s != nil {
		s.mu.Lock()
		s.lastActive = time.Now()
		s.mu.Unlock()
	}
	b.sessionMu.Unlock()
	if s == nil {
		return nil, api.Fail("not_found", "session is closed or expired")
	}
	return s, nil
}

func (b *Backend) discardOutputSession(id string) {
	b.sessionMu.Lock()
	s := b.sessions[id]
	delete(b.sessions, id)
	b.sessionMu.Unlock()
	if s != nil {
		s.stop()
		<-s.finished
	}
}

// Release a session if the opening RPC was canceled before it can return.
func (b *Backend) finishSessionOpen(ctx context.Context, id string) error {
	stop := context.AfterFunc(ctx, func() { b.discardOutputSession(id) })
	if !stop() {
		b.discardOutputSession(id)
		if err := ctx.Err(); err != nil {
			return err
		}
		return context.Canceled
	}
	if err := ctx.Err(); err != nil {
		b.discardOutputSession(id)
		return err
	}
	return nil
}

func (b *Backend) closeExecSessions() {
	b.sessionMu.Lock()
	sessions := b.sessions
	b.sessions = nil
	b.sessionMu.Unlock()
	for _, s := range sessions {
		s.stop()
	}
	for _, s := range sessions {
		<-s.finished
	}
}

func (b *Backend) outputSessionCall(ctx context.Context, req api.Request) (any, error) {
	p, err := api.Decode[api.ExecParams](req)
	if err != nil {
		return nil, err
	}
	if p.SessionID == "" {
		return nil, api.Fail("invalid_argument", "session ID is required")
	}
	index := strings.LastIndexByte(req.Method, '.')
	if index < 0 {
		return nil, api.Fail("invalid_argument", "unknown session method")
	}
	kind := req.Method[:index]
	if strings.HasSuffix(req.Method, ".close") {
		b.sessionMu.Lock()
		s := b.sessions[p.SessionID]
		if s != nil && s.kind == kind {
			delete(b.sessions, p.SessionID)
		} else {
			s = nil
		}
		b.sessionMu.Unlock()
		if s == nil {
			return nil, api.Fail("not_found", "session is closed or expired")
		}
		s.stop()
		<-s.finished
		return nil, nil
	}
	s, err := b.findOutputSession(p.SessionID, kind)
	if err != nil {
		return nil, err
	}
	switch {
	case strings.HasSuffix(req.Method, ".read"):
		if p.MaxBytes == 0 {
			p.MaxBytes = 64 << 10
		}
		if p.MaxBytes < 1 || p.MaxBytes > 256<<10 {
			return nil, api.Fail("invalid_argument", "maxBytes must be between 1 and 262144")
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		data := make([]byte, min(p.MaxBytes, s.buffer.Len()))
		_, _ = s.buffer.Read(data)
		if len(data) > 0 {
			s.space.Broadcast()
		}
		if len(data) == 0 && s.done && s.err != nil {
			return nil, fmt.Errorf("read session output: %w", s.err)
		}
		eof := s.done && s.buffer.Len() == 0 && s.err == nil
		return api.ExecReadResult{Data: data, EOF: eof, ExitCode: s.exitCode}, nil
	case strings.HasSuffix(req.Method, ".write"):
		if s.input == nil {
			return nil, api.Fail("invalid_argument", "session has no input")
		}
		if len(p.Data) > 64<<10 {
			return nil, api.Fail("invalid_argument", "input exceeds 65536 bytes")
		}
		s.inputMu.Lock()
		defer s.inputMu.Unlock()
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		conn, ok := s.input.(interface{ SetWriteDeadline(time.Time) error })
		if !ok {
			return nil, fmt.Errorf("terminal input connection does not support deadlines")
		}
		deadline := time.Now().Add(30 * time.Second)
		if requestDeadline, ok := ctx.Deadline(); ok && requestDeadline.Before(deadline) {
			deadline = requestDeadline
		}
		if err := conn.SetWriteDeadline(deadline); err != nil {
			return nil, fmt.Errorf("set terminal input deadline: %w", err)
		}
		canceled := make(chan struct{})
		stop := context.AfterFunc(ctx, func() { _ = conn.SetWriteDeadline(time.Now()); close(canceled) })
		defer func() {
			if !stop() {
				<-canceled
			}
			_ = conn.SetWriteDeadline(time.Time{})
		}()
		n, err := s.input.Write(p.Data)
		if err != nil {
			return nil, fmt.Errorf("write terminal input: %w", err)
		}
		if n != len(p.Data) {
			return nil, fmt.Errorf("write terminal input: %w", io.ErrShortWrite)
		}
		return nil, nil
	case strings.HasSuffix(req.Method, ".resize"):
		if s.execID == "" {
			return nil, api.Fail("invalid_argument", "session is not a terminal")
		}
		if p.Rows == 0 || p.Cols == 0 || p.Rows > 1000 || p.Cols > 1000 {
			return nil, api.Fail("invalid_argument", "terminal dimensions must be between 1 and 1000")
		}
		_, err := b.docker.ExecResize(ctx, s.execID, client.ExecResizeOptions{Height: p.Rows, Width: p.Cols})
		if err != nil {
			return nil, fmt.Errorf("resize terminal: %w", err)
		}
		return nil, nil
	default:
		return nil, api.Fail("invalid_argument", "unknown session method")
	}
}

func (b *Backend) containerExecCall(ctx context.Context, req api.Request) (any, error) {
	if req.Method != "container.exec.open" {
		return b.outputSessionCall(ctx, req)
	}
	p, err := api.Decode[api.ExecParams](req)
	if err != nil {
		return nil, err
	}
	if p.ID == "" || len(p.Command) == 0 || p.Command[0] == "" {
		return nil, api.Fail("invalid_argument", "container ID and command arguments are required")
	}
	if p.Rows == 0 && p.Cols == 0 {
		p.Rows, p.Cols = 24, 80
	}
	if p.Rows == 0 || p.Cols == 0 || p.Rows > 1000 || p.Cols > 1000 {
		return nil, api.Fail("invalid_argument", "terminal dimensions must be between 1 and 1000")
	}
	size := client.ConsoleSize{Height: p.Rows, Width: p.Cols}
	created, err := b.docker.ExecCreate(ctx, p.ID, client.ExecCreateOptions{Cmd: p.Command, User: p.User, WorkingDir: p.WorkingDir, Env: p.Env, TTY: true, AttachStdin: true, AttachStdout: true, AttachStderr: true, ConsoleSize: size})
	if err != nil {
		return nil, fmt.Errorf("create terminal command: %w", err)
	}
	streamCtx, cancel, detach := newStreamContext(ctx)
	defer detach()
	attached, err := b.docker.ExecAttach(streamCtx, created.ID, client.ExecAttachOptions{TTY: true, ConsoleSize: size})
	if err != nil {
		cancel()
		return nil, fmt.Errorf("start terminal command: %w", err)
	}
	finish := func() (*int, error) {
		inspectCtx, cancelInspect := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancelInspect()
		result, err := b.docker.ExecInspect(inspectCtx, created.ID, client.ExecInspectOptions{})
		if err != nil {
			return nil, fmt.Errorf("inspect terminal exit status: %w", err)
		}
		if result.Running {
			return nil, fmt.Errorf("terminal stream ended while command was still running")
		}
		return &result.ExitCode, nil
	}
	id, err := b.addOutputSession(attached.Close, cancel, func(w io.Writer) error { _, err := io.Copy(w, attached.Reader); return err }, finish, created.ID, attached.Conn, "container.exec")
	if err != nil {
		return nil, err
	}
	if err := b.finishSessionOpen(ctx, id); err != nil {
		return nil, fmt.Errorf("open terminal session: %w", err)
	}
	return api.ExecOpenResult{SessionID: id}, nil
}
