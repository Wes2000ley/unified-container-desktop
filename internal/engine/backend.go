// Package engine connects the controller to a Docker Engine and Compose SDK.
package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/docker/cli/cli/command"
	"github.com/docker/cli/cli/config"
	"github.com/docker/cli/cli/config/configfile"
	"github.com/docker/cli/cli/config/types"
	"github.com/docker/cli/cli/flags"
	dockeropts "github.com/docker/cli/opts"
	composeapi "github.com/docker/compose/v5/pkg/api"
	"github.com/docker/compose/v5/pkg/compose"
	"github.com/moby/moby/client"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/skyri/ucd/internal/api"
)

// Backend owns a connection to one machine-owned Docker Engine.
type Backend struct {
	id               string
	dataRoot         string
	hostIPv4         func() (netip.Addr, error)
	docker           *client.Client
	compose          composeapi.Compose
	composeCLI       *command.DockerCli
	events           *api.EventBus
	registryConfig   *configfile.ConfigFile
	runtimeConfig    api.EngineConfiguration
	configMu         sync.Mutex
	projectMu        sync.Mutex
	portMu           sync.Mutex
	forwards         map[forwardKey]*portForward
	portReconcile    chan struct{}
	linuxPortsMu     sync.RWMutex
	linuxPortState   map[forwardKey]api.LinuxPortBinding
	linuxPortChanged chan struct{}
	linuxRelayPorts  map[linuxRelayKey]linuxFamilyRelay
	sessionMu        sync.Mutex
	sessions         map[string]*execSession
	fileToolMu       sync.Mutex
	transferMu       sync.Mutex
	transfers        map[string]*transferSession
	transfersClosed  bool
}

// New constructs a backend without requiring the engine to be online.
func New(engineID, endpoint, dataRoot string, events *api.EventBus) (*Backend, error) {
	if !api.ValidEngine(engineID) || endpoint == "" || !filepath.IsAbs(dataRoot) {
		return nil, fmt.Errorf("valid engine, Docker endpoint, and absolute data root are required")
	}
	dataRoot = filepath.Clean(dataRoot)
	if _, err := dockeropts.ParseHost(false, endpoint); err != nil {
		return nil, fmt.Errorf("invalid Docker endpoint: %w", err)
	}
	if events == nil {
		events = &api.EventBus{}
	}
	configDir := filepath.Join(dataRoot, "docker-config")
	if err := os.MkdirAll(configDir, 0700); err != nil {
		return nil, fmt.Errorf("create machine Docker configuration directory: %w", err)
	}
	// Load only the machine configuration. Do not discover a user's credential helper.
	configFile, err := config.Load(configDir)
	if err != nil {
		return nil, fmt.Errorf("load machine Docker configuration: %w", err)
	}
	runtimeConfig, err := loadRuntimeConfiguration(dataRoot)
	if err != nil {
		return nil, fmt.Errorf("load machine engine configuration: %w", err)
	}
	docker, err := client.New(client.WithHost(endpoint), client.WithTraceProvider(noop.NewTracerProvider()))
	if err != nil {
		return nil, fmt.Errorf("create Docker client: %w", err)
	}
	cli, err := command.NewDockerCli(command.WithAPIClient(docker), command.WithOutputStream(io.Discard), command.WithErrorStream(io.Discard), command.WithInputStream(io.NopCloser(strings.NewReader(""))))
	if err == nil {
		opts := flags.NewClientOptions()
		opts.ConfigDir = configDir
		opts.Hosts = []string{endpoint}
		err = cli.Initialize(opts)
	}
	if err != nil {
		_ = docker.Close()
		return nil, fmt.Errorf("initialize Docker CLI adapter: %w", err)
	}
	b := &Backend{id: engineID, dataRoot: dataRoot, docker: docker, events: events, registryConfig: configFile, runtimeConfig: runtimeConfig, portReconcile: make(chan struct{}, 1), linuxPortChanged: make(chan struct{}), hostIPv4: windowsHostIPv4}
	b.composeCLI = cli
	b.compose, err = b.newCompose(context.Background(), io.Discard, "")
	if err != nil {
		_ = docker.Close()
		return nil, fmt.Errorf("create Compose service: %w", err)
	}
	return b, nil
}

// RuntimeConfiguration returns the validated machine settings currently applied to this engine.
func (b *Backend) RuntimeConfiguration() api.EngineConfiguration {
	b.configMu.Lock()
	defer b.configMu.Unlock()
	result := b.runtimeConfig
	result.RegistryMirrors = append([]string(nil), b.runtimeConfig.RegistryMirrors...)
	return result
}

// newCompose creates isolated output streams and a machine-credential snapshot
// for an operation. The lock covers only the snapshot, never network or build work.
func (b *Backend) newCompose(ctx context.Context, out io.Writer, project string) (composeapi.Compose, error) {
	snapshot, err := b.composeConfigSnapshot(ctx)
	if err != nil {
		return nil, err
	}
	proxy := b.RuntimeConfiguration().ContainersProxy
	return compose.NewComposeService(&machineCLI{DockerCli: b.composeCLI, file: snapshot, docker: b.docker, backend: b}, compose.WithProxyConfig(composeBuildProxyConfig(proxy)), compose.WithEventProcessor(progress{engine: b.id, project: project, events: b.events}), compose.WithStreams(out, out, strings.NewReader("")), compose.WithPrompt(func(string, bool) (bool, error) {
		return false, fmt.Errorf("interactive Compose prompts are not supported")
	}))
}

func (b *Backend) composeConfigSnapshot(ctx context.Context) (*configfile.ConfigFile, error) {
	b.configMu.Lock()
	data, err := json.Marshal(b.registryConfig)
	var snapshot configfile.ConfigFile
	if err == nil {
		err = json.Unmarshal(data, &snapshot)
		snapshot.Filename = b.registryConfig.Filename
	}
	b.configMu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("copy machine Docker configuration: %w", err)
	}
	if snapshot.Proxies == nil {
		snapshot.Proxies = make(map[string]configfile.ProxyConfig)
	}
	// The Compose client wrapper applies container proxy defaults after parsing
	// the service environment, preserving explicit values across case variants.
	snapshot.Proxies["default"] = configfile.ProxyConfig{}
	if registrySnapshot, ok := registrySnapshotFromContext(ctx); ok {
		snapshot.AuthConfigs = make(map[string]types.AuthConfig, len(registrySnapshot.AuthConfigs))
		for server, auth := range registrySnapshot.AuthConfigs {
			snapshot.AuthConfigs[server] = types.AuthConfig{
				ServerAddress: server,
				Username:      auth.Username,
				Password:      auth.Password,
				IdentityToken: auth.IdentityToken,
				RegistryToken: auth.RegistryToken,
			}
		}
	}
	if b.id == "windows" {
		executable, err := os.Executable()
		if err != nil {
			return nil, fmt.Errorf("locate bundled Docker plugins: %w", err)
		}
		dir := filepath.Join(filepath.Dir(executable), "cli-plugins")
		snapshot.CLIPluginsExtraDirs = append([]string{dir}, snapshot.CLIPluginsExtraDirs...)
	}
	return &snapshot, nil
}

type machineCLI struct {
	*command.DockerCli
	file    *configfile.ConfigFile
	docker  *client.Client
	backend *Backend
}

func (c *machineCLI) ConfigFile() *configfile.ConfigFile { return c.file }
func (c *machineCLI) Client() client.APIClient {
	return &composeDefaultsClient{APIClient: c.docker, backend: c.backend}
}

type progress struct {
	engine  string
	project string
	events  *api.EventBus
}

func (p progress) Start(_ context.Context, operation string) {
	p.events.Publish(api.Event{Engine: p.engine, Project: p.project, Type: "compose.progress", Message: operation + " started"})
}
func (p progress) On(resources ...composeapi.Resource) {
	for _, r := range resources {
		p.events.Publish(api.Event{Engine: p.engine, Project: p.project, Type: "compose.progress", Message: strings.TrimSpace(r.ID + " " + r.Text)})
	}
}
func (p progress) Done(operation string, failed bool) {
	message := operation + " completed"
	// Compose v5.7.1's Run passes err != nil, despite naming this parameter
	// "success" in its EventProcessor interface.
	if failed {
		message = operation + " failed"
	}
	p.events.Publish(api.Event{Engine: p.engine, Project: p.project, Type: "compose.progress", Message: message})
}

// Close releases the Docker connection.
func (b *Backend) Close() error {
	transferErr := b.closeTransfers()
	b.closeExecSessions()
	b.closeWindowsPorts()
	b.closeLinuxPortRelays()
	return errors.Join(transferErr, b.docker.Close())
}

// Call executes one engine request.
func (b *Backend) Call(ctx context.Context, r api.Request) (any, error) {
	if r.Engine != "" && r.Engine != b.id {
		return nil, api.Fail("invalid_argument", "request targets a different engine")
	}
	switch {
	case r.Method == "engine.status":
		return b.status(ctx), nil
	case r.Method == "engine.configure":
		return b.configure(r)
	case r.Method == "engine.internal.hostaliases":
		return nil, b.configureHostAliases(r)
	case r.Method == "engine.internal.portbindings":
		return b.linuxPortBindings(ctx)
	case r.Method == "engine.internal.portstate":
		return nil, b.updateLinuxPortState(ctx, r)
	case strings.HasPrefix(r.Method, "compose."):
		return b.composeCall(ctx, r)
	case r.Method == "project.discover":
		return b.discover(ctx)
	case strings.HasPrefix(r.Method, "container."):
		return b.containerCall(ctx, r)
	case strings.HasPrefix(r.Method, "image."):
		return b.imageCall(ctx, r)
	case strings.HasPrefix(r.Method, "volume."):
		return b.volumeCall(ctx, r)
	case strings.HasPrefix(r.Method, "network."):
		return b.networkCall(ctx, r)
	case strings.HasPrefix(r.Method, "storage."):
		return b.storageCall(ctx, r)
	case strings.HasPrefix(r.Method, "registry."):
		return b.registryCall(ctx, r)
	case strings.HasPrefix(r.Method, "transfer."):
		return b.transferCall(ctx, r)
	default:
		return nil, api.Fail("invalid_argument", "unknown engine method: "+r.Method)
	}
}
