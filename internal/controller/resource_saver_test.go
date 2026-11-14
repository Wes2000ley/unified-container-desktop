package controller

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/skyri/ucd/internal/api"
	"github.com/skyri/ucd/internal/resourcesaver"
)

type saverRuntimeFunc func(context.Context, bool) error

func (f saverRuntimeFunc) SetLinuxDockerPaused(ctx context.Context, paused bool) error {
	return f(ctx, paused)
}

func TestResourceSaverSettingDoesNotReconfigureEngines(t *testing.T) {
	for _, diskFailure := range []bool{false, true} {
		t.Run(map[bool]string{false: "saved", true: "save failure restores gate"}[diskFailure], func(t *testing.T) {
			store := testStore(t)
			initial := store.Snapshot()
			backend := callerFunc(func(context.Context, api.Request) (any, error) {
				t.Fatal("changing idle settings must not reconfigure or restart Docker")
				return nil, nil
			})
			gate, err := resourcesaver.New(resourcesaver.Config{IdleSeconds: initial.ResourceSaver.IdleSeconds}, backend, saverRuntimeFunc(func(context.Context, bool) error {
				t.Fatal("changing an awake daemon's idle settings must not stop it")
				return nil
			}), &api.EventBus{})
			if err != nil {
				t.Fatal(err)
			}
			controller := New(store, backend, backend, nil, &api.EventBus{})
			controller.SetResourceSaver(gate)
			if diskFailure {
				if err := os.WriteFile(filepath.Join(initial.DataRoot, "config"), []byte("block atomic save"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			next := initial
			next.ResourceSaver = api.ResourceSaverSettings{Enabled: true, IdleSeconds: 31}
			value, err := controller.Call(admin(), request(t, "system.configure", "", next))
			if diskFailure {
				if err == nil {
					t.Fatal("failed atomic save reported success")
				}
				if gate.Snapshot().Enabled || store.Snapshot().ResourceSaver != initial.ResourceSaver {
					t.Fatal("failed save changed the gate or stored settings")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var result struct {
				RestartRequired bool `json:"restartRequired"`
			}
			if err := convert(value, &result); err != nil {
				t.Fatal(err)
			}
			if result.RestartRequired || !gate.Snapshot().Enabled || gate.Snapshot().IdleSeconds != 31 || store.Snapshot().ResourceSaver != next.ResourceSaver {
				t.Fatalf("idle settings were not applied without restart: %#v", value)
			}
		})
	}
}
