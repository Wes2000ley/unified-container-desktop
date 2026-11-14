package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/skyri/ucd/internal/api"
	"github.com/skyri/ucd/internal/state"
)

type callerFunc func(context.Context, api.Request) (any, error)

func (f callerFunc) Call(ctx context.Context, r api.Request) (any, error) { return f(ctx, r) }
func admin() context.Context {
	return api.WithPrincipal(context.Background(), api.Principal{SID: "test-admin", Role: "administrator"})
}
func request(t *testing.T, method, engine string, p any) api.Request {
	t.Helper()
	r, err := api.NewRequest(method, engine, p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func testStore(t *testing.T) *state.Store {
	t.Helper()
	s, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func saveProject(t *testing.T, s *state.Store) {
	t.Helper()
	err := s.Update(func(v *api.Settings) error {
		v.Projects = []api.Project{{ID: "test", Name: "Test app", Parts: []api.ProjectPart{{Engine: "windows", Name: "test-windows"}, {Engine: "linux", Name: "test-linux"}}}}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestMixedOperationRetainsEachEngineOutcome(t *testing.T) {
	s := testStore(t)
	saveProject(t, s)
	backend := func(engine string) api.Caller {
		return callerFunc(func(_ context.Context, r api.Request) (any, error) {
			switch r.Method {
			case "compose.ports":
				return []api.Port{}, nil
			case "container.list":
				return []api.Container{}, nil
			case "compose.action":
				if engine == "linux" {
					return nil, errors.New("daemon is unavailable")
				}
				return map[string]bool{"started": true}, nil
			}
			return nil, fmt.Errorf("unexpected %s", r.Method)
		})
	}
	c := New(s, backend("windows"), backend("linux"), nil, &api.EventBus{})
	v, err := c.Call(admin(), request(t, "project.action", "", api.ProjectParams{ID: "test", Action: "up"}))
	if err != nil {
		t.Fatal(err)
	}
	result := v.(api.OperationResult)
	if result.Success || len(result.Parts) != 2 || result.Parts[0].Error != "" || result.Parts[1].Error == "" {
		t.Fatalf("lost partial result: %#v", result)
	}
	if len(s.Snapshot().Projects) != 1 {
		t.Fatal("failed deployment lost project metadata")
	}
}

func TestOfflineEngineDoesNotHideAvailableInventory(t *testing.T) {
	windows := callerFunc(func(context.Context, api.Request) (any, error) {
		return []api.Container{{ID: "running", Engine: "windows"}}, nil
	})
	linux := callerFunc(func(context.Context, api.Request) (any, error) { return nil, errors.New("connection refused") })
	c := New(testStore(t), windows, linux, nil, &api.EventBus{})
	v, err := c.Call(admin(), api.Request{Method: "container.list"})
	if err != nil {
		t.Fatal(err)
	}
	result := v.(Inventory)
	if len(result.Items) != 1 || len(result.Errors) != 1 || result.Errors[0].Engine != "linux" {
		t.Fatalf("bad inventory: %#v", result)
	}
}

func TestAuthorizationPrecedesAnyBackendAction(t *testing.T) {
	called := false
	b := callerFunc(func(context.Context, api.Request) (any, error) { called = true; return nil, nil })
	c := New(testStore(t), b, b, b, &api.EventBus{})
	for _, principal := range []api.Principal{{}, {SID: "viewer", Role: "viewer"}, {SID: "operator", Role: "operator"}} {
		_, err := c.Call(api.WithPrincipal(context.Background(), principal), request(t, "operator.add", "", map[string]string{"account": "someone"}))
		if err == nil {
			t.Fatalf("unauthorized principal accepted: %#v", principal)
		}
	}
	if called {
		t.Fatal("unauthorized caller reached backend")
	}
}

func TestRegistryOperationsUseTheWindowsMachineStore(t *testing.T) {
	methods := []string{"registry.list", "registry.logout", "registry.credential.get", "registry.credential.store", "registry.credential.erase", "registry.credential.list"}
	for _, method := range methods {
		t.Run(method, func(t *testing.T) {
			windows := callerFunc(func(_ context.Context, got api.Request) (any, error) {
				if got.Method != method || got.Engine != "windows" {
					t.Fatalf("machine registry request = %#v", got)
				}
				return "machine-store", nil
			})
			linux := callerFunc(func(context.Context, api.Request) (any, error) {
				t.Fatal("registry operation reached Linux-specific storage")
				return nil, nil
			})
			c := New(testStore(t), windows, linux, nil, &api.EventBus{})
			got, err := c.Call(api.WithPrincipal(context.Background(), api.Principal{SID: "operator", Role: "operator"}), api.Request{Method: method, Engine: "linux"})
			if err != nil || got != "machine-store" {
				t.Fatalf("registry result=%v err=%v", got, err)
			}
		})
	}
}

func TestRegistryLoginValidatesOnLinuxWhenWindowsEngineIsOffline(t *testing.T) {
	const secret = "synthetic-login-secret"
	windows := callerFunc(func(_ context.Context, got api.Request) (any, error) {
		switch got.Method {
		case "registry.internal.validate":
			return nil, errors.New("Windows daemon is offline")
		case "registry.internal.store":
			var stored api.RegistryAuthParams
			if err := json.Unmarshal(got.Params, &stored); err != nil {
				t.Fatal(err)
			}
			if stored.Server != "registry.example" || stored.Auth.IdentityToken != secret || stored.Auth.Password != "" {
				t.Fatalf("stored credentials = %#v", stored)
			}
			return nil, nil
		default:
			t.Fatalf("unexpected Windows request: %#v", got)
			return nil, nil
		}
	})
	linux := callerFunc(func(_ context.Context, got api.Request) (any, error) {
		if got.Method != "registry.internal.validate" || got.Engine != "linux" {
			t.Fatalf("unexpected Linux request: %#v", got)
		}
		return api.RegistryAuth{Server: "registry.example", IdentityToken: secret}, nil
	})
	c := New(testStore(t), windows, linux, nil, &api.EventBus{})
	result, err := c.Call(api.WithPrincipal(context.Background(), api.Principal{SID: "operator", Role: "operator"}), request(t, "registry.login", "", api.RegistryParams{Server: "registry.example", Username: "test-user", Password: "synthetic-password"}))
	if err != nil || result != nil {
		t.Fatalf("login result=%v err=%v", result, err)
	}
}

func TestLinuxImageOperationGetsAuthenticatedSharedRegistrySnapshot(t *testing.T) {
	const secret = "synthetic-controller-registry-secret"
	windows := callerFunc(func(_ context.Context, got api.Request) (any, error) {
		if got.Method != "registry.internal.snapshot" || got.Engine != "windows" {
			t.Fatalf("snapshot request = %#v", got)
		}
		return api.RegistrySnapshot{AuthConfigs: map[string]api.RegistryAuth{
			"registry.example": {Username: "test-user", Password: secret},
		}}, nil
	})
	linux := callerFunc(func(_ context.Context, got api.Request) (any, error) {
		if got.Method != "registry.internal.call" || got.Engine != "linux" {
			t.Fatalf("internal call = %#v", got)
		}
		var operation api.RegistryOperation
		if err := json.Unmarshal(got.Params, &operation); err != nil {
			t.Fatal(err)
		}
		if operation.Request.Method != "image.pull" || operation.Request.Engine != "linux" || operation.Snapshot.AuthConfigs["registry.example"].Password != secret {
			t.Fatalf("Linux call did not carry its operation-scoped credentials: %#v", operation)
		}
		return "pulled", nil
	})
	c := New(testStore(t), windows, linux, nil, &api.EventBus{})
	got, err := c.Call(admin(), request(t, "image.pull", "linux", api.ImageParams{Reference: "registry.example/app:latest"}))
	if err != nil || got != "pulled" {
		t.Fatalf("result=%v err=%v", got, err)
	}
}

func TestLinuxStreamingImageBuildGetsSharedRegistrySnapshot(t *testing.T) {
	windows := callerFunc(func(_ context.Context, got api.Request) (any, error) {
		if got.Method != "registry.internal.snapshot" {
			t.Fatalf("snapshot request = %#v", got)
		}
		return api.RegistrySnapshot{AuthConfigs: map[string]api.RegistryAuth{}}, nil
	})
	linux := callerFunc(func(_ context.Context, got api.Request) (any, error) {
		if got.Method != "registry.internal.call" {
			t.Fatalf("internal request = %#v", got)
		}
		var operation api.RegistryOperation
		if err := json.Unmarshal(got.Params, &operation); err != nil {
			t.Fatal(err)
		}
		var transfer api.TransferOpenParams
		if err := json.Unmarshal(operation.Request.Params, &transfer); err != nil {
			t.Fatal(err)
		}
		if operation.Request.Method != "transfer.open" || transfer.Operation != "image.build" {
			t.Fatalf("stream build operation = %#v, %#v", operation.Request, transfer)
		}
		return "opened", nil
	})
	c := New(testStore(t), windows, linux, nil, &api.EventBus{})
	got, err := c.Call(admin(), request(t, "transfer.open", "linux", api.TransferOpenParams{Operation: "image.build", Dockerfile: "FROM scratch"}))
	if err != nil || got != "opened" {
		t.Fatalf("result=%v err=%v", got, err)
	}
}

func TestRegistryInternalCallsCannotBeRequestedByLocalClients(t *testing.T) {
	called := false
	backend := callerFunc(func(context.Context, api.Request) (any, error) { called = true; return nil, nil })
	c := New(testStore(t), backend, backend, nil, &api.EventBus{})
	for _, method := range []string{"registry.internal.call", "registry.internal.snapshot", "registry.internal.sync"} {
		if _, err := c.Call(admin(), api.Request{Method: method, Engine: "windows"}); err == nil {
			t.Errorf("accepted %s", method)
		}
	}
	if called {
		t.Fatal("internal registry request reached an engine")
	}
}

func TestPortConflictStopsDeployment(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	_, portText, _ := net.SplitHostPort(listener.Addr().String())
	port, _ := strconv.Atoi(portText)
	s := testStore(t)
	saveProject(t, s)
	deployed := false
	b := callerFunc(func(_ context.Context, r api.Request) (any, error) {
		switch r.Method {
		case "compose.ports":
			return []api.Port{{HostIP: "127.0.0.1", Published: uint16(port), Target: 80, Protocol: "tcp"}}, nil
		case "container.list":
			return []api.Container{}, nil
		case "compose.action":
			deployed = true
			return nil, nil
		}
		return nil, errors.New("unexpected request")
	})
	c := New(s, b, b, nil, &api.EventBus{})
	v, err := c.Call(admin(), request(t, "project.action", "windows", api.ProjectParams{ID: "test", Action: "up"}))
	if err != nil {
		t.Fatal(err)
	}
	result := v.(api.OperationResult)
	if result.Success || deployed || result.Parts[0].Error == "" {
		t.Fatalf("deployment ignored port conflict: %#v", result)
	}
}

func TestProjectRemovePreservesMetadataOnPartialFailure(t *testing.T) {
	s := testStore(t)
	saveProject(t, s)
	ok := callerFunc(func(context.Context, api.Request) (any, error) { return nil, nil })
	bad := callerFunc(func(context.Context, api.Request) (any, error) { return nil, errors.New("Linux unreachable") })
	c := New(s, ok, bad, nil, &api.EventBus{})
	v, err := c.Call(admin(), request(t, "project.action", "", api.ProjectParams{ID: "test", Action: "remove"}))
	if err != nil {
		t.Fatal(err)
	}
	if v.(api.OperationResult).Success || len(s.Snapshot().Projects) != 1 {
		t.Fatal("partial removal lost recoverable metadata")
	}
}

func TestAttachAndReplacePreserveLogicalApplication(t *testing.T) {
	s := testStore(t)
	b := callerFunc(func(_ context.Context, r api.Request) (any, error) {
		p, err := api.Decode[api.ComposeParams](r)
		if err != nil {
			return nil, err
		}
		return p.Project, nil
	})
	c := New(s, b, b, nil, &api.EventBus{})
	for _, engine := range []string{"windows", "linux"} {
		_, err := c.Call(admin(), request(t, "project.import", engine, api.ProjectImport{Name: "Shared App", Engine: engine, YAML: "services: {}"}))
		if err != nil {
			t.Fatal(err)
		}
	}
	projects := s.Snapshot().Projects
	if len(projects) != 1 || len(projects[0].Parts) != 2 {
		t.Fatalf("bad application: %#v", projects)
	}
	_, err := c.Call(admin(), request(t, "project.import", "linux", api.ProjectImport{Name: "Shared App", Engine: "linux", YAML: "services: {}"}))
	if err == nil {
		t.Fatal("duplicate import unexpectedly overwrote files")
	}
	_, err = c.Call(admin(), request(t, "project.import", "linux", api.ProjectImport{Name: "Shared App", Engine: "linux", YAML: "services: {}", Replace: true}))
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Snapshot().Projects[0].Parts) != 2 {
		t.Fatal("replace duplicated project part")
	}
}

func TestProjectOperationsAreSerialized(t *testing.T) {
	s := testStore(t)
	saveProject(t, s)
	var mu sync.Mutex
	active, maxActive := 0, 0
	b := callerFunc(func(context.Context, api.Request) (any, error) {
		mu.Lock()
		active++
		maxActive = max(active, maxActive)
		mu.Unlock()
		defer func() { mu.Lock(); active--; mu.Unlock() }()
		return nil, nil
	})
	c := New(s, b, b, nil, &api.EventBus{})
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make(chan error, 20)
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := c.Call(admin(), request(t, "project.action", "windows", api.ProjectParams{ID: "test", Action: "stop"}))
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if maxActive != 1 {
		t.Fatalf("concurrent operations: %d", maxActive)
	}
	if len(c.locks) != 0 {
		t.Fatal("completed operations retained lock entries")
	}
}

func TestEngineSettingsRollbackAndReconcile(t *testing.T) {
	s := testStore(t)
	current := s.Snapshot()
	type engineSettings struct {
		DockerProxy     api.ProxySettings `json:"dockerProxy"`
		RegistryMirrors []string          `json:"registryMirrors"`
	}
	var windowsCalls, linuxCalls []engineSettings
	var windowsApplied engineSettings
	failLinux := true
	windows := callerFunc(func(_ context.Context, r api.Request) (any, error) {
		if r.Method != "engine.configure" {
			return nil, fmt.Errorf("unexpected Windows request %s", r.Method)
		}
		p, err := api.Decode[engineSettings](r)
		if err != nil {
			return nil, err
		}
		windowsCalls = append(windowsCalls, p)
		windowsApplied = p
		return nil, nil
	})
	linux := callerFunc(func(_ context.Context, r api.Request) (any, error) {
		if r.Method != "engine.configure" {
			return nil, fmt.Errorf("unexpected Linux request %s", r.Method)
		}
		p, err := api.Decode[engineSettings](r)
		if err != nil {
			return nil, err
		}
		linuxCalls = append(linuxCalls, p)
		if failLinux {
			return nil, errors.New("helper unavailable")
		}
		return nil, nil
	})
	c := New(s, windows, linux, nil, &api.EventBus{})
	next := current
	next.DockerProxy.HTTPS = "https://proxy.example:8443"
	next.RegistryMirrors = []string{"https://mirror.example"}
	_, err := c.Call(admin(), request(t, "system.configure", "", next))
	if err == nil || !strings.Contains(err.Error(), "helper unavailable") {
		t.Fatalf("expected underlying Linux error, got %v", err)
	}
	if len(windowsCalls) != 2 || windowsCalls[0].DockerProxy != next.DockerProxy || windowsCalls[1].DockerProxy != current.DockerProxy || !reflect.DeepEqual(windowsCalls[0].RegistryMirrors, next.RegistryMirrors) {
		t.Fatalf("Windows settings were not applied then restored: %#v", windowsCalls)
	}
	if windowsApplied.DockerProxy != current.DockerProxy || len(windowsApplied.RegistryMirrors) != 0 || s.Snapshot().DockerProxy != current.DockerProxy {
		t.Fatalf("failed configuration left changed state: applied=%#v stored=%#v", windowsApplied, s.Snapshot())
	}
	if len(linuxCalls) != 1 || linuxCalls[0].DockerProxy != next.DockerProxy {
		t.Fatalf("Linux did not receive the requested settings: %#v", linuxCalls)
	}

	// A later save of the original settings must still write both engines: a
	// prior rollback or response could have failed after the daemon changed.
	failLinux = false
	if _, err := c.Call(admin(), request(t, "system.configure", "", current)); err != nil {
		t.Fatal(err)
	}
	if len(windowsCalls) != 3 || len(linuxCalls) != 2 || windowsCalls[2].DockerProxy != current.DockerProxy || linuxCalls[1].DockerProxy != current.DockerProxy {
		t.Fatalf("saving original settings did not reconcile both engines: windows=%#v linux=%#v", windowsCalls, linuxCalls)
	}
}

type testWSLPlatform struct {
	settings api.WSLSettings
	calls    int
}

func (p *testWSLPlatform) Call(_ context.Context, r api.Request) (any, error) {
	p.calls++
	if r.Method != "wsl.configure" {
		return nil, fmt.Errorf("unexpected platform request %s", r.Method)
	}
	limits, err := api.Decode[api.WSLSettings](r)
	if err != nil {
		return nil, err
	}
	p.settings.CPUs, p.settings.MemoryMB, p.settings.DiskGB = limits.CPUs, limits.MemoryMB, limits.DiskGB
	return p.settings, nil
}

func (p *testWSLPlatform) WSLSettings() api.WSLSettings { return p.settings }

func TestInvalidWSLLimitsNeverReachPlatform(t *testing.T) {
	s := testStore(t)
	platform := &testWSLPlatform{settings: s.Snapshot().WSL}
	c := New(s, nil, nil, platform, &api.EventBus{})
	for _, limits := range []api.WSLSettings{
		{CPUs: 129, MemoryMB: 4096, DiskGB: 128},
		{CPUs: 4, MemoryMB: 511, DiskGB: 128},
		{CPUs: 4, MemoryMB: 4096, DiskGB: 8193},
	} {
		if _, err := c.Call(admin(), request(t, "wsl.configure", "", limits)); err == nil {
			t.Fatalf("accepted invalid limits: %#v", limits)
		}
	}
	if platform.calls != 0 {
		t.Fatalf("invalid settings reached the platform %d times", platform.calls)
	}
}

func TestWSLMetadataFailureReportsAppliedRuntimeLimits(t *testing.T) {
	s := testStore(t)
	initial := s.Snapshot()
	platform := &testWSLPlatform{settings: initial.WSL}
	var engineCalls int
	engine := callerFunc(func(_ context.Context, r api.Request) (any, error) {
		if r.Method != "engine.configure" {
			return nil, fmt.Errorf("unexpected engine request %s", r.Method)
		}
		engineCalls++
		return nil, nil
	})
	c := New(s, engine, engine, platform, &api.EventBus{})
	// Blocking the temporary store's config directory forces a real save error
	// without changing host services, WSL, or permissions.
	if err := os.WriteFile(filepath.Join(initial.DataRoot, "config"), []byte("block directory creation"), 0600); err != nil {
		t.Fatal(err)
	}
	limits := api.WSLSettings{CPUs: 8, MemoryMB: 8192, DiskGB: 256}
	_, err := c.Call(admin(), request(t, "wsl.configure", "", limits))
	if err == nil || !strings.Contains(err.Error(), "applied") || !strings.Contains(err.Error(), "saving controller metadata failed") {
		t.Fatalf("expected actionable partial-application error, got %v", err)
	}
	if platform.calls != 1 || platform.settings.CPUs != limits.CPUs || s.Snapshot().WSL.CPUs != initial.WSL.CPUs {
		t.Fatalf("unexpected platform/stored settings after save failure: platform=%#v stored=%#v", platform.settings, s.Snapshot().WSL)
	}
	value, err := c.Call(admin(), api.Request{Method: "system.settings"})
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Settings api.Settings `json:"settings"`
	}
	if err := convert(value, &result); err != nil {
		t.Fatal(err)
	}
	if result.Settings.WSL != platform.settings {
		t.Fatalf("system.settings did not show applied WSL limits: %#v", result.Settings.WSL)
	}
	if err := os.Remove(filepath.Join(initial.DataRoot, "config")); err != nil {
		t.Fatal(err)
	}
	result.Settings.DockerProxy.HTTPS = "https://proxy.example"
	if _, err := c.Call(admin(), request(t, "system.configure", "", result.Settings)); err != nil {
		t.Fatalf("saving proxy after recovering metadata storage failed: %v", err)
	}
	if engineCalls != 2 || s.Snapshot().DockerProxy != result.Settings.DockerProxy {
		t.Fatalf("proxy was not reconciled after WSL metadata failure: engine calls=%d settings=%#v", engineCalls, s.Snapshot())
	}
}

func TestEventCursorsRecoverAfterHelperAndControllerRestarts(t *testing.T) {
	s := testStore(t)
	helperEvents := &api.EventBus{}
	for range 5 {
		helperEvents.Publish(api.Event{Type: "engine.event", Message: "before helper restart"})
	}
	helper := callerFunc(func(_ context.Context, r api.Request) (any, error) {
		if r.Method != "system.events" {
			return nil, fmt.Errorf("unexpected helper request %s", r.Method)
		}
		p, err := api.Decode[api.EventsParams](r)
		if err != nil {
			return nil, err
		}
		return helperEvents.Batch(p.After, p.Epoch), nil
	})
	controllerEvents := &api.EventBus{}
	c := New(s, nil, helper, nil, controllerEvents)
	poll := func(c *Controller, cursor api.EventsParams) api.EventBatch {
		t.Helper()
		value, err := c.Call(admin(), request(t, "system.events", "", cursor))
		if err != nil {
			t.Fatal(err)
		}
		return value.(api.EventBatch)
	}
	first := poll(c, api.EventsParams{})
	if first.Epoch == "" || len(first.Events) != 5 {
		t.Fatalf("unexpected initial event batch: %#v", first)
	}
	cursor := api.EventsParams{After: first.Events[len(first.Events)-1].Sequence, Epoch: first.Epoch}
	if repeated := poll(c, cursor); len(repeated.Events) != 0 {
		t.Fatalf("poll repeated helper events: %#v", repeated)
	}

	helperEvents = &api.EventBus{}
	helperEvents.Publish(api.Event{Type: "engine.event", Message: "after helper restart"})
	afterHelper := poll(c, cursor)
	if afterHelper.Epoch != first.Epoch || len(afterHelper.Events) != 1 || afterHelper.Events[0].Message != "after helper restart" {
		t.Fatalf("helper restart lost event: %#v", afterHelper)
	}
	cursor.After = afterHelper.Events[0].Sequence
	if repeated := poll(c, cursor); len(repeated.Events) != 0 {
		t.Fatalf("poll repeated helper restart event: %#v", repeated)
	}

	// An open client keeps its old cursor across a controller service restart.
	restarted := New(s, nil, helper, nil, &api.EventBus{})
	afterController := poll(restarted, cursor)
	if afterController.Epoch == first.Epoch || len(afterController.Events) != 1 || afterController.Events[0].Message != "after helper restart" {
		t.Fatalf("controller restart did not reset the client cursor: %#v", afterController)
	}
	newCursor := api.EventsParams{After: afterController.Events[0].Sequence, Epoch: afterController.Epoch}
	if repeated := poll(restarted, newCursor); len(repeated.Events) != 0 {
		t.Fatalf("poll repeated controller restart event: %#v", repeated)
	}
}

func TestImplicitProjectIDCollisionDoesNotAttachDifferentName(t *testing.T) {
	s := testStore(t)
	calls := 0
	b := callerFunc(func(_ context.Context, r api.Request) (any, error) {
		calls++
		p, err := api.Decode[api.ComposeParams](r)
		if err != nil {
			return nil, err
		}
		return p.Project, nil
	})
	c := New(s, b, b, nil, &api.EventBus{})
	if _, err := c.Call(admin(), request(t, "project.import", "windows", api.ProjectImport{Name: "Foo Bar", Engine: "windows", YAML: "services: {}"})); err != nil {
		t.Fatal(err)
	}
	_, err := c.Call(admin(), request(t, "project.import", "linux", api.ProjectImport{Name: "Foo-Bar", Engine: "linux", YAML: "services: {}"}))
	var apiErr *api.Error
	if !errors.As(err, &apiErr) || apiErr.Code != "already_exists" || !strings.Contains(apiErr.Message, "normalized ID") {
		t.Fatalf("expected normalized name collision error, got %v", err)
	}
	projects := s.Snapshot().Projects
	if calls != 1 || len(projects) != 1 || len(projects[0].Parts) != 1 || projects[0].Name != "Foo Bar" {
		t.Fatalf("collision modified existing application: calls=%d projects=%#v", calls, projects)
	}
	if _, err := c.Call(admin(), request(t, "project.import", "linux", api.ProjectImport{ID: "foo-bar-2", Name: "Foo-Bar", Engine: "linux", YAML: "services: {}"})); err != nil {
		t.Fatalf("explicit unique project ID should succeed: %v", err)
	}
}

func TestMixedPortsPreflightPreventsBothDeployments(t *testing.T) {
	for _, protocol := range []string{"tcp", "udp"} {
		for _, action := range []string{"up", "start", "restart"} {
			t.Run(protocol+"/"+action, func(t *testing.T) {
				// Reserve an available port only long enough to choose its number.
				var port uint16
				if protocol == "tcp" {
					listener, err := net.Listen("tcp", "127.0.0.1:0")
					if err != nil {
						t.Fatal(err)
					}
					port = uint16(listener.Addr().(*net.TCPAddr).Port)
					if err := listener.Close(); err != nil {
						t.Fatal(err)
					}
				} else {
					listener, err := net.ListenPacket("udp", "127.0.0.1:0")
					if err != nil {
						t.Fatal(err)
					}
					port = uint16(listener.LocalAddr().(*net.UDPAddr).Port)
					if err := listener.Close(); err != nil {
						t.Fatal(err)
					}
				}
				s := testStore(t)
				saveProject(t, s)
				deployed := []string{}
				backend := func(engine string) api.Caller {
					return callerFunc(func(_ context.Context, r api.Request) (any, error) {
						switch r.Method {
						case "compose.ports":
							return []api.Port{{HostIP: "127.0.0.1", Published: port, Protocol: protocol}}, nil
						case "container.list":
							return []api.Container{}, nil
						case "compose.action":
							deployed = append(deployed, engine)
							return nil, nil
						default:
							return nil, fmt.Errorf("unexpected %s", r.Method)
						}
					})
				}
				c := New(s, backend("windows"), backend("linux"), nil, &api.EventBus{})
				v, err := c.Call(admin(), request(t, "project.action", "", api.ProjectParams{ID: "test", Action: action}))
				if err != nil {
					t.Fatal(err)
				}
				result := v.(api.OperationResult)
				if result.Success || len(result.Parts) != 2 || len(deployed) != 0 {
					t.Fatalf("conflicting parts deployed: %#v, %#v", result, deployed)
				}
				for _, part := range result.Parts {
					if !strings.Contains(part.Error, "requested by both") {
						t.Fatalf("missing conflict for %s: %s", part.Engine, part.Error)
					}
				}
			})
		}
	}
}

func TestMixedPortPreflightKeepsAvailableEngineIndependent(t *testing.T) {
	s := testStore(t)
	saveProject(t, s)
	started := false
	windows := callerFunc(func(_ context.Context, r api.Request) (any, error) {
		if r.Method == "compose.ports" {
			return nil, errors.New("Windows daemon unavailable")
		}
		if r.Method == "container.list" {
			return nil, errors.New("Windows daemon unavailable")
		}
		if r.Method == "registry.internal.snapshot" {
			return api.RegistrySnapshot{AuthConfigs: map[string]api.RegistryAuth{}}, nil
		}
		t.Fatalf("unavailable engine received %s", r.Method)
		return nil, nil
	})
	linux := callerFunc(func(_ context.Context, r api.Request) (any, error) {
		switch r.Method {
		case "compose.ports":
			return []api.Port{}, nil
		case "container.list":
			return []api.Container{}, nil
		case "registry.internal.call":
			var operation api.RegistryOperation
			if err := json.Unmarshal(r.Params, &operation); err != nil {
				t.Fatal(err)
			}
			if operation.Request.Method != "compose.action" {
				t.Fatalf("unexpected wrapped operation: %#v", operation.Request)
			}
			started = true
			return nil, nil
		default:
			return nil, fmt.Errorf("unexpected %s", r.Method)
		}
	})
	c := New(s, windows, linux, nil, &api.EventBus{})
	v, err := c.Call(admin(), request(t, "project.action", "", api.ProjectParams{ID: "test", Action: "up"}))
	if err != nil {
		t.Fatal(err)
	}
	result := v.(api.OperationResult)
	if result.Success || !started || len(result.Parts) != 2 || !strings.Contains(result.Parts[0].Error, "Windows daemon unavailable") || result.Parts[1].Error != "" {
		t.Fatalf("available engine did not start independently: %#v", result)
	}
}
