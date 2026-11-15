package engine

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/netip"
	"strconv"
	"strings"

	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
	"github.com/skyri/ucd/internal/api"
)

const maxContainerTransfer = 8 << 20

func (b *Backend) containerOperation(ctx context.Context, req api.Request) (any, error) {
	if req.Method == "container.create" || req.Method == "container.run" {
		p, err := api.Decode[api.ContainerCreateParams](req)
		if err != nil {
			return nil, err
		}
		if p.Config == nil || strings.TrimSpace(p.Config.Image) == "" {
			return nil, api.Fail("invalid_argument", "container config must specify an image")
		}
		settings := b.RuntimeConfiguration()
		p.Config.Env = ApplyContainerProxyEnvironment(p.Config.Env, settings.ContainersProxy)
		p.HostConfig = ApplyContainerDNSDefaults(b.id, p.HostConfig)
		if p.HostConfig != nil {
			if err := ValidateContainerNetworkMode(b.id, string(p.HostConfig.NetworkMode), settings.Network); err != nil {
				return nil, err
			}
		}
		if err := b.prepareContainerPorts(ctx, &p); err != nil {
			return nil, err
		}
		result, err := b.docker.ContainerCreate(ctx, client.ContainerCreateOptions{Config: p.Config, HostConfig: p.HostConfig, NetworkingConfig: p.NetworkingConfig, Name: p.Name})
		result, err = b.finishContainerCreate(ctx, result, err, p.HostConfig)
		if err != nil {
			return nil, fmt.Errorf("create container: %w", err)
		}
		if req.Method == "container.run" {
			action, _ := api.NewRequest("container.action", b.id, api.ContainerParams{ID: result.ID, Action: "start"})
			if _, err := b.containerCall(ctx, action); err != nil {
				return nil, fmt.Errorf("container %s was created but could not start: %w", result.ID, err)
			}
		}
		return api.ContainerCreateResult{ID: result.ID, Warnings: result.Warnings}, nil
	}
	p, err := api.Decode[api.ContainerOperationParams](req)
	if err != nil {
		return nil, err
	}
	if p.ID == "" {
		return nil, api.Fail("invalid_argument", "container ID is required")
	}
	switch req.Method {
	case "container.rename":
		if strings.TrimSpace(p.Name) == "" {
			return nil, api.Fail("invalid_argument", "container name is required")
		}
		_, err := b.docker.ContainerRename(ctx, p.ID, client.ContainerRenameOptions{NewName: p.Name})
		if err != nil {
			return nil, fmt.Errorf("rename container: %w", err)
		}
		return nil, nil
	case "container.update":
		if p.Resources == nil && p.RestartPolicy == nil {
			return nil, api.Fail("invalid_argument", "resources or restart policy is required")
		}
		result, err := b.docker.ContainerUpdate(ctx, p.ID, client.ContainerUpdateOptions{Resources: p.Resources, RestartPolicy: p.RestartPolicy})
		if err != nil {
			return nil, fmt.Errorf("update container: %w", err)
		}
		return struct {
			Warnings []string `json:"warnings"`
		}{result.Warnings}, nil
	case "container.top":
		var args []string
		if p.PSArgs != "" {
			args = []string{p.PSArgs}
		}
		result, err := b.docker.ContainerTop(ctx, p.ID, client.ContainerTopOptions{Arguments: args})
		if err != nil {
			return nil, fmt.Errorf("list container processes: %w", err)
		}
		return result, nil
	case "container.diff":
		result, err := b.docker.ContainerDiff(ctx, p.ID, client.ContainerDiffOptions{})
		if err != nil {
			return nil, fmt.Errorf("list container filesystem changes: %w", err)
		}
		return result.Changes, nil
	case "container.commit":
		result, err := b.docker.ContainerCommit(ctx, p.ID, client.ContainerCommitOptions{Reference: p.Reference, Author: p.Author, Comment: p.Comment, Changes: p.Changes, NoPause: p.Pause != nil && !*p.Pause})
		if err != nil {
			return nil, fmt.Errorf("commit container: %w", err)
		}
		return struct {
			ID string `json:"id"`
		}{result.ID}, nil
	case "container.export":
		result, err := b.docker.ContainerExport(ctx, p.ID, client.ContainerExportOptions{})
		if err != nil {
			return nil, fmt.Errorf("export container: %w", err)
		}
		defer result.Close()
		data, err := readLimited(result, maxContainerTransfer)
		if err != nil {
			return nil, fmt.Errorf("export container: %w", err)
		}
		return api.BinaryResult{Data: data, Name: "container.tar"}, nil
	default:
		return nil, api.Fail("invalid_argument", "unknown container operation")
	}
}

func (b *Backend) prepareContainerPorts(ctx context.Context, p *api.ContainerCreateParams) error {
	if b.id == "windows" {
		if _, exists := p.Config.Labels[windowsPortsLabel]; exists {
			return api.Fail("invalid_argument", "container uses reserved published port label")
		}
	}
	if b.id == "linux" {
		if _, exists := p.Config.Labels[linuxPortsLabel]; exists {
			return api.Fail("invalid_argument", "container uses reserved Linux published port label")
		}
	}
	if p.HostConfig == nil {
		if b.id == "linux" {
			clearInheritedLinuxPortLabel(p)
			return nil
		}
		return nil
	}
	if p.HostConfig.PublishAllPorts {
		image, err := b.docker.ImageInspect(ctx, p.Config.Image)
		if err != nil {
			return fmt.Errorf("inspect image exposed ports: %w", err)
		}
		if p.HostConfig.PortBindings == nil {
			p.HostConfig.PortBindings = make(network.PortMap)
		}
		exposed := make(network.PortSet)
		for port := range p.Config.ExposedPorts {
			exposed[port] = struct{}{}
		}
		if image.Config != nil {
			for raw := range image.Config.ExposedPorts {
				port, err := network.ParsePort(raw)
				if err != nil {
					return fmt.Errorf("invalid image exposed port %q: %w", raw, err)
				}
				exposed[port] = struct{}{}
			}
		}
		for port := range exposed {
			if len(p.HostConfig.PortBindings[port]) == 0 {
				p.HostConfig.PortBindings[port] = []network.PortBinding{{}}
			}
		}
		p.HostConfig.PublishAllPorts = false
	}
	defaultHostIP := netip.MustParseAddr("127.0.0.1")
	if b.RuntimeConfiguration().Network.DefaultPortBinding == api.DefaultPortBindingAll {
		defaultHostIP = netip.IPv4Unspecified()
	}
	for port, bindings := range p.HostConfig.PortBindings {
		for i := range bindings {
			if !bindings[i].HostIP.IsValid() {
				bindings[i].HostIP = defaultHostIP
			}
		}
		p.HostConfig.PortBindings[port] = bindings
	}
	ports, err := api.ContainerPublishedPorts(*p)
	if err != nil {
		return err
	}
	if b.id == "linux" {
		if len(ports) > 0 && linuxPortRelayEnabled(p.HostConfig) {
			return prepareLinuxContainerPorts(p, ports)
		}
		clearInheritedLinuxPortLabel(p)
		return nil
	}
	if b.id != "windows" {
		return nil
	}
	if len(ports) == 0 {
		return nil
	}
	if p.Config.ExposedPorts == nil {
		p.Config.ExposedPorts = make(network.PortSet)
	}
	for port := range p.HostConfig.PortBindings {
		p.Config.ExposedPorts[port] = struct{}{}
	}
	metadata, err := json.Marshal(ports)
	if err != nil {
		return fmt.Errorf("encode published port metadata: %w", err)
	}
	if p.Config.Labels == nil {
		p.Config.Labels = make(map[string]string)
	}
	p.Config.Labels[windowsPortsLabel] = string(metadata)
	p.HostConfig.PortBindings = nil
	return nil
}

func readLimited(r io.Reader, max int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, fmt.Errorf("data exceeds %d-byte transfer limit", max)
	}
	return data, nil
}

type cappedBuffer struct {
	buffer bytes.Buffer
	limit  int
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if len(p) > b.limit-b.buffer.Len() {
		return 0, fmt.Errorf("output exceeds %d-byte limit", b.limit)
	}
	return b.buffer.Write(p)
}
func (b *cappedBuffer) String() string { return b.buffer.String() }
func (b *cappedBuffer) Bytes() []byte  { return b.buffer.Bytes() }

func (b *Backend) containerLogs(ctx context.Context, req api.Request) (any, error) {
	p, options, tty, err := b.containerLogOptions(ctx, req)
	if err != nil {
		return nil, err
	}
	body, err := b.docker.ContainerLogs(ctx, p.ID, options)
	if err != nil {
		return nil, fmt.Errorf("read container logs: %w", err)
	}
	defer body.Close()
	out := &cappedBuffer{limit: 4 << 20}
	if err := copyLogStream(out, body, tty); err != nil {
		return nil, fmt.Errorf("decode container logs: %w", err)
	}
	return api.LogResult{Text: out.String()}, nil
}

func (b *Backend) containerLogOptions(ctx context.Context, req api.Request) (api.ContainerLogParams, client.ContainerLogsOptions, bool, error) {
	p, err := api.Decode[api.ContainerLogParams](req)
	if err != nil {
		return p, client.ContainerLogsOptions{}, false, err
	}
	if p.ID == "" {
		return p, client.ContainerLogsOptions{}, false, api.Fail("invalid_argument", "container ID is required")
	}
	if p.Tail < 0 || p.Tail > 100000 {
		return p, client.ContainerLogsOptions{}, false, api.Fail("invalid_argument", "tail must be between 0 and 100000")
	}
	if p.Tail == 0 {
		p.Tail = 500
	}
	stdout, stderr := true, true
	if p.Stdout != nil || p.Stderr != nil {
		stdout, stderr = p.Stdout != nil && *p.Stdout, p.Stderr != nil && *p.Stderr
		if !stdout && !stderr {
			return p, client.ContainerLogsOptions{}, false, api.Fail("invalid_argument", "select stdout or stderr")
		}
	}
	info, err := b.docker.ContainerInspect(ctx, p.ID, client.ContainerInspectOptions{})
	if err != nil {
		return p, client.ContainerLogsOptions{}, false, fmt.Errorf("inspect container for logs: %w", err)
	}
	options := client.ContainerLogsOptions{ShowStdout: stdout, ShowStderr: stderr, Tail: strconv.Itoa(p.Tail), Since: p.Since, Until: p.Until, Timestamps: p.Timestamps}
	return p, options, info.Container.Config != nil && info.Container.Config.Tty, nil
}

func copyLogStream(w io.Writer, r io.Reader, tty bool) error {
	var err error
	if tty {
		_, err = io.Copy(w, r)
	} else {
		err = copyDockerStream(w, w, r)
	}
	return err
}

// Docker's multiplexed stream uses an eight-byte header per frame. Copy each
// payload incrementally so a large frame cannot defeat session backpressure.
func copyDockerStream(stdout, stderr io.Writer, r io.Reader) error {
	var header [8]byte
	for {
		_, err := io.ReadFull(r, header[:])
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read Docker stream header: %w", err)
		}
		size := int64(binary.BigEndian.Uint32(header[4:]))
		var target io.Writer
		switch header[0] {
		case 0, 1:
			target = stdout
		case 2:
			target = stderr
		case 3:
			if size > 64<<10 {
				return fmt.Errorf("Docker stream error exceeds 65536 bytes")
			}
			message, err := io.ReadAll(io.LimitReader(r, size))
			if err != nil {
				return err
			}
			if int64(len(message)) != size {
				return io.ErrUnexpectedEOF
			}
			return fmt.Errorf("Docker stream error: %s", message)
		default:
			return fmt.Errorf("unknown Docker stream type %d", header[0])
		}
		if _, err := io.CopyN(target, r, size); err != nil {
			return fmt.Errorf("read Docker stream payload: %w", err)
		}
	}
}

func (b *Backend) containerLogStreamCall(ctx context.Context, req api.Request) (any, error) {
	if req.Method != "container.logs.open" {
		return b.outputSessionCall(ctx, req)
	}
	p, options, tty, err := b.containerLogOptions(ctx, req)
	if err != nil {
		return nil, err
	}
	options.Follow = true
	streamCtx, cancel, detach := newStreamContext(ctx)
	defer detach()
	body, err := b.docker.ContainerLogs(streamCtx, p.ID, options)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("follow container logs: %w", err)
	}
	id, err := b.addOutputSession(func() { _ = body.Close() }, cancel, func(w io.Writer) error { return copyLogStream(w, body, tty) }, nil, "", nil, "container.logs")
	if err != nil {
		return nil, err
	}
	if err := b.finishSessionOpen(ctx, id); err != nil {
		return nil, fmt.Errorf("open log session: %w", err)
	}
	return api.ExecOpenResult{SessionID: id}, nil
}
