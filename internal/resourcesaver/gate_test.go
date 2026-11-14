package resourcesaver

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/skyri/ucd/internal/api"
)

type callerFunc func(context.Context, api.Request) (any, error)

func (f callerFunc) Call(ctx context.Context, request api.Request) (any, error) {
	return f(ctx, request)
}

type fakeRuntime struct {
	mu      sync.Mutex
	paused  bool
	changes []bool
	err     error
}

func (f *fakeRuntime) SetLinuxDockerPaused(_ context.Context, paused bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.paused = paused
	f.changes = append(f.changes, paused)
	return nil
}

func (f *fakeRuntime) state() (bool, []bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.paused, append([]bool(nil), f.changes...)
}

func newGate(t *testing.T, runtime Runtime, backend api.Caller) *Gate {
	t.Helper()
	g, err := New(Config{Enabled: true, IdleSeconds: 31}, backend, runtime, nil)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func TestTickSuspendsOnlyAfterIdleAndServesCachedInventory(t *testing.T) {
	runtime := &fakeRuntime{}
	calls := 0
	containerID := "initial"
	backend := callerFunc(func(_ context.Context, request api.Request) (any, error) {
		calls++
		switch request.Method {
		case "engine.status":
			return api.EngineStatus{ID: "linux", Online: true}, nil
		case "container.list":
			var params struct {
				Metrics bool `json:"metrics"`
			}
			paramsJSON := request.Params
			if len(paramsJSON) == 0 {
				paramsJSON = []byte(`{}`)
			}
			if err := json.Unmarshal(paramsJSON, &params); err != nil {
				return nil, err
			}
			return []api.Container{{ID: containerID, State: "exited", MetricsAvailable: params.Metrics}}, nil
		default:
			return nil, nil
		}
	})
	g := newGate(t, runtime, backend)
	ctx := context.Background()
	if _, err := g.Call(ctx, api.Request{Method: "container.list", Engine: "linux"}); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := g.tickAt(ctx, start); err != nil {
		t.Fatal(err)
	}
	if err := g.tickAt(ctx, start.Add(30*time.Second)); err != nil {
		t.Fatal(err)
	}
	if paused, _ := runtime.state(); paused {
		t.Fatal("paused before the configured threshold")
	}
	containerID = "fresh-at-idle-transition"
	if err := g.tickAt(ctx, start.Add(31*time.Second)); err != nil {
		t.Fatal(err)
	}
	if paused, changes := runtime.state(); !paused || len(changes) != 1 || !changes[0] {
		t.Fatalf("unexpected runtime state: paused=%t changes=%v", paused, changes)
	}

	before := calls
	value, err := g.Call(ctx, api.Request{Method: "container.list", Engine: "linux"})
	if err != nil {
		t.Fatal(err)
	}
	var cached []api.Container
	if err := json.Unmarshal(value.(json.RawMessage), &cached); err != nil {
		t.Fatal(err)
	}
	if len(cached) != 1 || cached[0].ID != "fresh-at-idle-transition" || calls != before {
		t.Fatalf("inventory was not served from cache: %#v calls=%d before=%d", cached, calls, before)
	}
	metricsRequest := api.Request{Method: "container.list", Engine: "linux", Params: []byte(`{"metrics":true}`)}
	value, err = g.Call(ctx, metricsRequest)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(value.(json.RawMessage), &cached); err != nil {
		t.Fatal(err)
	}
	if len(cached) != 1 || cached[0].ID != "fresh-at-idle-transition" || !cached[0].MetricsAvailable || calls != before {
		t.Fatalf("dashboard metrics inventory was not freshly cached: %#v calls=%d before=%d", cached, calls, before)
	}
	value, err = g.Call(ctx, api.Request{Method: "engine.status", Engine: "linux"})
	if err != nil {
		t.Fatal(err)
	}
	var status map[string]any
	if err := json.Unmarshal(mustJSON(t, value), &status); err != nil {
		t.Fatal(err)
	}
	if status["resourceSaver"] != true || status["online"] != true || status["error"] != nil {
		t.Fatalf("sleeping status was not marked clearly: %#v", status)
	}
}

func TestCacheUsesTotal32MiBLRU(t *testing.T) {
	runtime := &fakeRuntime{}
	backend := callerFunc(func(context.Context, api.Request) (any, error) { return nil, nil })
	g := newGate(t, runtime, backend)
	largeA := make([]byte, maxCachedBytes/2)
	largeB := make([]byte, maxCachedBytes/2)
	g.mu.Lock()
	g.cacheBytesLocked("a", largeA)
	g.cacheBytesLocked("b", largeB)
	g.sequence++
	entry := g.cache["a"]
	entry.used = g.sequence
	g.cache["a"] = entry
	g.cacheBytesLocked("c", []byte("small"))
	_, hasA := g.cache["a"]
	_, hasB := g.cache["b"]
	_, hasC := g.cache["c"]
	used := g.cacheBytes
	g.mu.Unlock()
	if !hasA || hasB || !hasC || used > maxCachedBytes {
		t.Fatalf("cache did not enforce total LRU bound: a=%t b=%t c=%t bytes=%d", hasA, hasB, hasC, used)
	}
}

func TestConfigUsesResourceSaverSettingsRange(t *testing.T) {
	for _, tc := range []struct {
		seconds int
		valid   bool
	}{{30, false}, {31, true}, {3600, true}, {3601, false}} {
		err := (Config{Enabled: true, IdleSeconds: tc.seconds}).validate()
		if (err == nil) != tc.valid {
			t.Errorf("threshold %d seconds validity = %t, want %t (error %v)", tc.seconds, err == nil, tc.valid, err)
		}
	}
}

func TestAcquireDuringRefreshPreventsSleep(t *testing.T) {
	runtime := &fakeRuntime{}
	refreshStarted := make(chan struct{})
	finishRefresh := make(chan struct{})
	var once sync.Once
	backend := callerFunc(func(_ context.Context, request api.Request) (any, error) {
		if request.Method == "engine.status" {
			return map[string]any{"online": true}, nil
		}
		if request.Method == "image.list" {
			once.Do(func() { close(refreshStarted) })
			<-finishRefresh
		}
		return []any{}, nil
	})
	g := newGate(t, runtime, backend)
	ctx := context.Background()
	start := time.Now()
	if err := g.tickAt(ctx, start); err != nil {
		t.Fatal(err)
	}

	tickDone := make(chan error, 1)
	go func() { tickDone <- g.tickAt(ctx, start.Add(31*time.Second)) }()
	select {
	case <-refreshStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("inventory refresh did not start")
	}
	release, err := g.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	close(finishRefresh)
	select {
	case err := <-tickDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("resource-saver tick did not finish after refresh was released")
	}
	release()
	if paused, _ := runtime.state(); paused {
		t.Fatal("paused Docker while a new operation was acquiring the engine")
	}
}

func TestRequiredInventoriesMustFitBeforeSleep(t *testing.T) {
	runtime := &fakeRuntime{}
	backend := callerFunc(func(_ context.Context, request api.Request) (any, error) {
		if request.Method == "engine.status" {
			return map[string]any{"online": true}, nil
		}
		return strings.Repeat("x", 6<<20), nil
	})
	g := newGate(t, runtime, backend)
	ctx := context.Background()
	start := time.Now()
	if err := g.tickAt(ctx, start); err != nil {
		t.Fatal(err)
	}
	if err := g.tickAt(ctx, start.Add(31*time.Second)); err == nil {
		t.Fatal("entered saver mode without enough cache space for required inventories")
	}
	if paused, _ := runtime.state(); paused {
		t.Fatal("paused Docker when required inventories did not fit in the cache")
	}
}

func TestRunningOrBackendOperationResetsIdleTimer(t *testing.T) {
	runtime := &fakeRuntime{}
	running, active := 1, 0
	backend := callerFunc(func(_ context.Context, request api.Request) (any, error) {
		if request.Method == "engine.status" {
			return map[string]any{"online": true, "running": running, "activeOperations": active}, nil
		}
		return nil, nil
	})
	g := newGate(t, runtime, backend)
	g.mu.Lock()
	g.warmed = true
	g.mu.Unlock()
	ctx := context.Background()
	start := time.Now()
	if err := g.tickAt(ctx, start); err != nil {
		t.Fatal(err)
	}
	if err := g.tickAt(ctx, start.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if paused, _ := runtime.state(); paused {
		t.Fatal("paused while a container was running")
	}
	g.mu.Lock()
	warmed := g.warmed
	g.mu.Unlock()
	if warmed {
		t.Fatal("kept a warmed inventory after observing a running container")
	}
	running = 0
	if err := g.tickAt(ctx, start.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	g.mu.Lock()
	g.warmed = true
	g.mu.Unlock()
	active = 1
	if err := g.tickAt(ctx, start.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	g.mu.Lock()
	warmed = g.warmed
	g.mu.Unlock()
	if warmed {
		t.Fatal("kept a warmed inventory after observing an active backend operation")
	}
	active = 0
	if err := g.tickAt(ctx, start.Add(4*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := g.tickAt(ctx, start.Add(4*time.Minute+30*time.Second)); err != nil {
		t.Fatal(err)
	}
	if paused, _ := runtime.state(); paused {
		t.Fatal("idle timer was not reset by running or backend operations")
	}
}

func TestOperationWakesEngineAndStreamHoldsLease(t *testing.T) {
	runtime := &fakeRuntime{paused: true}
	backendCalls := 0
	backend := callerFunc(func(_ context.Context, request api.Request) (any, error) {
		backendCalls++
		if request.Method == "container.exec.open" {
			return api.ExecOpenResult{SessionID: "stream-1"}, nil
		}
		return nil, nil
	})
	g := newGate(t, runtime, backend)
	g.paused = true
	ctx := context.Background()
	if _, err := g.Call(ctx, api.Request{Method: "container.exec.open", Engine: "linux"}); err != nil {
		t.Fatal(err)
	}
	if paused, changes := runtime.state(); paused || len(changes) != 1 || changes[0] {
		t.Fatalf("operation did not wake Docker: paused=%t changes=%v", paused, changes)
	}
	start := time.Now()
	if err := g.tickAt(ctx, start.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if paused, _ := runtime.state(); paused {
		t.Fatal("active stream did not hold the Docker lease")
	}
	params, _ := json.Marshal(api.ExecParams{SessionID: "stream-1"})
	if _, err := g.Call(ctx, api.Request{Method: "container.exec.close", Engine: "linux", Params: params}); err != nil {
		t.Fatal(err)
	}
	g.mu.Lock()
	active := g.active
	g.mu.Unlock()
	if active != 0 || backendCalls != 3 {
		t.Fatalf("stream close did not release its lease: active=%d backend calls=%d", active, backendCalls)
	}
}

func TestResetAfterExplicitEngineStartInvalidatesSaverState(t *testing.T) {
	runtime := &fakeRuntime{paused: true}
	backendCalls := 0
	backend := callerFunc(func(_ context.Context, request api.Request) (any, error) {
		if request.Method == "engine.status" {
			backendCalls++
			return map[string]any{"online": true, "source": "backend"}, nil
		}
		return nil, nil
	})
	g := newGate(t, runtime, backend)
	g.paused = true
	g.cacheStatus(map[string]any{"online": true, "source": "stale"})
	g.mu.Lock()
	g.active = 1
	g.streams["exec-1"] = streamLease{lastActive: time.Now()}
	g.mu.Unlock()

	g.ResetAfterEngineControl()
	if got := g.Snapshot(); got.Sleeping {
		t.Fatal("explicit engine start left the saver marked asleep")
	}
	value, err := g.Call(context.Background(), api.Request{Method: "engine.status", Engine: "linux"})
	if err != nil {
		t.Fatal(err)
	}
	var status map[string]any
	if err := json.Unmarshal(mustJSON(t, value), &status); err != nil {
		t.Fatal(err)
	}
	if status["source"] != "backend" || status["resourceSaver"] != nil || backendCalls != 1 {
		t.Fatalf("status did not refresh after engine start: %#v, calls=%d", status, backendCalls)
	}
	g.mu.Lock()
	active, streams := g.active, len(g.streams)
	g.mu.Unlock()
	if active != 1 || streams != 1 {
		t.Fatalf("engine reset changed active leases: active=%d streams=%d", active, streams)
	}
}

func TestResetAfterExplicitEngineStopDoesNotAutoResume(t *testing.T) {
	runtime := &fakeRuntime{paused: true}
	backend := callerFunc(func(_ context.Context, request api.Request) (any, error) {
		if request.Method == "engine.status" {
			return map[string]any{"online": false}, nil
		}
		return nil, nil
	})
	g := newGate(t, runtime, backend)
	g.paused = true
	g.cacheStatus(map[string]any{"online": true, "source": "stale"})
	g.ResetAfterEngineControl()

	value, err := g.Call(context.Background(), api.Request{Method: "engine.status", Engine: "linux"})
	if err != nil {
		t.Fatal(err)
	}
	var status map[string]any
	if err := json.Unmarshal(mustJSON(t, value), &status); err != nil {
		t.Fatal(err)
	}
	if status["online"] != false || status["resourceSaver"] != nil {
		t.Fatalf("administratively stopped engine was reported as saver-asleep: %#v", status)
	}
	if _, err := g.Call(context.Background(), api.Request{Method: "container.start", Engine: "linux"}); err != nil {
		t.Fatal(err)
	}
	if paused, changes := runtime.state(); !paused || len(changes) != 0 {
		t.Fatalf("ordinary operation auto-resumed an administrative stop: paused=%t changes=%v", paused, changes)
	}
}

func TestConfigureDisablingWakesAndPauseErrorsStayVisible(t *testing.T) {
	runtime := &fakeRuntime{paused: true}
	backend := callerFunc(func(_ context.Context, request api.Request) (any, error) {
		if request.Method == "engine.status" {
			return map[string]any{"online": true}, nil
		}
		return nil, nil
	})
	g := newGate(t, runtime, backend)
	g.paused = true
	if err := g.Configure(context.Background(), Config{Enabled: false, IdleSeconds: 31}); err != nil {
		t.Fatal(err)
	}
	if paused, changes := runtime.state(); paused || len(changes) != 1 || changes[0] {
		t.Fatalf("disabling saver did not wake Docker: paused=%t changes=%v", paused, changes)
	}
	if got := g.Snapshot(); got.Enabled || got.Sleeping {
		t.Fatalf("unexpected snapshot: %+v", got)
	}

	boom := errors.New("systemd failed")
	runtime.err = boom
	if err := g.Configure(context.Background(), Config{Enabled: true, IdleSeconds: 31}); err != nil {
		t.Fatal(err)
	}
	if err := g.tickAt(context.Background(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := g.tickAt(context.Background(), time.Now().Add(time.Minute)); !errors.Is(err, boom) {
		t.Fatalf("pause failure did not preserve its cause: %v", err)
	}
	if got := g.Snapshot(); got.Sleeping {
		t.Fatal("marked the engine asleep after systemd failed")
	}
}

func TestAcquireHonorsCancellationAndSingleRelease(t *testing.T) {
	runtime := &fakeRuntime{}
	backend := callerFunc(func(context.Context, api.Request) (any, error) { return nil, nil })
	g := newGate(t, runtime, backend)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := g.Acquire(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled acquire did not return context error: %v", err)
	}
	release, err := g.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	release()
	release()
	g.mu.Lock()
	active := g.active
	g.mu.Unlock()
	if active != 0 {
		t.Fatalf("release was not idempotent: active=%d", active)
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
