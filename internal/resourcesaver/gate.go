// Package resourcesaver suspends the dedicated Linux Docker daemon after an
// idle period and wakes it for container work. The WSL distribution and helper
// remain available; only Docker's systemd service and socket are stopped.
package resourcesaver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/skyri/ucd/internal/api"
)

const (
	streamIdleExpiry = 12 * time.Minute
	maxCachedEntries = 16
	maxCachedBytes   = 32 << 20
)

// Config controls when the dedicated Linux Docker daemon is suspended.
type Config struct {
	Enabled     bool
	IdleSeconds int
}

func (c Config) validate() error {
	return api.ValidateResourceSaverSettings(api.ResourceSaverSettings{Enabled: c.Enabled, IdleSeconds: c.IdleSeconds})
}

// Runtime provides a non-gated container count and controls only the owned
// Linux Docker service and socket.
type Runtime interface {
	SetLinuxDockerPaused(context.Context, bool) error
}

// Snapshot reports resource-saver state for the settings page and diagnostics.
type Snapshot struct {
	Enabled        bool      `json:"enabled"`
	IdleSeconds    int       `json:"idleSeconds"`
	Sleeping       bool      `json:"sleeping"`
	LastTransition time.Time `json:"lastTransition,omitempty"`
}

type cachedResponse struct {
	data []byte
	used uint64
}

type streamLease struct {
	lastActive time.Time
}

// Gate wraps the Linux helper caller. Read-only inventories are returned from
// a bounded cache while the daemon sleeps; operations and live streams wake or
// keep the daemon active. Tick must be called periodically by the controller.
type Gate struct {
	mu         sync.Mutex
	config     Config
	runtime    Runtime
	backend    api.Caller
	events     *api.EventBus
	paused     bool
	active     int
	idleSince  time.Time
	changedAt  time.Time
	streams    map[string]streamLease
	cache      map[string]cachedResponse
	cacheBytes int
	sequence   uint64
	generation uint64
	warmed     bool
	warming    bool
}

// New creates a Linux-engine gate. It does not start a goroutine; the service
// owns a ticker and calls Tick until shutdown.
func New(config Config, backend api.Caller, runtime Runtime, events *api.EventBus) (*Gate, error) {
	if err := config.validate(); err != nil {
		return nil, err
	}
	if backend == nil || runtime == nil {
		return nil, errors.New("resource saver requires a Linux helper caller and runtime")
	}
	return &Gate{
		config:  config,
		runtime: runtime,
		backend: backend,
		events:  events,
		streams: make(map[string]streamLease),
		cache:   make(map[string]cachedResponse),
	}, nil
}

// Snapshot returns the current state without contacting WSL or Docker.
func (g *Gate) Snapshot() Snapshot {
	g.mu.Lock()
	defer g.mu.Unlock()
	return Snapshot{Enabled: g.config.Enabled, IdleSeconds: g.config.IdleSeconds, Sleeping: g.paused, LastTransition: g.changedAt}
}

// ResetAfterEngineControl drops saver state after an explicit Linux engine
// start or stop. Administrative stops remain stopped until explicitly started.
func (g *Gate) ResetAfterEngineControl() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.paused = false
	g.idleSince = time.Time{}
	g.clearCacheLocked()
	g.warmed = false
	g.generation++
}

// Configure applies a saved setting. Disabling the feature wakes the daemon
// first, so a failure is visible and the setting remains enabled.
func (g *Gate) Configure(ctx context.Context, config Config) error {
	if ctx == nil {
		return errors.New("resource saver configuration requires a context")
	}
	if err := config.validate(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.paused && !config.Enabled {
		if err := g.runtime.SetLinuxDockerPaused(ctx, false); err != nil {
			return fmt.Errorf("resume Linux Docker before disabling resource saver: %w", err)
		}
		g.paused = false
		g.changedAt = time.Now()
		g.publish("linux.resource_saver.resumed", "Linux Docker Engine resumed")
	}
	g.config = config
	g.idleSince = time.Time{}
	g.warmed = false
	return nil
}

// Acquire reserves the Linux daemon for an operation, waking it when needed.
// Call the returned release function exactly once; it is safe to call repeatedly.
func (g *Gate) Acquire(ctx context.Context) (func(), error) {
	return g.acquire(ctx, true)
}

func (g *Gate) acquire(ctx context.Context, activity bool) (func(), error) {
	if ctx == nil {
		return nil, errors.New("resource saver operation requires a context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	g.mu.Lock()
	if err := ctx.Err(); err != nil {
		g.mu.Unlock()
		return nil, err
	}
	if g.paused {
		if err := g.runtime.SetLinuxDockerPaused(ctx, false); err != nil {
			g.mu.Unlock()
			return nil, fmt.Errorf("resume Linux Docker: %w", err)
		}
		g.paused = false
		g.changedAt = time.Now()
		g.publish("linux.resource_saver.resumed", "Linux Docker Engine resumed")
	}
	g.active++
	if activity {
		g.idleSince = time.Time{}
		g.clearCacheLocked()
		g.warmed = false
		g.generation++
	}
	g.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			defer g.mu.Unlock()
			g.active--
			if g.active < 0 {
				panic("resource saver active-operation count became negative")
			}
			if activity {
				g.idleSince = time.Now()
			}
		})
	}, nil
}

func (g *Gate) refreshInventories(ctx context.Context, config Config, generation uint64) error {
	g.mu.Lock()
	if g.warming || g.paused || g.generation != generation || !g.config.Enabled || g.config != config {
		g.mu.Unlock()
		return nil
	}
	g.warming = true
	g.warmed = false
	g.mu.Unlock()
	defer func() {
		g.mu.Lock()
		g.warming = false
		g.mu.Unlock()
	}()

	for _, request := range requiredInventories() {
		key := cacheKey(request)
		g.mu.Lock()
		if g.generation != generation || g.paused || !g.config.Enabled || g.config != config {
			g.mu.Unlock()
			return nil
		}
		g.mu.Unlock()

		result, err := g.backend.Call(ctx, request)
		if err != nil {
			return fmt.Errorf("cache Linux %s for resource saver: %w", request.Method, err)
		}
		data, err := json.Marshal(result)
		if err != nil {
			return fmt.Errorf("encode Linux %s inventory for resource saver: %w", request.Method, err)
		}
		if len(data) > maxCachedBytes {
			return fmt.Errorf("Linux %s inventory exceeds the %d MiB resource-saver cache limit", request.Method, maxCachedBytes>>20)
		}

		g.mu.Lock()
		if g.generation != generation || g.paused || !g.config.Enabled || g.config != config {
			g.mu.Unlock()
			return nil
		}
		g.cacheBytesLocked(key, data)
		g.mu.Unlock()
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	if g.generation != generation || g.paused || !g.config.Enabled || g.config != config {
		return nil
	}
	for _, request := range requiredInventories() {
		if _, exists := g.cache[cacheKey(request)]; !exists {
			return errors.New("required Linux inventories exceed the 32 MiB resource-saver cache limit")
		}
	}
	g.warmed = true
	return nil
}

func requiredInventories() []api.Request {
	return []api.Request{
		{Method: "container.list", Engine: "linux"},
		{Method: "container.list", Engine: "linux", Params: []byte(`{"metrics":true}`)},
		{Method: "image.list", Engine: "linux"},
		{Method: "volume.list", Engine: "linux"},
		{Method: "network.list", Engine: "linux"},
		{Method: "storage.df", Engine: "linux"},
	}
}

// Tick checks container activity and suspends the Linux Docker daemon when the
// configured idle period has elapsed. Probe errors reset the timer and prevent
// suspension because the running-container state is unknown.
func (g *Gate) Tick(ctx context.Context) error {
	return g.tickAt(ctx, time.Now())
}

func (g *Gate) tickAt(ctx context.Context, now time.Time) error {
	if ctx == nil {
		return errors.New("resource saver tick requires a context")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	g.mu.Lock()
	g.expireStreams(now)
	if !g.config.Enabled || g.paused || g.active != 0 || g.warming {
		g.mu.Unlock()
		return nil
	}
	config := g.config
	generation := g.generation
	g.mu.Unlock()

	running, active, online, statusValue, err := g.runningState(ctx)
	if err != nil {
		g.resetIdle(generation)
		return fmt.Errorf("check Linux running containers before resource saver: %w", err)
	}
	if running < 0 || active < 0 {
		return errors.New("Linux engine returned a negative activity count")
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	g.mu.Lock()
	if !g.config.Enabled || g.config != config || g.paused || g.active != 0 || g.warming || g.generation != generation {
		g.mu.Unlock()
		return nil
	}
	g.cacheStatusLocked(statusValue)
	if !online || running != 0 || active != 0 {
		g.idleSince = time.Time{}
		g.warmed = false
		g.mu.Unlock()
		return nil
	}
	if g.idleSince.IsZero() {
		g.idleSince = now
		g.mu.Unlock()
		return nil
	}
	if now.Sub(g.idleSince) < time.Duration(config.IdleSeconds)*time.Second {
		g.mu.Unlock()
		return nil
	}
	g.mu.Unlock()

	if err := g.refreshInventories(ctx, config, generation); err != nil {
		g.resetIdle(generation)
		return err
	}
	g.mu.Lock()
	ready := g.config.Enabled && g.config == config && !g.paused && g.active == 0 && !g.warming && g.generation == generation
	g.mu.Unlock()
	if !ready {
		return nil
	}
	running, active, online, statusValue, err = g.runningState(ctx)
	if err != nil {
		g.resetIdle(generation)
		return fmt.Errorf("recheck Linux activity before resource saver: %w", err)
	}
	if running < 0 || active < 0 {
		g.resetIdle(generation)
		return errors.New("Linux engine returned a negative activity count")
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.config.Enabled || g.config != config || g.paused || g.active != 0 || g.warming || g.generation != generation {
		return nil
	}
	g.cacheStatusLocked(statusValue)
	if !online || running != 0 || active != 0 {
		g.idleSince = time.Time{}
		g.warmed = false
		return nil
	}
	if now.Sub(g.idleSince) < time.Duration(config.IdleSeconds)*time.Second {
		return nil
	}
	if !g.warmed || !g.requiredSnapshotsCachedLocked() {
		g.idleSince = time.Time{}
		g.warmed = false
		return errors.New("required Linux inventories or engine status are not available in the resource-saver cache")
	}
	if err := g.runtime.SetLinuxDockerPaused(ctx, true); err != nil {
		return fmt.Errorf("pause idle Linux Docker Engine: %w", err)
	}
	g.paused = true
	g.changedAt = now
	g.publish("linux.resource_saver.sleeping", "Linux Docker Engine entered resource saver mode")
	return nil
}

func (g *Gate) resetIdle(generation uint64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.paused && g.generation == generation {
		g.idleSince = time.Time{}
		g.warmed = false
	}
}

func (g *Gate) runningState(ctx context.Context) (int, int, bool, any, error) {
	value, err := g.backend.Call(ctx, api.Request{Method: "engine.status", Engine: "linux"})
	if err != nil {
		return 0, 0, false, nil, err
	}
	data, err := json.Marshal(value)
	if err != nil {
		return 0, 0, false, nil, fmt.Errorf("encode Linux engine activity: %w", err)
	}
	var status struct {
		Online           bool `json:"online"`
		Running          int  `json:"running"`
		ActiveOperations int  `json:"activeOperations"`
	}
	if err := json.Unmarshal(data, &status); err != nil {
		return 0, 0, false, nil, fmt.Errorf("decode Linux engine activity: %w", err)
	}
	return status.Running, status.ActiveOperations, status.Online, value, nil
}

// Call forwards Linux engine operations through the gate. While sleeping it
// serves recent inventory snapshots and gives an explicit error for uncached
// reads instead of waking Docker for GUI polling.
func (g *Gate) Call(ctx context.Context, request api.Request) (any, error) {
	if ctx == nil {
		return nil, errors.New("Linux engine call requires a context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if isCachedRead(request.Method) {
		key := cacheKey(request)
		g.mu.Lock()
		if g.paused {
			cached, ok := g.cache[key]
			if ok {
				g.sequence++
				cached.used = g.sequence
				g.cache[key] = cached
			}
			g.mu.Unlock()
			if !ok {
				return nil, api.Fail("unavailable", "Linux engine is in resource saver mode and this inventory has no cached result")
			}
			if request.Method == "engine.status" {
				return asleepStatus(cached.data)
			}
			return json.RawMessage(cached.data), nil
		}
		g.active++
		g.mu.Unlock()
		defer g.release(false)
	}

	var release func()
	var err error
	if !isCachedRead(request.Method) && request.Method != "system.events" {
		release, err = g.Acquire(ctx)
		if err != nil {
			return nil, err
		}
		defer func() {
			if release != nil {
				release()
			}
		}()
	}

	if session := sessionID(request); session != "" {
		g.touchStream(session)
	}
	result, callErr := g.backend.Call(ctx, request)
	if callErr != nil {
		return nil, callErr
	}
	if isCachedRead(request.Method) {
		g.cacheResult(request, result)
	}
	if request.Method == "engine.status" {
		g.cacheStatus(result)
	}
	if isStreamOpen(request.Method) {
		if id := resultSessionID(result); id != "" {
			g.keepStream(id)
			release = nil
		}
	}
	if isStreamClose(request.Method) {
		if id := sessionID(request); id != "" {
			g.releaseStream(id)
		}
	}
	return result, nil
}

func (g *Gate) release(activity bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.active--
	if g.active < 0 {
		panic("resource saver active-operation count became negative")
	}
	if activity {
		g.idleSince = time.Now()
	}
}

func (g *Gate) cacheResult(request api.Request, result any) {
	data, err := json.Marshal(result)
	if err != nil || len(data) > maxCachedBytes {
		return
	}
	key := cacheKey(request)
	g.mu.Lock()
	defer g.mu.Unlock()
	g.cacheBytesLocked(key, data)
}

func (g *Gate) cacheStatusLocked(result any) {
	data, err := json.Marshal(result)
	if err != nil || len(data) > maxCachedBytes {
		return
	}
	g.cacheBytesLocked(cacheKey(api.Request{Method: "engine.status", Engine: "linux"}), data)
}

func (g *Gate) cacheBytesLocked(key string, data []byte) {
	if len(data) > maxCachedBytes {
		return
	}
	if previous, exists := g.cache[key]; exists {
		g.cacheBytes -= len(previous.data)
	}
	g.sequence++
	g.cache[key] = cachedResponse{data: data, used: g.sequence}
	g.cacheBytes += len(data)
	for g.cacheBytes > maxCachedBytes || len(g.cache) > maxCachedEntries {
		oldestKey := ""
		var oldest uint64
		for candidate, entry := range g.cache {
			if oldestKey == "" || entry.used < oldest {
				oldestKey, oldest = candidate, entry.used
			}
		}
		g.cacheBytes -= len(g.cache[oldestKey].data)
		delete(g.cache, oldestKey)
	}
}

func (g *Gate) requiredSnapshotsCachedLocked() bool {
	status := api.Request{Method: "engine.status", Engine: "linux"}
	if _, exists := g.cache[cacheKey(status)]; !exists {
		return false
	}
	for _, request := range requiredInventories() {
		if _, exists := g.cache[cacheKey(request)]; !exists {
			return false
		}
	}
	return true
}

func (g *Gate) clearCacheLocked() {
	g.cache = make(map[string]cachedResponse)
	g.cacheBytes = 0
}

func (g *Gate) cacheStatus(result any) {
	request := api.Request{Method: "engine.status", Engine: "linux"}
	g.cacheResult(request, result)
}

func (g *Gate) keepStream(id string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.streams[id] = streamLease{lastActive: time.Now()}
}

func (g *Gate) touchStream(id string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if stream, exists := g.streams[id]; exists {
		stream.lastActive = time.Now()
		g.streams[id] = stream
	}
}

func (g *Gate) releaseStream(id string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, exists := g.streams[id]; exists {
		delete(g.streams, id)
		g.active--
		if g.active < 0 {
			panic("resource saver active-operation count became negative")
		}
		g.idleSince = time.Now()
	}
}

func (g *Gate) expireStreams(now time.Time) {
	for id, stream := range g.streams {
		if now.Sub(stream.lastActive) >= streamIdleExpiry {
			delete(g.streams, id)
			g.active--
			if g.active < 0 {
				panic("resource saver active-operation count became negative")
			}
			g.idleSince = now
		}
	}
}

func (g *Gate) publish(kind, message string) {
	if g.events != nil {
		g.events.Publish(api.Event{Engine: "linux", Type: kind, Message: message})
	}
}

func isCachedRead(method string) bool {
	switch method {
	case "engine.status", "container.list", "image.list", "volume.list", "network.list", "storage.df":
		return true
	default:
		return false
	}
}

func cacheKey(request api.Request) string {
	params := bytes.TrimSpace(request.Params)
	if len(params) == 0 {
		params = []byte("{}")
	}
	var compact bytes.Buffer
	if json.Compact(&compact, params) == nil {
		params = compact.Bytes()
	}
	return request.Method + "\x00" + request.Engine + "\x00" + string(params)
}

func asleepStatus(data []byte) (any, error) {
	var status map[string]any
	if err := json.Unmarshal(data, &status); err != nil {
		return nil, fmt.Errorf("decode cached Linux engine status: %w", err)
	}
	status["resourceSaver"] = true
	// The controller service remains available and will wake Docker on demand.
	status["online"] = true
	delete(status, "error")
	return status, nil
}

func isStreamOpen(method string) bool {
	switch method {
	case "container.exec.open", "container.logs.open", "compose.logs.open", "transfer.open":
		return true
	default:
		return false
	}
}

func isStreamClose(method string) bool {
	return strings.HasSuffix(method, ".close") || method == "transfer.finish"
}

func sessionID(request api.Request) string {
	if !isSessionMethod(request.Method) {
		return ""
	}
	var value struct {
		SessionID string `json:"sessionID"`
	}
	if json.Unmarshal(request.Params, &value) != nil {
		return ""
	}
	return value.SessionID
}

func isSessionMethod(method string) bool {
	return strings.HasSuffix(method, ".read") || strings.HasSuffix(method, ".write") ||
		strings.HasSuffix(method, ".close") || method == "transfer.finish"
}

func resultSessionID(result any) string {
	data, err := json.Marshal(result)
	if err != nil {
		return ""
	}
	var value struct {
		SessionID string `json:"sessionID"`
	}
	if json.Unmarshal(data, &value) != nil {
		return ""
	}
	return value.SessionID
}
