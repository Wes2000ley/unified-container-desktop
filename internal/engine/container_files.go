package engine

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/containerd/errdefs"
	"github.com/moby/moby/client"
	"github.com/skyri/ucd/internal/api"
)

// Container paths are interpreted on the engine, not the controller host.
func containerPath(engine, raw string) (string, error) {
	if raw == "" || strings.ContainsRune(raw, 0) {
		return "", api.Fail("invalid_argument", "an absolute container path is required")
	}
	if engine == "windows" {
		raw = strings.ReplaceAll(raw, `\`, "/")
		if len(raw) < 3 || raw[1] != ':' || raw[2] != '/' || !((raw[0] >= 'A' && raw[0] <= 'Z') || (raw[0] >= 'a' && raw[0] <= 'z')) {
			return "", api.Fail("invalid_argument", "Windows container path must be absolute, for example C:\\data")
		}
		return strings.ToUpper(raw[:1]) + ":" + path.Clean(raw[2:]), nil
	}
	if !path.IsAbs(raw) {
		return "", api.Fail("invalid_argument", "Linux container path must be absolute")
	}
	return path.Clean(raw), nil
}

func containerRoot(engine, p string) bool {
	if engine == "windows" {
		return len(p) == 3 && p[1:] == ":/"
	}
	return p == "/"
}

func (b *Backend) containerFilesCall(ctx context.Context, req api.Request) (any, error) {
	p, err := api.Decode[api.ContainerFileParams](req)
	if err != nil {
		return nil, err
	}
	if p.ID == "" {
		return nil, api.Fail("invalid_argument", "container ID is required")
	}
	if req.Method == "container.files.prepare" {
		return b.prepareContainerFiles(ctx, p.ID, p.Confirmed)
	}
	p.Path, err = containerPath(b.id, p.Path)
	if err != nil {
		return nil, err
	}
	switch req.Method {
	case "container.files.list":
		return b.listContainerFiles(ctx, p.ID, p.Path)
	case "container.files.mkdir", "container.files.remove":
		if req.Method == "container.files.remove" && containerRoot(b.id, p.Path) {
			return nil, api.Fail("invalid_argument", "cannot remove the container filesystem root")
		}
		args := []string{strings.TrimPrefix(req.Method, "container.files.")}
		if p.Recursive && req.Method == "container.files.remove" {
			args = append(args, "--recursive")
		}
		args = append(args, p.Path)
		_, err := b.runFileTool(ctx, p.ID, args)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", strings.TrimPrefix(req.Method, "container.files."), err)
		}
		return nil, nil
	case "container.files.write", "container.files.upload":
		if containerRoot(b.id, p.Path) {
			return nil, api.Fail("invalid_argument", "file path must include a name")
		}
		if len(p.Data) > maxContainerTransfer {
			return nil, api.Fail("invalid_argument", "file exceeds 8388608-byte transfer limit")
		}
		if b.id == "windows" {
			return nil, b.writeFileTool(ctx, p.ID, []string{"write", p.Path, strconv.Itoa(len(p.Data)), strconv.FormatUint(uint64(p.Mode), 10)}, bytes.NewReader(p.Data))
		}
		var archive bytes.Buffer
		writer := tar.NewWriter(&archive)
		header := &tar.Header{Name: path.Base(p.Path), Mode: 0644, Size: int64(len(p.Data)), Typeflag: tar.TypeReg}
		existing, err := b.docker.CopyFromContainer(ctx, p.ID, client.CopyFromContainerOptions{SourcePath: p.Path})
		if err == nil {
			if !existing.Stat.Mode.IsRegular() {
				_ = existing.Content.Close()
				return nil, api.Fail("invalid_argument", "destination exists and is not a regular file")
			}
			previous, readErr := tar.NewReader(existing.Content).Next()
			_ = existing.Content.Close()
			if readErr != nil {
				return nil, fmt.Errorf("read existing file metadata: %w", readErr)
			}
			if previous.Typeflag != tar.TypeReg && previous.Typeflag != tar.TypeRegA {
				return nil, fmt.Errorf("existing file archive is not a regular file")
			}
			header.Mode, header.Uid, header.Gid = previous.Mode, previous.Uid, previous.Gid
			header.Uname, header.Gname = previous.Uname, previous.Gname
		} else if !errdefs.IsNotFound(err) {
			return nil, fmt.Errorf("inspect existing file: %w", err)
		}
		if p.Mode != 0 {
			header.Mode = int64(p.Mode & 0777)
		}
		if err := writer.WriteHeader(header); err != nil {
			return nil, fmt.Errorf("prepare file archive: %w", err)
		}
		if _, err := writer.Write(p.Data); err != nil {
			return nil, fmt.Errorf("prepare file contents: %w", err)
		}
		if err := writer.Close(); err != nil {
			return nil, fmt.Errorf("finish file archive: %w", err)
		}
		parent := path.Dir(p.Path)
		if b.id == "windows" && len(parent) == 2 && parent[1] == ':' {
			parent += "/"
		}
		_, err = b.docker.CopyToContainer(ctx, p.ID, client.CopyToContainerOptions{DestinationPath: parent, Content: &archive, CopyUIDGID: b.id == "linux"})
		if err != nil {
			return nil, fmt.Errorf("write container file: %w", err)
		}
		return nil, nil
	case "container.files.read", "container.files.download", "container.files.archive":
		if b.id == "windows" {
			command := "read"
			name := path.Base(p.Path)
			if req.Method == "container.files.archive" {
				command = "archive"
				if containerRoot(b.id, p.Path) {
					name = "files"
				}
				name += ".tar"
			}
			data, err := b.runFileTool(ctx, p.ID, []string{command, p.Path})
			if err != nil {
				return nil, err
			}
			return api.BinaryResult{Data: data, Name: name}, nil
		}
		result, err := b.docker.CopyFromContainer(ctx, p.ID, client.CopyFromContainerOptions{SourcePath: p.Path})
		if err != nil {
			return nil, fmt.Errorf("copy container path: %w", err)
		}
		defer result.Content.Close()
		if req.Method == "container.files.archive" {
			data, err := readLimited(result.Content, maxContainerTransfer)
			if err != nil {
				return nil, fmt.Errorf("download container archive: %w", err)
			}
			name := path.Base(p.Path)
			if containerRoot(b.id, p.Path) {
				name = "container-root"
			}
			return api.BinaryResult{Data: data, Name: name + ".tar"}, nil
		}
		if !result.Stat.Mode.IsRegular() {
			return nil, api.Fail("invalid_argument", "path is not a regular file; use archive for directories")
		}
		reader := tar.NewReader(result.Content)
		header, err := reader.Next()
		if err != nil {
			return nil, fmt.Errorf("read file archive: %w", err)
		}
		if header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA {
			return nil, fmt.Errorf("container archive does not contain a regular file")
		}
		if header.Size > maxContainerTransfer {
			return nil, fmt.Errorf("file exceeds %d-byte transfer limit", maxContainerTransfer)
		}
		data, err := readLimited(reader, maxContainerTransfer)
		if err != nil {
			return nil, fmt.Errorf("read container file: %w", err)
		}
		return api.BinaryResult{Data: data, Name: path.Base(p.Path)}, nil
	default:
		return nil, api.Fail("invalid_argument", "unknown container file operation")
	}
}

// Preparation is explicit because a running Hyper-V container must be stopped
// before Docker can access its filesystem. Always try to restore its prior state.
func (b *Backend) prepareContainerFiles(ctx context.Context, id string, confirmed bool) (api.ContainerFilePrepareResult, error) {
	if b.id != "windows" {
		return api.ContainerFilePrepareResult{}, api.Fail("invalid_argument", "file preparation is only required for Windows containers")
	}
	// Serialize the whole lifecycle transition, so concurrent preparation
	// requests cannot restart a container while another one is installing.
	b.fileToolMu.Lock()
	defer b.fileToolMu.Unlock()
	if err := ctx.Err(); err != nil {
		return api.ContainerFilePrepareResult{}, err
	}
	inspection, err := b.docker.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if err != nil {
		return api.ContainerFilePrepareResult{}, fmt.Errorf("inspect container before preparing files: %w", err)
	}
	if inspection.Container.State == nil {
		return api.ContainerFilePrepareResult{}, fmt.Errorf("container state is unavailable")
	}
	if inspection.Container.State.Paused {
		return api.ContainerFilePrepareResult{}, api.Fail("invalid_state", "container is paused; unpause it before preparing file access")
	}
	wasRunning := inspection.Container.State.Running
	if wasRunning && !confirmed {
		return api.ContainerFilePrepareResult{}, api.Fail("confirmation_required", "preparing files requires stopping and restarting this running container")
	}
	if _, _, _, err := b.fileToolSource(); err != nil {
		return api.ContainerFilePrepareResult{}, err
	}
	if wasRunning {
		if err := ctx.Err(); err != nil {
			return api.ContainerFilePrepareResult{}, err
		}
		// Once the user has confirmed, finish the stop/install/start sequence
		// even if the client disconnects. Keep both phases bounded.
		workCtx, workCancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
		defer workCancel()
		stopRequest, _ := api.NewRequest("container.action", b.id, api.ContainerParams{ID: id, Action: "stop"})
		_, stopErr := b.containerCall(workCtx, stopRequest)
		var installErr error
		if stopErr == nil {
			_, installErr = b.prepareFileToolLocked(workCtx, id)
		} else {
			stopErr = fmt.Errorf("stop container before preparing files: %w", stopErr)
		}
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		startRequest, _ := api.NewRequest("container.action", b.id, api.ContainerParams{ID: id, Action: "start"})
		_, restartErr := b.containerCall(cleanupCtx, startRequest)
		cancel()
		if restartErr != nil {
			restartErr = fmt.Errorf("restart container after preparing files: %w", restartErr)
		}
		if stopErr != nil || installErr != nil || restartErr != nil {
			return api.ContainerFilePrepareResult{}, errors.Join(stopErr, installErr, restartErr)
		}
		return api.ContainerFilePrepareResult{Prepared: true, Restarted: true}, nil
	}
	if _, err := b.prepareFileToolLocked(ctx, id); err != nil {
		return api.ContainerFilePrepareResult{}, err
	}
	return api.ContainerFilePrepareResult{Prepared: true}, nil
}

func (b *Backend) listContainerFiles(ctx context.Context, id, directory string) (api.ContainerFileList, error) {
	data, err := b.runFileTool(ctx, id, []string{"list", directory})
	if err != nil {
		return api.ContainerFileList{}, fmt.Errorf("list container directory: %w", err)
	}
	var result api.ContainerFileList
	if err := json.Unmarshal(data, &result); err != nil {
		return api.ContainerFileList{}, fmt.Errorf("decode directory listing: %w", err)
	}
	return result, nil
}

// fileToolSource identifies the packaged utility without accessing the container.
func (b *Backend) fileToolSource() (data []byte, destination, inside string, err error) {
	executable, err := os.Executable()
	if err != nil {
		return nil, "", "", fmt.Errorf("locate packaged file utility: %w", err)
	}
	name := "ucd-filetool"
	destination = "/"
	if b.id == "windows" {
		name, destination = "ucd-filetool.exe", "C:/Windows/Temp"
	}
	if (b.id == "windows") != (runtime.GOOS == "windows") {
		return nil, "", "", fmt.Errorf("file utility platform does not match the engine")
	}
	file, err := os.Open(filepath.Join(filepath.Dir(executable), name))
	if err != nil {
		return nil, "", "", fmt.Errorf("read packaged %s beside the engine service: %w", name, err)
	}
	data, err = readLimited(file, maxContainerTransfer)
	_ = file.Close()
	if err != nil {
		return nil, "", "", fmt.Errorf("read packaged file utility: %w", err)
	}
	digest := sha256.Sum256(data)
	toolName := fmt.Sprintf(".ucd-filetool-%x", digest[:8])
	if b.id == "windows" {
		toolName += ".exe"
	}
	return data, destination, path.Join(destination, toolName), nil
}

// prepareFileTool is called while a Windows container is stopped. Linux
// containers allow their archive API while running and are prepared on use.
func (b *Backend) prepareFileTool(ctx context.Context, id string) (string, error) {
	b.fileToolMu.Lock()
	defer b.fileToolMu.Unlock()
	return b.prepareFileToolLocked(ctx, id)
}

func (b *Backend) prepareFileToolLocked(ctx context.Context, id string) (string, error) {
	data, destination, inside, err := b.fileToolSource()
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(data)
	toolName := path.Base(inside)
	prior, err := b.docker.CopyFromContainer(ctx, id, client.CopyFromContainerOptions{SourcePath: inside})
	if err == nil {
		// The name identifies a version, but the complete contents must match;
		// never replace a pre-existing path merely because its prefix matches.
		verifyErr := func() error {
			defer prior.Content.Close()
			if !prior.Stat.Mode.IsRegular() || prior.Stat.LinkTarget != "" {
				return fmt.Errorf("path is not a regular file")
			}
			if prior.Stat.Size != int64(len(data)) {
				return fmt.Errorf("file size differs from the packaged utility")
			}
			reader := tar.NewReader(prior.Content)
			header, err := reader.Next()
			if err != nil {
				return fmt.Errorf("read file archive: %w", err)
			}
			if header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA {
				return fmt.Errorf("archive entry is not a regular file")
			}
			if header.Size != int64(len(data)) {
				return fmt.Errorf("archive size differs from the packaged utility")
			}
			if b.id == "linux" && (prior.Stat.Mode.Perm()&0111 == 0 || header.Mode&0111 == 0) {
				return fmt.Errorf("file is not executable")
			}
			existingHash := sha256.New()
			if _, err := io.Copy(existingHash, reader); err != nil {
				return fmt.Errorf("read file contents: %w", err)
			}
			if !bytes.Equal(existingHash.Sum(nil), digest[:]) {
				return fmt.Errorf("file contents differ from the packaged utility")
			}
			return nil
		}()
		if verifyErr != nil {
			return "", fmt.Errorf("file utility path %s is occupied; refusing to overwrite it: %w", inside, verifyErr)
		}
	} else if !errdefs.IsNotFound(err) {
		return "", fmt.Errorf("inspect file utility path %s: %w", inside, err)
	} else {
		var archive bytes.Buffer
		tw := tar.NewWriter(&archive)
		if err := tw.WriteHeader(&tar.Header{Name: toolName, Mode: 0755, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
			return "", fmt.Errorf("prepare file utility: %w", err)
		}
		if _, err := tw.Write(data); err != nil {
			return "", fmt.Errorf("prepare file utility: %w", err)
		}
		if err := tw.Close(); err != nil {
			return "", fmt.Errorf("prepare file utility: %w", err)
		}
		if _, err := b.docker.CopyToContainer(ctx, id, client.CopyToContainerOptions{DestinationPath: destination, Content: &archive}); err != nil {
			return "", fmt.Errorf("copy file utility into container: %w", err)
		}
	}
	return inside, nil
}

// The packaged native utility works even when the container image has no shell.
func (b *Backend) runFileTool(ctx context.Context, id string, args []string) ([]byte, error) {
	stream, done, err := b.openFileToolRead(ctx, id, args)
	if err != nil {
		return nil, err
	}
	data, readErr := readLimited(stream, maxContainerTransfer)
	_ = stream.Close()
	finishErr := <-done
	return data, errors.Join(readErr, finishErr)
}

func (b *Backend) startFileTool(ctx context.Context, id string, args []string, stdin bool) (client.ExecAttachResult, string, error) {
	var inside string
	var err error
	if b.id == "windows" {
		_, _, inside, err = b.fileToolSource()
	} else {
		inside, err = b.prepareFileTool(ctx, id)
	}
	if err != nil {
		return client.ExecAttachResult{}, "", err
	}
	user := ""
	if b.id == "windows" {
		// Nano Server's default ContainerUser cannot read archive-injected
		// files. This matches the privileges of Docker's filesystem API.
		user = "ContainerAdministrator"
	}
	created, err := b.docker.ExecCreate(ctx, id, client.ExecCreateOptions{
		Cmd: append([]string{inside}, args...), User: user,
		AttachStdin: stdin, AttachStdout: true, AttachStderr: true,
	})
	if err == nil {
		var attached client.ExecAttachResult
		attached, err = b.docker.ExecAttach(ctx, created.ID, client.ExecAttachOptions{})
		if err == nil {
			return attached, created.ID, nil
		}
	}
	if b.id == "windows" {
		return client.ExecAttachResult{}, "", fmt.Errorf("start container file utility (use Prepare Files to install it; preparation may restart the container): %w", err)
	}
	return client.ExecAttachResult{}, "", fmt.Errorf("start container file utility: %w", err)
}

func (b *Backend) fileToolResult(ctx context.Context, execID string, stderr *cappedBuffer, streamErr error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if streamErr != nil {
		return fmt.Errorf("read file utility output: %w", streamErr)
	}
	result, err := b.docker.ExecInspect(ctx, execID, client.ExecInspectOptions{})
	if err != nil {
		return fmt.Errorf("inspect file utility status: %w", err)
	}
	if result.Running {
		return fmt.Errorf("file utility stream ended while the command was still running")
	}
	if result.ExitCode != 0 {
		message := strings.TrimSpace(stderr.String())
		if b.id == "windows" && !strings.Contains(message, "ucd-filetool:") {
			return fmt.Errorf("file utility failed with exit code %d: %s; use Prepare Files to install it (this can restart the container)", result.ExitCode, message)
		}
		return fmt.Errorf("file utility failed with exit code %d: %s", result.ExitCode, message)
	}
	return nil
}

type fileToolReader struct {
	*io.PipeReader
	cancel context.CancelFunc
}

func (r *fileToolReader) Close() error {
	r.cancel()
	return r.PipeReader.Close()
}

// openFileToolRead provides backpressured, demultiplexed stdout. Completion is
// reported both at the end of the stream and on done; closing it cancels exec.
func (b *Backend) openFileToolRead(ctx context.Context, id string, args []string) (io.ReadCloser, <-chan error, error) {
	streamCtx, cancel := context.WithCancel(ctx)
	attached, execID, err := b.startFileTool(streamCtx, id, args, false)
	if err != nil {
		cancel()
		return nil, nil, err
	}
	reader, writer := io.Pipe()
	done := make(chan error, 1)
	go func() {
		stopClose := context.AfterFunc(streamCtx, attached.Close)
		defer func() { stopClose(); attached.Close(); cancel() }()
		stderr := &cappedBuffer{limit: 64 << 10}
		streamErr := copyDockerStream(writer, stderr, attached.Reader)
		result := b.fileToolResult(streamCtx, execID, stderr, streamErr)
		done <- result
		_ = writer.CloseWithError(result)
	}()
	return &fileToolReader{PipeReader: reader, cancel: cancel}, done, nil
}

type fileToolWriter struct {
	*io.PipeWriter
	cancel        context.CancelFunc
	executionDone <-chan struct{}
	mu            sync.Mutex
	expected      int64
	written       int64
	pending       int64
	closed        bool
}

func (w *fileToolWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return 0, io.ErrClosedPipe
	}
	if int64(len(p)) > w.expected-w.written-w.pending {
		w.mu.Unlock()
		return 0, fmt.Errorf("file utility input exceeds declared size")
	}
	w.pending += int64(len(p))
	w.mu.Unlock()
	n, err := w.PipeWriter.Write(p)
	w.mu.Lock()
	w.pending -= int64(len(p))
	w.written += int64(n)
	w.mu.Unlock()
	return n, err
}

func (w *fileToolWriter) Close() error {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return nil
	}
	w.closed = true
	written, expected := w.written, w.expected
	w.mu.Unlock()
	if written != expected {
		err := fmt.Errorf("file utility input ended after %d of %d bytes", written, expected)
		select {
		case <-w.executionDone:
		default:
			w.cancel()
		}
		_ = w.PipeWriter.CloseWithError(err)
		return err
	}
	return w.PipeWriter.Close()
}

// openFileToolWrite feeds exactly the size declared in the utility arguments.
// The utility stops reading at that boundary, without requiring pipe half-close.
func (b *Backend) openFileToolWrite(ctx context.Context, id string, args []string) (io.WriteCloser, <-chan error, error) {
	if len(args) < 3 || (args[0] != "write" && args[0] != "extract") {
		return nil, nil, fmt.Errorf("file utility input requires write/extract and a declared size")
	}
	size, err := strconv.ParseInt(args[2], 10, 64)
	if err != nil || size < 0 {
		return nil, nil, fmt.Errorf("invalid file utility input size")
	}
	streamCtx, cancel := context.WithCancel(ctx)
	attached, execID, err := b.startFileTool(streamCtx, id, args, true)
	if err != nil {
		cancel()
		return nil, nil, err
	}
	reader, writer := io.Pipe()
	done := make(chan error, 1)
	executionDone := make(chan struct{})
	go func() {
		stopClose := context.AfterFunc(streamCtx, attached.Close)
		defer func() { stopClose(); attached.Close(); cancel() }()
		inputDone := make(chan error, 1)
		go func() {
			_, err := io.Copy(attached.Conn, reader)
			inputDone <- err
		}()
		stderr := &cappedBuffer{limit: 64 << 10}
		streamErr := copyDockerStream(io.Discard, stderr, attached.Reader)
		close(executionDone)
		// If the command ended early, release a blocked producer immediately.
		_ = reader.CloseWithError(streamErr)
		inputErr := <-inputDone
		result := b.fileToolResult(streamCtx, execID, stderr, streamErr)
		if result == nil && inputErr != nil && !errors.Is(inputErr, io.ErrClosedPipe) {
			result = fmt.Errorf("write file utility input: %w", inputErr)
		}
		done <- result
	}()
	return &fileToolWriter{PipeWriter: writer, cancel: cancel, executionDone: executionDone, expected: size}, done, nil
}

func (b *Backend) writeFileTool(ctx context.Context, id string, args []string, data io.Reader) error {
	writer, done, err := b.openFileToolWrite(ctx, id, args)
	if err != nil {
		return err
	}
	_, writeErr := io.Copy(writer, data)
	closeErr := writer.Close()
	return errors.Join(writeErr, closeErr, <-done)
}
