package engine

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/compose-spec/compose-go/v2/types"
	clitypes "github.com/docker/cli/cli/config/types"
	composeapi "github.com/docker/compose/v5/pkg/api"
	"github.com/docker/compose/v5/pkg/compose"
	"github.com/skyri/ucd/internal/api"
)

func TestComposeProgressReportsActualOutcome(t *testing.T) {
	bus := &api.EventBus{}
	processor := progress{engine: "linux", project: "demo-linux", events: bus}
	if err := compose.Run(context.Background(), func(context.Context) error { return nil }, "up", processor); err != nil {
		t.Fatal(err)
	}
	if err := compose.Run(context.Background(), func(context.Context) error { return errors.New("test failure") }, "build", processor); err == nil {
		t.Fatal("expected build failure")
	}
	events := bus.Since(0)
	if len(events) != 4 || events[1].Message != "up completed" || events[3].Message != "build failed" {
		t.Fatalf("unexpected operation outcomes: %#v", events)
	}
}

func TestComposeOutputIsScopedAndPreservesChunks(t *testing.T) {
	bus := &api.EventBus{}
	out := &composeOutput{engine: "linux", project: "sample-linux", events: bus}
	input := strings.Repeat("a", 4095) + "世界\nfinished"
	for _, chunk := range []string{input[:4096], input[4096:]} {
		if _, err := io.WriteString(out, chunk); err != nil {
			t.Fatal(err)
		}
	}
	out.flush()
	var got strings.Builder
	for _, event := range bus.Since(0) {
		if event.Type != "compose.output" || event.Engine != "linux" || event.Project != "sample-linux" || len(event.Message) > 4096 {
			t.Fatalf("unexpected output event: %#v", event)
		}
		got.WriteString(event.Message)
	}
	if got.String() != input {
		t.Fatalf("output differs: got %d bytes, want %d", got.Len(), len(input))
	}
	// A timer flush can occur between writes of a UTF-8 character.
	bus = &api.EventBus{}
	out = &composeOutput{engine: "linux", project: "sample-linux", events: bus}
	_, _ = out.Write([]byte("世")[:1])
	time.Sleep(200 * time.Millisecond)
	_, _ = out.Write([]byte("世")[1:])
	out.flush()
	got.Reset()
	for _, event := range bus.Since(0) {
		got.WriteString(event.Message)
	}
	if got.String() != "世" {
		t.Fatalf("timer split UTF-8: %q", got.String())
	}
}

func TestSelectComposeServices(t *testing.T) {
	project := &types.Project{Services: types.Services{
		"web":   {Name: "web", WorkloadSpec: types.WorkloadSpec{DependsOn: types.DependsOnConfig{"db": {Condition: "service_started"}}}},
		"db":    {Name: "db"},
		"other": {Name: "other"},
	}}
	selected, err := selectComposeServices(project, []string{"web"}, true)
	if err != nil || len(selected.Services) != 2 || selected.Services["db"].Name != "db" {
		t.Fatalf("selection with dependencies: %#v, %v", selected, err)
	}
	selected, err = selectComposeServices(project, []string{"web"}, false)
	if err != nil || len(selected.Services) != 1 || selected.Services["web"].Name != "web" {
		t.Fatalf("selection without dependencies: %#v, %v", selected, err)
	}
	if _, err := selectComposeServices(project, []string{"missing"}, false); err == nil {
		t.Fatal("unknown service accepted")
	}
}

func TestComposeFactorySnapshotsConcurrentRegistryChanges(t *testing.T) {
	b := testBackend(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) })
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for range 4 {
		wg.Go(func() {
			for range 20 {
				if _, err := b.newCompose(context.Background(), io.Discard, "demo"); err != nil {
					errs <- err
					return
				}
			}
		})
	}
	for range 20 {
		b.configMu.Lock()
		b.registryConfig.AuthConfigs["registry.invalid"] = clitypes.AuthConfig{Username: "test-user"}
		delete(b.registryConfig.AuthConfigs, "registry.invalid")
		b.configMu.Unlock()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}

func TestComposeConfigSnapshotIncludesBundledWindowsPlugins(t *testing.T) {
	b := testBackend(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) })
	b.registryConfig.CLIPluginsExtraDirs = []string{"machine-extra"}
	b.id = "windows"
	snapshot, err := b.composeConfigSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(filepath.Dir(executable), "cli-plugins")
	if len(snapshot.CLIPluginsExtraDirs) != 2 || snapshot.CLIPluginsExtraDirs[0] != want || snapshot.CLIPluginsExtraDirs[1] != "machine-extra" {
		t.Fatalf("unexpected Windows plugin search paths: %#v", snapshot.CLIPluginsExtraDirs)
	}
	snapshot.CLIPluginsExtraDirs[1] = "changed"
	if b.registryConfig.CLIPluginsExtraDirs[0] != "machine-extra" {
		t.Fatal("operation snapshot mutated the machine configuration")
	}
	b.id = "linux"
	snapshot, err = b.composeConfigSnapshot(context.Background())
	if err != nil || len(snapshot.CLIPluginsExtraDirs) != 1 || snapshot.CLIPluginsExtraDirs[0] != "machine-extra" {
		t.Fatalf("unexpected Linux plugin search paths: %#v, %v", snapshot, err)
	}
}

type followingCompose struct {
	composeapi.Compose
	started chan composeapi.LogOptions
	stopped chan struct{}
}

func (c followingCompose) Logs(ctx context.Context, _ string, consumer composeapi.LogConsumer, options composeapi.LogOptions) error {
	c.started <- options
	consumer.Log("web-1", "first log line")
	<-ctx.Done()
	close(c.stopped)
	return nil
}

func TestComposeLogsSessionStreamsAndCloses(t *testing.T) {
	b := testBackend(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) })
	value, err := call(t, b, "compose.import", api.ComposeParams{Project: api.ProjectPart{Engine: "linux", Name: "stream-demo"}, YAML: "services:\n  web:\n    image: nginx\n  db:\n    image: postgres\n"})
	if err != nil {
		t.Fatal(err)
	}
	part := value.(api.ProjectPart)
	fake := followingCompose{Compose: b.compose, started: make(chan composeapi.LogOptions, 1), stopped: make(chan struct{})}
	b.compose = fake
	value, err = call(t, b, "compose.logs.open", api.ComposeParams{Project: part, Services: []string{"web"}})
	if err != nil {
		t.Fatal(err)
	}
	id := value.(api.ExecOpenResult).SessionID
	select {
	case opts := <-fake.started:
		if !opts.Follow || opts.Tail != "500" || len(opts.Services) != 1 || opts.Services[0] != "web" {
			t.Fatalf("unexpected log options: %#v", opts)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("log stream did not start")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		value, err = call(t, b, "compose.logs.read", api.ExecParams{SessionID: id})
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(value.(api.ExecReadResult).Data), "web-1 | first log line") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("log output not received")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := call(t, b, "compose.logs.close", api.ExecParams{SessionID: id}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-fake.stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("closing session did not stop log stream")
	}
}
