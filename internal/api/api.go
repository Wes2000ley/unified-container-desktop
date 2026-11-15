// Package api defines the local controller and authenticated Linux helper contract.
package api

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
)

const (
	PipeName      = `\\.\pipe\UnifiedContainerDesktop`
	ServiceName   = "UnifiedContainerDesktop"
	OperatorGroup = "Container Operators"
	Version       = "0.4.14"
)

type Request struct {
	Method string          `json:"method"`
	Engine string          `json:"engine,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
}

type Caller interface {
	Call(context.Context, Request) (any, error)
}
type Response struct {
	Result any    `json:"result,omitempty"`
	Error  *Error `json:"error,omitempty"`
}
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string        { return e.Message }
func Fail(code, message string) error { return &Error{Code: code, Message: message} }
func Decode[T any](r Request) (T, error) {
	var p T
	if len(r.Params) == 0 {
		r.Params = json.RawMessage(`{}`)
	}
	if err := json.Unmarshal(r.Params, &p); err != nil {
		return p, Fail("invalid_argument", fmt.Sprintf("invalid %s parameters: %v", r.Method, err))
	}
	return p, nil
}
func NewRequest(method, engine string, params any) (Request, error) {
	b, err := json.Marshal(params)
	return Request{Method: method, Engine: engine, Params: b}, err
}

type Principal struct {
	SID  string `json:"sid"`
	Name string `json:"name"`
	Role string `json:"role"`
}
type principalKey struct{}

func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}
func CurrentPrincipal(ctx context.Context) Principal {
	p, _ := ctx.Value(principalKey{}).(Principal)
	return p
}
func Authorize(p Principal, method string) error {
	if p.SID == "" {
		return Fail("unauthenticated", "Windows identity is required")
	}
	if strings.HasPrefix(method, "registry.internal.") || strings.HasPrefix(method, "engine.internal.") {
		return Fail("permission_denied", "internal service operations are not available to local clients")
	}
	switch method {
	case "system.configure", "engine.configure", "wsl.configure", "engine.start", "engine.stop", "operator.add", "operator.remove", "share.create", "share.remove":
		if p.Role != "administrator" {
			return Fail("permission_denied", "Windows administrator rights are required")
		}
	case "engine.list", "engine.status", "project.list", "project.logs.open", "project.logs.read", "project.logs.close", "container.list", "container.logs", "container.logs.open", "container.logs.read", "container.logs.close", "container.stats", "image.list", "volume.list", "volume.inspect", "network.list", "network.inspect", "registry.list", "storage.df", "storage.preview", "system.settings", "system.events", "system.diagnostics", "operator.list", "wsl.status", "share.list":
		return nil
	default:
		if p.Role != "administrator" && p.Role != "operator" {
			return Fail("permission_denied", "Container Operators membership is required")
		}
	}
	return nil
}

type EngineStatus struct {
	ID               string `json:"id"`
	OS               string `json:"os"`
	Online           bool   `json:"online"`
	ResourceSaver    bool   `json:"resourceSaver"`
	ActiveOperations int    `json:"activeOperations,omitempty"`
	Version          string `json:"version,omitempty"`
	Containers       int    `json:"containers"`
	Running          int    `json:"running"`
	Stopped          int    `json:"stopped"`
	CPUs             int    `json:"cpus"`
	MemoryBytes      uint64 `json:"memoryBytes"`
	Error            string `json:"error,omitempty"`
}
type Port struct {
	HostIP    string `json:"hostIP"`
	Published uint16 `json:"published"`
	Target    uint16 `json:"target"`
	Protocol  string `json:"protocol"`
}
type Container struct {
	ID               string    `json:"id"`
	Name             string    `json:"name"`
	Engine           string    `json:"engine"`
	Image            string    `json:"image"`
	State            string    `json:"state"`
	Status           string    `json:"status"`
	Project          string    `json:"project,omitempty"`
	Service          string    `json:"service,omitempty"`
	Ports            []Port    `json:"ports"`
	CPUPercent       float64   `json:"cpuPercent"`
	MemoryBytes      uint64    `json:"memoryBytes"`
	MetricsAvailable bool      `json:"metricsAvailable"`
	MemoryLimitBytes *uint64   `json:"memoryLimitBytes,omitempty"`
	NetworkRxBytes   *uint64   `json:"networkRxBytes,omitempty"`
	NetworkTxBytes   *uint64   `json:"networkTxBytes,omitempty"`
	BlockReadBytes   *uint64   `json:"blockReadBytes,omitempty"`
	BlockWriteBytes  *uint64   `json:"blockWriteBytes,omitempty"`
	Pids             *uint64   `json:"pids,omitempty"`
	SampleTime       time.Time `json:"sampleTime"`
}
type Image struct {
	ID         string   `json:"id"`
	Engine     string   `json:"engine"`
	Tags       []string `json:"tags"`
	Size       int64    `json:"size"`
	Created    int64    `json:"created"`
	Containers int64    `json:"containers"`
}
type Volume struct {
	Name       string            `json:"name"`
	Engine     string            `json:"engine"`
	Driver     string            `json:"driver"`
	Mountpoint string            `json:"mountpoint"`
	Labels     map[string]string `json:"labels,omitempty"`
	Size       int64             `json:"size"`
	UsageCount int64             `json:"usageCount"`
}
type ContainerParams struct {
	ID     string `json:"id"`
	Action string `json:"action,omitempty"`
	Tail   int    `json:"tail,omitempty"`
	Force  bool   `json:"force,omitempty"`
}
type ImageParams struct {
	Reference string `json:"reference"`
	ID        string `json:"id,omitempty"`
}
type VolumeParams struct {
	Name string `json:"name"`
}

type ProjectPart struct {
	Engine      string `json:"engine"`
	ComposeFile string `json:"composeFile"`
	Directory   string `json:"directory"`
	Name        string `json:"name"`
}
type Project struct {
	ID           string        `json:"id"`
	Name         string        `json:"name"`
	Parts        []ProjectPart `json:"parts"`
	StartupOrder []string      `json:"startupOrder,omitempty"`
	Discovered   bool          `json:"discovered,omitempty"`
}
type ProjectImport struct {
	ID           string   `json:"id,omitempty"`
	Name         string   `json:"name"`
	Engine       string   `json:"engine"`
	YAML         string   `json:"yaml"`
	Files        []File   `json:"files,omitempty"`
	StartupOrder []string `json:"startupOrder,omitempty"`
	Replace      bool     `json:"replace,omitempty"`
}
type File struct {
	Path    string `json:"path"`
	Content []byte `json:"content"`
	Mode    uint32 `json:"mode,omitempty"`
}
type ProjectParams struct {
	ID            string   `json:"id"`
	Action        string   `json:"action,omitempty"`
	Engine        string   `json:"engine,omitempty"`
	YAML          string   `json:"yaml,omitempty"`
	RemoveVolumes bool     `json:"removeVolumes,omitempty"`
	Services      []string `json:"services,omitempty"`
}

// ComposeParams is a trusted controller-to-engine request after project lookup.
type ComposeParams struct {
	Project       ProjectPart `json:"project"`
	Action        string      `json:"action,omitempty"`
	YAML          string      `json:"yaml,omitempty"`
	Files         []File      `json:"files,omitempty"`
	RemoveVolumes bool        `json:"removeVolumes,omitempty"`
	Replace       bool        `json:"replace,omitempty"`
	Services      []string    `json:"services,omitempty"`
}
type PartResult struct {
	Engine string `json:"engine"`
	Result any    `json:"result,omitempty"`
	Error  string `json:"error,omitempty"`
}
type OperationResult struct {
	ProjectID string       `json:"projectID"`
	Success   bool         `json:"success"`
	Parts     []PartResult `json:"parts"`
}
type LogResult struct {
	Text string `json:"text"`
}
type ProjectFile struct {
	Engine string `json:"engine"`
	YAML   string `json:"yaml"`
}

type WSLSettings struct {
	Distro           string `json:"distro"`
	ServiceAccount   string `json:"serviceAccount"`
	CPUs             int    `json:"cpus"`
	MemoryMB         int64  `json:"memoryMB"`
	DiskGB           int64  `json:"diskGB"`
	ProfileDirectory string `json:"profileDirectory"`
	InstallDirectory string `json:"installDirectory"`
}
type Settings struct {
	Version         int                   `json:"version"`
	DataRoot        string                `json:"dataRoot"`
	WindowsEndpoint string                `json:"windowsEndpoint"`
	HelperURL       string                `json:"helperURL"`
	CACert          string                `json:"caCert"`
	ClientCert      string                `json:"clientCert"`
	ClientKey       string                `json:"clientKey"`
	WSL             WSLSettings           `json:"wsl"`
	Projects        []Project             `json:"projects"`
	DockerProxy     ProxySettings         `json:"dockerProxy"`
	ContainersProxy ProxySettings         `json:"containersProxy"`
	Network         NetworkSettings       `json:"network"`
	ResourceSaver   ResourceSaverSettings `json:"resourceSaver"`
	RegistryMirrors []string              `json:"registryMirrors,omitempty"`
}
type Event struct {
	Sequence uint64    `json:"sequence"`
	Time     time.Time `json:"time"`
	Engine   string    `json:"engine,omitempty"`
	Project  string    `json:"project,omitempty"`
	Type     string    `json:"type"`
	Message  string    `json:"message"`
}
type EventsParams struct {
	After uint64 `json:"after"`
	Epoch string `json:"epoch,omitempty"`
}
type EventBatch struct {
	Epoch  string  `json:"epoch"`
	Events []Event `json:"events"`
}

// EventBus keeps a bounded recent history for clients and operation progress.
type EventBus struct {
	mu      sync.Mutex
	seq     uint64
	history []Event
	epoch   string
}

// Batch resets the cursor when a helper process restarts.
func (b *EventBus) Batch(after uint64, epoch string) EventBatch {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.epoch == "" {
		b.epoch = rand.Text()
	}
	if epoch != b.epoch {
		after = 0
	}
	out := EventBatch{Epoch: b.epoch, Events: []Event{}}
	for _, e := range b.history {
		if e.Sequence > after {
			out.Events = append(out.Events, e)
		}
	}
	return out
}

func (b *EventBus) Publish(e Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.seq++
	e.Sequence = b.seq
	e.Time = time.Now().UTC()
	b.history = append(b.history, e)
	if len(b.history) > 500 {
		b.history = append([]Event(nil), b.history[len(b.history)-500:]...)
	}
}
func (b *EventBus) Since(after uint64) []Event {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := []Event{}
	for _, e := range b.history {
		if e.Sequence > after {
			out = append(out, e)
		}
	}
	return out
}
func ValidEngine(s string) bool { return s == "windows" || s == "linux" }
func ValidName(s string) bool {
	if len(s) == 0 || len(s) > 63 || s[0] < 'a' || s[0] > 'z' {
		return false
	}
	return strings.IndexFunc(s, func(r rune) bool { return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') }) < 0
}
