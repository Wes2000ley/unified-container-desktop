package controller

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"strconv"
	"testing"

	"github.com/skyri/ucd/internal/api"
)

func TestCorePermissionsAreEnforcedBeforeEngineCalls(t *testing.T) {
	for _, method := range []string{
		"container.exec.open", "container.exec.read", "container.files.list", "container.files.read",
		"container.files.prepare",
		"container.create", "container.run", "container.rename", "container.update", "container.commit",
		"image.inspect", "image.history", "image.tag", "image.push", "image.load", "image.save",
		"volume.create", "volume.backup", "volume.restore", "volume.clone", "volume.empty", "volume.files.read",
		"network.create", "network.remove", "network.connect", "network.disconnect", "registry.login", "registry.logout",
		"storage.prune", "transfer.open", "transfer.read", "transfer.write", "transfer.finish", "transfer.close",
	} {
		t.Run(method, func(t *testing.T) {
			called := false
			engine := callerFunc(func(context.Context, api.Request) (any, error) { called = true; return nil, nil })
			c := New(testStore(t), engine, engine, nil, &api.EventBus{})
			ctx := api.WithPrincipal(context.Background(), api.Principal{SID: "viewer", Role: "viewer"})
			_, err := c.Call(ctx, api.Request{Method: method, Engine: "linux"})
			var denied *api.Error
			if !errors.As(err, &denied) || denied.Code != "permission_denied" || called {
				t.Fatalf("viewer reached privileged operation: called=%v err=%v", called, err)
			}
		})
	}
}

func TestCoreReadsAndMutationsRouteToTheirSelectedEngine(t *testing.T) {
	for _, method := range []string{
		"container.exec.open", "container.files.list", "container.rename", "image.inspect", "image.build",
		"container.files.prepare",
		"volume.create", "volume.clone", "volume.files.upload", "network.connect",
		"storage.prune", "transfer.open", "transfer.read", "transfer.close",
	} {
		t.Run(method, func(t *testing.T) {
			windows := callerFunc(func(_ context.Context, got api.Request) (any, error) {
				if got.Method == "registry.internal.snapshot" {
					return api.RegistrySnapshot{AuthConfigs: map[string]api.RegistryAuth{}}, nil
				}
				t.Fatal("wrong engine called")
				return nil, nil
			})
			linux := callerFunc(func(_ context.Context, got api.Request) (any, error) {
				if method == "image.build" {
					if got.Method != "registry.internal.call" || got.Engine != "linux" {
						t.Fatalf("wrapped request = %#v", got)
					}
					var operation api.RegistryOperation
					if err := json.Unmarshal(got.Params, &operation); err != nil {
						t.Fatal(err)
					}
					got = operation.Request
				}
				if got.Method != method || got.Engine != "linux" || string(got.Params) != `{"marker":"preserved"}` {
					t.Fatalf("request changed: %#v", got)
				}
				return "result", nil
			})
			c := New(testStore(t), windows, linux, nil, &api.EventBus{})
			ctx := api.WithPrincipal(context.Background(), api.Principal{SID: "operator", Role: "operator"})
			r := request(t, method, "linux", map[string]string{"marker": "preserved"})
			got, err := c.Call(ctx, r)
			if err != nil || got != "result" {
				t.Fatalf("route: %v, %v", got, err)
			}
			r.Engine = ""
			if _, err := c.Call(ctx, r); err == nil {
				t.Fatal("accepted engine-less operation")
			}
		})
	}
}

func TestViewerCanReadStatusAndLogsWithoutContainerAdministration(t *testing.T) {
	for _, method := range []string{"container.logs.open", "container.logs.read", "container.logs.close", "container.stats", "network.inspect", "registry.list", "storage.df", "storage.preview"} {
		t.Run(method, func(t *testing.T) {
			engine := callerFunc(func(context.Context, api.Request) (any, error) { return "allowed", nil })
			c := New(testStore(t), engine, engine, nil, &api.EventBus{})
			ctx := api.WithPrincipal(context.Background(), api.Principal{SID: "viewer", Role: "viewer"})
			got, err := c.Call(ctx, api.Request{Method: method, Engine: "windows"})
			if err != nil || got != "allowed" {
				t.Fatalf("read: %v, %v", got, err)
			}
		})
	}
}

func TestStandaloneCreateChecksOccupiedHostPortBeforeCreating(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	_, port, _ := net.SplitHostPort(listener.Addr().String())
	created := false
	engine := callerFunc(func(_ context.Context, r api.Request) (any, error) {
		if r.Method == "container.list" {
			return []api.Container{}, nil
		}
		created = true
		return nil, nil
	})
	c := New(testStore(t), engine, engine, nil, &api.EventBus{})
	r := request(t, "container.run", "linux", map[string]any{
		"config":     map[string]any{"Image": "busybox:1.37.0"},
		"hostConfig": map[string]any{"PortBindings": map[string]any{"80/tcp": []any{map[string]any{"HostIp": "127.0.0.1", "HostPort": port}}}},
	})
	_, err = c.Call(admin(), r)
	var conflict *api.Error
	if !errors.As(err, &conflict) || conflict.Code != "port_conflict" || created {
		t.Fatalf("created with occupied port %s: called=%v err=%v", port, created, err)
	}
}

func TestStandaloneCreatePreflightUsesDefaultBindingAndPreservesExplicitHost(t *testing.T) {
	for _, engineID := range []string{"linux", "windows"} {
		for _, explicit := range []bool{false, true} {
			t.Run(engineID+"/explicit="+strconv.FormatBool(explicit), func(t *testing.T) {
				listener, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				port := listener.Addr().(*net.TCPAddr).Port
				if err := listener.Close(); err != nil {
					t.Fatal(err)
				}
				s := testStore(t)
				if err := s.Update(func(settings *api.Settings) error {
					settings.Network.DefaultPortBinding = api.DefaultPortBindingAll
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				created := false
				engine := callerFunc(func(_ context.Context, r api.Request) (any, error) {
					if r.Method == "container.list" {
						return []api.Container{{ID: "existing", Ports: []api.Port{{HostIP: "127.0.0.2", Published: uint16(port), Target: 80, Protocol: "tcp"}}}}, nil
					}
					created = true
					return api.ContainerCreateResult{ID: "new"}, nil
				})
				binding := map[string]any{"HostPort": strconv.Itoa(port)}
				if explicit {
					binding["HostIp"] = "127.0.0.1"
				}
				c := New(s, engine, engine, nil, &api.EventBus{})
				_, err = c.Call(admin(), request(t, "container.create", engineID, map[string]any{
					"config":     map[string]any{"Image": "fixture"},
					"hostConfig": map[string]any{"PortBindings": map[string]any{"80/tcp": []any{binding}}},
				}))
				if explicit {
					if err != nil || !created {
						t.Fatalf("explicit localhost was treated as an all-address binding: created=%v err=%v", created, err)
					}
					return
				}
				var conflict *api.Error
				if !errors.As(err, &conflict) || conflict.Code != "port_conflict" || created {
					t.Fatalf("all-address default missed the existing address-specific binding: created=%v err=%v", created, err)
				}
			})
		}
	}
}

func TestNetworkInventoryRetainsAvailableEngineAndItsError(t *testing.T) {
	good := callerFunc(func(context.Context, api.Request) (any, error) {
		return []map[string]string{{"Name": "nat", "engine": "windows"}}, nil
	})
	bad := callerFunc(func(context.Context, api.Request) (any, error) { return nil, errors.New("Linux offline") })
	c := New(testStore(t), good, bad, nil, &api.EventBus{})
	value, err := c.Call(admin(), api.Request{Method: "network.list"})
	if err != nil {
		t.Fatal(err)
	}
	result := value.(Inventory)
	if len(result.Items) != 1 || len(result.Errors) != 1 || result.Errors[0].Engine != "linux" {
		t.Fatalf("lost network inventory: %#v", result)
	}
}

func TestProjectServiceSelectionAndLogSessions(t *testing.T) {
	s := testStore(t)
	saveProject(t, s)
	var methods []string
	engine := callerFunc(func(_ context.Context, r api.Request) (any, error) {
		methods = append(methods, r.Method)
		switch r.Method {
		case "compose.ports", "compose.action", "compose.logs.open":
			p, err := api.Decode[api.ComposeParams](r)
			if err != nil || len(p.Services) != 1 || p.Services[0] != "web" {
				t.Fatalf("selection lost: %#v %v", p, err)
			}
			if r.Method == "compose.ports" {
				return []api.Port{}, nil
			}
			return api.ExecOpenResult{SessionID: "session-" + r.Engine}, nil
		case "compose.logs.read", "compose.logs.close":
			p, err := api.Decode[api.ExecParams](r)
			if err != nil || p.SessionID != "session-linux" {
				t.Fatalf("session changed: %#v %v", p, err)
			}
			return api.ExecReadResult{EOF: true}, nil
		default:
			return nil, errors.New("unexpected " + r.Method)
		}
	})
	c := New(s, engine, engine, nil, &api.EventBus{})
	params := api.ProjectParams{ID: "test", Engine: "linux", Action: "up", Services: []string{"web"}}
	if _, err := c.Call(admin(), request(t, "project.action", "", params)); err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"down", "remove"} {
		params.Action = action
		if _, err := c.Call(admin(), request(t, "project.action", "", params)); err == nil {
			t.Fatalf("accepted selected-service %s", action)
		}
	}
	viewer := api.WithPrincipal(context.Background(), api.Principal{SID: "viewer", Role: "viewer"})
	value, err := c.Call(viewer, request(t, "project.logs.open", "", params))
	if err != nil {
		t.Fatal(err)
	}
	result := value.(api.OperationResult)
	if !result.Success || len(result.Parts) != 1 || result.Parts[0].Engine != "linux" {
		t.Fatalf("bad log open: %#v", result)
	}
	for _, suffix := range []string{"read", "close"} {
		if _, err := c.Call(viewer, request(t, "project.logs."+suffix, "linux", api.ExecParams{SessionID: "session-linux"})); err != nil {
			t.Fatal(err)
		}
	}
	if len(methods) != 5 {
		t.Fatalf("unexpected methods %v", methods)
	}
}
