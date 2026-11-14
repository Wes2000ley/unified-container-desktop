package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/skyri/ucd/internal/api"
)

type testCaller struct {
	requests []api.Request
	respond  func(api.Request) (any, error)
}

func (c *testCaller) Call(ctx context.Context, request api.Request) (any, error) {
	if _, ok := ctx.Deadline(); !ok {
		return nil, errors.New("missing call deadline")
	}
	c.requests = append(c.requests, request)
	if c.respond != nil {
		return c.respond(request)
	}
	return json.RawMessage(`{"ok":true}`), nil
}

func TestDispatch(t *testing.T) {
	cases := []struct {
		name, method, engine string
		args                 []string
		want                 map[string]any
	}{
		{"all containers", "container.list", "", []string{"container", "ls"}, map[string]any{}},
		{"remove container", "container.action", "linux", []string{"container", "rm", "--engine", "linux", "--id", "web", "--force"}, map[string]any{"id": "web", "action": "remove", "tail": float64(200), "force": true}},
		{"pull image", "image.pull", "windows", []string{"image", "pull", "--engine", "windows", "--reference", "example:latest"}, map[string]any{"reference": "example:latest"}},
		{"remove volume", "volume.remove", "linux", []string{"volume", "rm", "--engine", "linux", "--name", "data"}, map[string]any{"name": "data"}},
		{"WSL resources", "wsl.configure", "", []string{"wsl", "configure", "--cpus", "4", "--memory-mb", "8192", "--disk-gb", "60"}, map[string]any{"cpus": float64(4), "memoryMB": float64(8192), "diskGB": float64(60)}},
		{"operator", "operator.add", "", []string{"operator", "add", "--account", `DOMAIN\alice`}, map[string]any{"account": `DOMAIN\alice`}},
		{"share", "share.create", "", []string{"share", "create", "--name", "work", "--path", `C:\work`}, map[string]any{"name": "work", "windowsPath": `C:\work`}},
		{"events", "system.events", "", []string{"events", "--after", "7", "--epoch", "previous"}, map[string]any{"after": float64(7), "epoch": "previous"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := &testCaller{}
			var out, errOut bytes.Buffer
			if code := Run(context.Background(), client, tc.args, &out, &errOut); code != 0 {
				t.Fatalf("exit %d: %s", code, errOut.String())
			}
			if len(client.requests) != 1 {
				t.Fatalf("requests = %d", len(client.requests))
			}
			req := client.requests[0]
			if req.Method != tc.method || req.Engine != tc.engine {
				t.Fatalf("request = %#v", req)
			}
			var got map[string]any
			if err := json.Unmarshal(req.Params, &got); err != nil {
				t.Fatal(err)
			}
			for key, want := range tc.want {
				if got[key] != want {
					t.Errorf("params[%s] = %v, want %v", key, got[key], want)
				}
			}
			if !json.Valid(out.Bytes()) {
				t.Errorf("invalid JSON result %q", out.String())
			}
		})
	}
}

func TestEventsPreservesBatch(t *testing.T) {
	client := &testCaller{respond: func(api.Request) (any, error) {
		return json.RawMessage(`{"epoch":"current","events":[{"sequence":8,"type":"engine.started"}]}`), nil
	}}
	var out, errOut bytes.Buffer
	if code := Run(context.Background(), client, []string{"events", "--after", "7", "--epoch", "previous"}, &out, &errOut); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	var batch api.EventBatch
	if err := json.Unmarshal(out.Bytes(), &batch); err != nil {
		t.Fatal(err)
	}
	if batch.Epoch != "current" || len(batch.Events) != 1 || batch.Events[0].Sequence != 8 || batch.Events[0].Type != "engine.started" {
		t.Fatalf("batch: %#v", batch)
	}
}

func TestProjectPartialFailureIsPrintedAndReturnsFailure(t *testing.T) {
	client := &testCaller{respond: func(api.Request) (any, error) {
		return json.RawMessage(`{"projectID":"demo","success":false,"parts":[{"engine":"linux","error":"build failed"}]}`), nil
	}}
	var out, errOut bytes.Buffer
	code := Run(context.Background(), client, []string{"project", "up", "--id", "demo"}, &out, &errOut)
	if code == 0 || !strings.Contains(out.String(), "build failed") || !strings.Contains(errOut.String(), "project action failed") {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out.String(), errOut.String())
	}
}

func TestProjectRemoveUsesAction(t *testing.T) {
	client := &testCaller{respond: func(api.Request) (any, error) { return json.RawMessage(`{"success":true,"parts":[]}`), nil }}
	var out, errOut bytes.Buffer
	if code := Run(context.Background(), client, []string{"project", "remove", "--id", "demo", "--volumes"}, &out, &errOut); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	if client.requests[0].Method != "project.action" {
		t.Fatal(client.requests[0].Method)
	}
	var params api.ProjectParams
	if err := json.Unmarshal(client.requests[0].Params, &params); err != nil {
		t.Fatal(err)
	}
	if params.Action != "remove" || !params.RemoveVolumes {
		t.Fatalf("params: %#v", params)
	}
}

func TestCommandFailureDoesNotPrintSuccessfulJSON(t *testing.T) {
	client := &testCaller{respond: func(api.Request) (any, error) { return nil, api.Fail("permission_denied", "administrator required") }}
	var out, errOut bytes.Buffer
	if code := Run(context.Background(), client, []string{"engine", "start", "--engine", "linux"}, &out, &errOut); code == 0 {
		t.Fatal("expected failure")
	}
	if out.Len() != 0 || !strings.Contains(errOut.String(), "administrator required") || strings.Contains(errOut.String(), "installed and running") {
		t.Fatalf("stdout %q stderr %q", out.String(), errOut.String())
	}
}

func writeFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestProjectImportCopiesContext(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "compose.yaml"), "services:\n  web:\n    build: .\n")
	writeFile(t, filepath.Join(dir, "Dockerfile"), "FROM scratch\n")
	writeFile(t, filepath.Join(dir, ".env"), "VALUE=test\n")
	writeFile(t, filepath.Join(dir, "src", "script.sh"), "echo hello\n")
	p, err := projectImport(dir, "", "demo", "", "linux")
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "demo" || p.Engine != "linux" || !strings.Contains(p.YAML, "build: .") {
		t.Fatalf("import: %#v", p)
	}
	got := map[string]string{}
	for _, file := range p.Files {
		got[file.Path] = string(file.Content)
		info, err := os.Stat(filepath.Join(dir, filepath.FromSlash(file.Path)))
		if err != nil {
			t.Fatal(err)
		}
		if file.Mode != uint32(info.Mode().Perm()) {
			t.Errorf("file %s mode = %#o, want %#o", file.Path, file.Mode, info.Mode().Perm())
		}
	}
	if len(got) != 3 || got["Dockerfile"] != "FROM scratch\n" || got[".env"] != "VALUE=test\n" || got["src/script.sh"] != "echo hello\n" {
		t.Fatalf("files: %#v", got)
	}
}

func TestProjectImportPreservesExecutableMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not reliably represent POSIX execute bits")
	}
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "compose.yaml"), "services: {}\n")
	file := filepath.Join(dir, "script.sh")
	writeFile(t, file, "#!/bin/sh\n")
	if err := os.Chmod(file, 0755); err != nil {
		t.Fatal(err)
	}
	p, err := projectImport(dir, "", "demo", "", "linux")
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Files) != 1 || p.Files[0].Mode != 0755 {
		t.Fatalf("files: %#v", p.Files)
	}
}

func TestProjectImportRejectsComposeCollision(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "compose.yaml"), "services: {}\n")
	writeFile(t, filepath.Join(dir, "other.yml"), "services: {}\n")
	_, err := projectImport(dir, "other.yml", "demo", "", "linux")
	if err == nil || !strings.Contains(err.Error(), "already contains compose.yaml") {
		t.Fatalf("error: %v", err)
	}
}

func TestProjectImportRejectsReservedManifest(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "compose.yaml"), "services: {}\n")
	writeFile(t, filepath.Join(dir, ".ucd-import.json"), "{}")
	_, err := projectImport(dir, "", "demo", "", "linux")
	if err == nil || !strings.Contains(err.Error(), "reserved by the service") {
		t.Fatalf("error: %v", err)
	}
}

func TestProjectImportRejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "compose.yaml"), "services: {}\n")
	if err := os.Symlink(t.TempDir(), filepath.Join(dir, "link")); err != nil {
		t.Skipf("symlink creation unavailable: %v", err)
	}
	_, err := projectImport(dir, "", "demo", "", "linux")
	if err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("error: %v", err)
	}
}

func TestProjectImportRejectsOversizedContext(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "compose.yaml"), "services: {}\n")
	f, err := os.Create(filepath.Join(dir, "large"))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(maxContextBytes + 1); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = projectImport(dir, "", "demo", "", "linux")
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("error: %v", err)
	}
}

func TestInventoryPartialFailureReturnsFailure(t *testing.T) {
	client := &testCaller{respond: func(api.Request) (any, error) {
		return json.RawMessage(`{"items":[{"id":"one"}],"errors":[{"engine":"linux","message":"offline"}]}`), nil
	}}
	var out, errOut bytes.Buffer
	code := Run(context.Background(), client, []string{"image", "ls"}, &out, &errOut)
	if code == 0 || !strings.Contains(out.String(), "offline") || !strings.Contains(errOut.String(), "incomplete") {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out.String(), errOut.String())
	}
}

func TestComposeUpRefreshesExistingContext(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "compose.yaml")
	writeFile(t, file, "services:\n  web:\n    build: .\n")
	writeFile(t, filepath.Join(dir, "Dockerfile"), "FROM scratch\n")
	client := &testCaller{respond: func(request api.Request) (any, error) {
		switch request.Method {
		case "project.list":
			return []api.Project{{ID: "demo", Name: "demo", Parts: []api.ProjectPart{{Engine: "linux"}}}}, nil
		case "project.import":
			var p api.ProjectImport
			if err := json.Unmarshal(request.Params, &p); err != nil {
				t.Fatal(err)
			}
			if p.ID != "demo" || !p.Replace || len(p.Files) != 1 || p.Files[0].Path != "Dockerfile" {
				t.Fatalf("import: %#v", p)
			}
			return api.Project{ID: "demo"}, nil
		case "project.action":
			var p api.ProjectParams
			if err := json.Unmarshal(request.Params, &p); err != nil {
				t.Fatal(err)
			}
			if p.ID != "demo" || p.Engine != "linux" || p.Action != "up" {
				t.Fatalf("action: %#v", p)
			}
			return json.RawMessage(`{"projectID":"demo","success":true,"parts":[]}`), nil
		default:
			t.Fatalf("unexpected request: %s", request.Method)
			return nil, nil
		}
	}}
	var out, errOut bytes.Buffer
	code := Run(context.Background(), client, []string{"compose", "--engine", "linux", "-f", file, "-p", "demo", "up", "-d"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	if len(client.requests) != 3 {
		t.Fatalf("requests: %#v", client.requests)
	}
}
