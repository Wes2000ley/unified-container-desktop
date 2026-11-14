package engine

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"strings"

	"github.com/moby/moby/api/types/registry"
	"github.com/moby/moby/client"
	"github.com/moby/patternmatcher"
	"github.com/moby/patternmatcher/ignorefile"
	"github.com/skyri/ucd/internal/api"
)

const maxResourceTransfer = 8 << 20
const maxResourceOutput = 4 << 20

func (b *Backend) imageResourceCall(ctx context.Context, req api.Request) (any, error) {
	p, err := api.Decode[api.ImageResourceParams](req)
	if err != nil {
		return nil, err
	}
	if len(p.OperationID) > 128 {
		return nil, api.Fail("invalid_argument", "operation ID must not exceed 128 bytes")
	}
	switch req.Method {
	case "image.inspect":
		if p.Reference == "" {
			return nil, api.Fail("invalid_argument", "image reference is required")
		}
		result, err := b.docker.ImageInspect(ctx, p.Reference)
		if err != nil {
			return nil, fmt.Errorf("inspect image: %w", err)
		}
		return result.InspectResponse, nil
	case "image.history":
		if p.Reference == "" {
			return nil, api.Fail("invalid_argument", "image reference is required")
		}
		result, err := b.docker.ImageHistory(ctx, p.Reference)
		if err != nil {
			return nil, fmt.Errorf("inspect image history: %w", err)
		}
		return result.Items, nil
	case "image.tag":
		if p.Reference == "" || p.Target == "" {
			return nil, api.Fail("invalid_argument", "source and target image references are required")
		}
		_, err := b.docker.ImageTag(ctx, client.ImageTagOptions{Source: p.Reference, Target: p.Target})
		if err != nil {
			return nil, fmt.Errorf("tag image: %w", err)
		}
		return nil, nil
	case "image.push":
		if p.Reference == "" {
			return nil, api.Fail("invalid_argument", "image reference is required")
		}
		auth, secrets, err := b.machineRegistryAuth(ctx, p.Reference)
		if err != nil {
			return nil, fmt.Errorf("resolve machine registry credentials: %w", err)
		}
		stream, err := b.docker.ImagePush(ctx, p.Reference, client.ImagePushOptions{RegistryAuth: auth})
		if err != nil {
			return nil, fmt.Errorf("push image: %w", redactResourceError(err, secrets))
		}
		defer stream.Close()
		return b.resourceProgress(stream, "push image", p.OperationID, secrets...)
	case "image.load", "image.import":
		if len(p.Data) == 0 || len(p.Data) > maxResourceTransfer {
			return nil, api.Fail("invalid_argument", "archive must contain 1 byte to 8 MiB; use the Docker CLI for larger transfers")
		}
		if req.Method == "image.load" {
			stream, err := b.docker.ImageLoad(ctx, bytes.NewReader(p.Data))
			if err != nil {
				return nil, fmt.Errorf("load image: %w", err)
			}
			defer stream.Close()
			return b.resourceProgress(stream, "load image", p.OperationID)
		}
		stream, err := b.docker.ImageImport(ctx, client.ImageImportSource{Source: bytes.NewReader(p.Data), SourceName: "-"}, p.Reference, client.ImageImportOptions{})
		if err != nil {
			return nil, fmt.Errorf("import image: %w", err)
		}
		defer stream.Close()
		return b.resourceProgress(stream, "import image", p.OperationID)
	case "image.save":
		if len(p.References) == 0 {
			return nil, api.Fail("invalid_argument", "at least one image reference is required")
		}
		for _, ref := range p.References {
			if ref == "" {
				return nil, api.Fail("invalid_argument", "image references cannot be empty")
			}
		}
		stream, err := b.docker.ImageSave(ctx, p.References)
		if err != nil {
			return nil, fmt.Errorf("save image: %w", err)
		}
		defer stream.Close()
		data, err := readResourceData(stream)
		if err != nil {
			return nil, fmt.Errorf("save image: %w", err)
		}
		return api.ResourceData{Data: data, Name: "images.tar"}, nil
	case "image.build":
		archive, err := buildContext(p)
		if err != nil {
			return nil, err
		}
		auths, secrets, err := b.buildRegistryAuth(ctx)
		if err != nil {
			return nil, err
		}
		buildArgs := ApplyProxyBuildArgs(buildArguments(p.BuildArgs), b.RuntimeConfiguration().ContainersProxy)
		stream, err := b.docker.ImageBuild(ctx, bytes.NewReader(archive), client.ImageBuildOptions{Dockerfile: buildDockerfilePath(p), Tags: p.Tags, BuildArgs: buildArgs, AuthConfigs: auths, Remove: true})
		if err != nil {
			return nil, fmt.Errorf("build image: %w", redactResourceError(err, secrets))
		}
		defer stream.Body.Close()
		return b.resourceProgress(stream.Body, "build image", p.OperationID, secrets...)
	default:
		return nil, api.Fail("invalid_argument", "unknown image method: "+req.Method)
	}
}

func (b *Backend) buildRegistryAuth(ctx context.Context) (map[string]registry.AuthConfig, []string, error) {
	if snapshot, ok := registrySnapshotFromContext(ctx); ok {
		auths := make(map[string]registry.AuthConfig, len(snapshot.AuthConfigs))
		var secrets []string
		for server, auth := range snapshot.AuthConfigs {
			auths[server] = registry.AuthConfig{
				Username: auth.Username, Password: auth.Password,
				IdentityToken: auth.IdentityToken, RegistryToken: auth.RegistryToken,
			}
			secrets = append(secrets, auth.Password, auth.IdentityToken, auth.RegistryToken)
		}
		return auths, secrets, nil
	}
	b.configMu.Lock()
	defer b.configMu.Unlock()
	if b.registryConfig.CredentialsStore != "" || len(b.registryConfig.CredentialHelpers) != 0 {
		return nil, nil, fmt.Errorf("build requires credentials in the machine Docker configuration; external credential helpers are not supported")
	}
	auths := make(map[string]registry.AuthConfig, len(b.registryConfig.AuthConfigs))
	var secrets []string
	for key, auth := range b.registryConfig.AuthConfigs {
		auths[key] = registry.AuthConfig(auth)
		secrets = append(secrets, auth.Password, auth.IdentityToken, auth.RegistryToken)
	}
	return auths, secrets, nil
}

func buildArguments(values map[string]string) map[string]*string {
	args := make(map[string]*string, len(values))
	for key, value := range values {
		value := value
		args[key] = &value
	}
	return args
}

func buildDockerfilePath(p api.ImageResourceParams) string {
	if p.DockerfilePath == "" {
		return "Dockerfile"
	}
	return p.DockerfilePath
}

// buildContext includes the supplied files after applying Docker's ignore rules.
func buildContext(p api.ImageResourceParams) ([]byte, error) {
	if p.Dockerfile == "" {
		return nil, api.Fail("invalid_argument", "Dockerfile contents are required")
	}
	if len(p.Files) > 10000 {
		return nil, api.Fail("invalid_argument", "build context contains too many files")
	}
	dockerfile := buildDockerfilePath(p)
	files := append([]api.File{{Path: dockerfile, Content: []byte(p.Dockerfile)}}, p.Files...)
	var patterns []string
	var rootIgnore, selectedIgnore []byte
	var hasSelectedIgnore bool
	for _, file := range p.Files {
		if file.Path == ".dockerignore" {
			rootIgnore = file.Content
		}
		if file.Path == dockerfile+".dockerignore" {
			selectedIgnore = file.Content
			hasSelectedIgnore = true
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
		return nil, fmt.Errorf("parse .dockerignore: %w", err)
	}
	seen := make(map[string]bool, len(files))
	var total int
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, file := range files {
		name := file.Path
		if name == "" || name != path.Clean(name) || strings.HasPrefix(name, "/") || name == "." || name == ".." || strings.HasPrefix(name, "../") || strings.ContainsAny(name, `\:`) || seen[name] {
			return nil, api.Fail("invalid_argument", "build context paths must be unique relative paths without traversal or backslashes")
		}
		seen[name] = true
		if name != dockerfile && name != ".dockerignore" && name != dockerfile+".dockerignore" {
			excluded, err := matcher.MatchesOrParentMatches(name)
			if err != nil {
				return nil, fmt.Errorf("match .dockerignore: %w", err)
			}
			if excluded {
				continue
			}
		}
		total += len(file.Content)
		if total > maxResourceTransfer {
			return nil, api.Fail("invalid_argument", "build context exceeds 8 MiB; use the Docker CLI for larger contexts")
		}
		mode := int64(file.Mode & 0777)
		if mode == 0 {
			mode = 0644
		}
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: mode, Size: int64(len(file.Content)), Typeflag: tar.TypeReg}); err != nil {
			return nil, fmt.Errorf("create build context: %w", err)
		}
		if _, err := tw.Write(file.Content); err != nil {
			return nil, fmt.Errorf("create build context: %w", err)
		}
	}
	if err := tw.Close(); err != nil {
		return nil, fmt.Errorf("create build context: %w", err)
	}
	return buf.Bytes(), nil
}

func readResourceData(r io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxResourceTransfer+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxResourceTransfer {
		return nil, fmt.Errorf("transfer exceeds 8 MiB; use the Docker CLI for larger transfers")
	}
	return data, nil
}

// resourceProgress consumes Docker's JSON stream, preserving real build/push logs.
func (b *Backend) resourceProgress(r io.Reader, action, operationID string, secrets ...string) (api.ResourceOutput, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 4096), maxResourceOutput)
	var output strings.Builder
	truncated := false
	for scanner.Scan() {
		var message struct {
			Stream      string `json:"stream"`
			Status      string `json:"status"`
			ID          string `json:"id"`
			Progress    string `json:"progress"`
			Error       string `json:"error"`
			ErrorDetail struct {
				Message string `json:"message"`
			} `json:"errorDetail"`
		}
		if len(bytes.TrimSpace(scanner.Bytes())) == 0 {
			continue
		}
		if err := json.Unmarshal(scanner.Bytes(), &message); err != nil {
			return api.ResourceOutput{Output: output.String()}, fmt.Errorf("%s: decode Docker output: %w", action, err)
		}
		if message.ErrorDetail.Message != "" {
			return api.ResourceOutput{Output: output.String()}, fmt.Errorf("%s: %s", action, redactResourceText(message.ErrorDetail.Message, secrets))
		}
		if message.Error != "" {
			return api.ResourceOutput{Output: output.String()}, fmt.Errorf("%s: %s", action, redactResourceText(message.Error, secrets))
		}
		line := message.Stream
		if line == "" {
			line = strings.TrimSpace(strings.Join([]string{message.ID, message.Status, message.Progress}, " "))
			if line != "" {
				line += "\n"
			}
		}
		if line == "" {
			continue
		}
		line = redactResourceText(line, secrets)
		if remaining := maxResourceOutput - output.Len(); remaining > 0 {
			if len(line) > remaining {
				output.WriteString(line[:remaining])
				truncated = true
			} else {
				output.WriteString(line)
			}
		} else {
			truncated = true
		}
		// Bound each event so a single stream record cannot evict the event history.
		if len(line) > 8192 {
			line = line[:8192]
		}
		b.events.Publish(api.Event{Engine: b.id, Type: "image.progress", Project: operationID, Message: line})
	}
	if err := scanner.Err(); err != nil {
		return api.ResourceOutput{Output: output.String()}, fmt.Errorf("%s: read Docker output: %w", action, err)
	}
	if truncated {
		output.WriteString("\n[Output truncated after 4 MiB; live progress was emitted during the operation.]\n")
	}
	return api.ResourceOutput{Output: redactResourceText(output.String(), secrets)}, nil
}

func redactResourceText(message string, secrets []string) string {
	for _, secret := range secrets {
		if secret != "" {
			message = strings.ReplaceAll(message, secret, "[redacted]")
		}
	}
	return message
}

type resourceRedactedError struct {
	err     error
	secrets []string
}

func (e resourceRedactedError) Error() string { return redactResourceText(e.err.Error(), e.secrets) }
func (e resourceRedactedError) Unwrap() error { return e.err }

func redactResourceError(err error, secrets []string) error {
	return resourceRedactedError{err: err, secrets: secrets}
}
