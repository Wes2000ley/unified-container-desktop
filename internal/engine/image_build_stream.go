package engine

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"

	"github.com/moby/moby/client"
	"github.com/moby/patternmatcher"
	"github.com/moby/patternmatcher/ignorefile"
	"github.com/skyri/ucd/internal/api"
)

const maxBuildContextEntries = 250000
const maxBuildIgnoreBytes = 4 << 20

// streamImageBuild spools the input tar privately, then filters it while sending
// it to Docker. The build context is never buffered in memory.
func (b *Backend) streamImageBuild(ctx context.Context, source io.Reader, p api.TransferOpenParams) (result api.ResourceOutput, retErr error) {
	if p.Dockerfile == "" || len(p.Dockerfile) > maxBuildIgnoreBytes {
		return result, api.Fail("invalid_argument", "Dockerfile contents must contain 1 byte to 4 MiB")
	}
	if len(p.OperationID) > 128 {
		return result, api.Fail("invalid_argument", "operation ID must not exceed 128 bytes")
	}
	dockerfile := p.DockerfilePath
	if dockerfile == "" {
		dockerfile = "Dockerfile"
	}
	if err := validateBuildPath(dockerfile); err != nil {
		return result, err
	}
	auths, secrets, err := b.buildRegistryAuth(ctx)
	if err != nil {
		return result, err
	}
	archive, err := os.CreateTemp(b.dataRoot, ".ucd-build-context-*")
	if err != nil {
		return result, fmt.Errorf("create temporary build context: %w", err)
	}
	defer func() {
		closeErr := archive.Close()
		removeErr := os.Remove(archive.Name())
		if closeErr != nil {
			retErr = errors.Join(retErr, fmt.Errorf("close temporary build context: %w", closeErr))
		}
		if removeErr != nil {
			retErr = errors.Join(retErr, fmt.Errorf("remove temporary build context: %w", removeErr))
		}
	}()
	if _, err := io.Copy(archive, &contextReader{ctx: ctx, reader: source}); err != nil {
		return result, fmt.Errorf("write temporary build context: %w", err)
	}
	if _, err := archive.Seek(0, io.SeekStart); err != nil {
		return result, fmt.Errorf("rewind build context: %w", err)
	}
	matcher, err := inspectBuildArchive(ctx, archive, dockerfile)
	if err != nil {
		return result, err
	}
	if _, err := archive.Seek(0, io.SeekStart); err != nil {
		return result, fmt.Errorf("rewind build context: %w", err)
	}
	reader, writer := io.Pipe()
	done := make(chan error, 1)
	go func() {
		err := writeFilteredBuildArchive(ctx, archive, writer, matcher, dockerfile, p.Dockerfile)
		_ = writer.CloseWithError(err)
		done <- err
	}()
	buildArgs := ApplyProxyBuildArgs(buildArguments(p.BuildArgs), b.RuntimeConfiguration().ContainersProxy)
	stream, err := b.docker.ImageBuild(ctx, reader, client.ImageBuildOptions{Dockerfile: dockerfile, Tags: p.Tags, BuildArgs: buildArgs, AuthConfigs: auths, Remove: true})
	if err != nil {
		_ = reader.CloseWithError(err)
		writeErr := <-done
		if writeErr != nil {
			return result, errors.Join(fmt.Errorf("build image: %w", redactResourceError(err, secrets)), fmt.Errorf("prepare build context: %w", writeErr))
		}
		return result, fmt.Errorf("build image: %w", redactResourceError(err, secrets))
	}
	defer stream.Body.Close()
	result, err = b.resourceProgress(stream.Body, "build image", p.OperationID, secrets...)
	_ = reader.Close()
	writeErr := <-done
	if writeErr != nil {
		err = errors.Join(err, fmt.Errorf("prepare build context: %w", writeErr))
	}
	return result, err
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *contextReader) Read(data []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(data)
}

func validateBuildPath(name string) error {
	if len(name) > 4096 || name == "." || !fs.ValidPath(name) || strings.ContainsAny(name, "\\:\x00") {
		return api.Fail("invalid_argument", "build context paths must be relative, portable paths without traversal")
	}
	return nil
}

// inspectBuildArchive validates all headers and reads only the applicable ignore files.
func inspectBuildArchive(ctx context.Context, source io.Reader, dockerfile string) (*patternmatcher.PatternMatcher, error) {
	input := &contextReader{ctx: ctx, reader: source}
	reader := tar.NewReader(input)
	seen := make(map[string]bool)
	var rootIgnore, selectedIgnore []byte
	var hasSelectedIgnore bool
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read build context archive: %w", err)
		}
		name := strings.TrimSuffix(header.Name, "/")
		if err := validateBuildPath(name); err != nil {
			return nil, err
		}
		if seen[name] {
			return nil, fmt.Errorf("build context contains duplicate path %q", name)
		}
		seen[name] = true
		if len(seen) > maxBuildContextEntries {
			return nil, fmt.Errorf("build context contains more than %d entries", maxBuildContextEntries)
		}
		if header.Size < 0 {
			return nil, fmt.Errorf("build context contains an invalid file size")
		}
		for key := range header.PAXRecords {
			if strings.HasPrefix(key, "GNU.sparse") || key == "SCHILY.realsize" {
				return nil, fmt.Errorf("build context contains an unsupported sparse file")
			}
		}
		switch header.Typeflag {
		case tar.TypeReg, tar.TypeRegA:
		case tar.TypeDir:
			if header.Size != 0 {
				return nil, fmt.Errorf("build context directory has file content")
			}
		default:
			return nil, fmt.Errorf("build context contains a symbolic link or special file at %q", name)
		}
		if name == ".dockerignore" || name == dockerfile+".dockerignore" {
			if header.Typeflag == tar.TypeDir {
				return nil, fmt.Errorf("Docker ignore file must be a regular file")
			}
			if header.Size > maxBuildIgnoreBytes {
				return nil, fmt.Errorf("Docker ignore file exceeds 4 MiB")
			}
			data, err := io.ReadAll(io.LimitReader(reader, maxBuildIgnoreBytes+1))
			if err != nil {
				return nil, fmt.Errorf("read Docker ignore file: %w", err)
			}
			if name == ".dockerignore" {
				rootIgnore = data
			}
			if name == dockerfile+".dockerignore" {
				selectedIgnore = data
				hasSelectedIgnore = true
			}
		}
	}
	// The tar end marker may be followed by padding, but not unvalidated data.
	var trailing [32 << 10]byte
	for {
		n, err := input.Read(trailing[:])
		for _, value := range trailing[:n] {
			if value != 0 {
				return nil, fmt.Errorf("build context has data after the archive")
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read build context trailer: %w", err)
		}
	}
	ignore := rootIgnore
	if hasSelectedIgnore {
		ignore = selectedIgnore
	}
	patterns, err := ignorefile.ReadAll(bytes.NewReader(ignore))
	if err != nil {
		return nil, fmt.Errorf("read Docker ignore file: %w", err)
	}
	matcher, err := patternmatcher.New(patterns)
	if err != nil {
		return nil, fmt.Errorf("parse Docker ignore file: %w", err)
	}
	return matcher, nil
}

func writeFilteredBuildArchive(ctx context.Context, source io.Reader, destination io.Writer, matcher *patternmatcher.PatternMatcher, dockerfile, content string) error {
	writer := tar.NewWriter(destination)
	if err := writer.WriteHeader(&tar.Header{Name: dockerfile, Mode: 0644, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
		return err
	}
	if _, err := io.WriteString(writer, content); err != nil {
		return err
	}
	reader := tar.NewReader(&contextReader{ctx: ctx, reader: source})
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("read build context: %w", err)
		}
		name := strings.TrimSuffix(header.Name, "/")
		if name == dockerfile {
			continue
		}
		if name != ".dockerignore" && name != dockerfile+".dockerignore" {
			excluded, err := matcher.MatchesOrParentMatches(name)
			if err != nil {
				return fmt.Errorf("match Docker ignore file: %w", err)
			}
			if excluded {
				continue
			}
		}
		if err := writer.WriteHeader(header); err != nil {
			return fmt.Errorf("write build context header: %w", err)
		}
		if _, err := io.Copy(writer, reader); err != nil {
			return fmt.Errorf("write build context file: %w", err)
		}
	}
	if err := writer.Close(); err != nil {
		return fmt.Errorf("finish build context: %w", err)
	}
	return nil
}
