package state

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/skyri/ucd/internal/api"
)

func TestAtomicMetadataPersistenceAndSnapshotIsolation(t *testing.T) {
	root := t.TempDir()
	s, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	err = s.Update(func(v *api.Settings) error {
		v.Projects = []api.Project{{ID: "demo", Name: "Demo", Parts: []api.ProjectPart{{Engine: "linux", Name: "demo-linux"}}}}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	copy := s.Snapshot()
	copy.Projects[0].Parts[0].Name = "corrupted"
	if s.Snapshot().Projects[0].Parts[0].Name != "demo-linux" {
		t.Fatal("snapshot shares mutable state")
	}
	reopened, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if reopened.Snapshot().Projects[0].ID != "demo" {
		t.Fatal("project not persisted")
	}
	before, _ := os.ReadFile(filepath.Join(root, "config", "settings.json"))
	err = s.Update(func(v *api.Settings) error { v.WSL.CPUs = 0; return nil })
	if err == nil {
		t.Fatal("invalid settings persisted")
	}
	after, _ := os.ReadFile(filepath.Join(root, "config", "settings.json"))
	if string(before) != string(after) {
		t.Fatal("failed update changed state")
	}
	err = s.Update(func(v *api.Settings) error {
		v.DockerProxy.HTTP = "https://changed.example"
		return errors.New("cancel update")
	})
	if err == nil || s.Snapshot().DockerProxy.HTTP != "" {
		t.Fatal("aborted mutation became visible")
	}
}

func TestLegacyProxyMigratesOnceAndRuntimeSettingsDefault(t *testing.T) {
	root := t.TempDir()
	old := Default(root)
	data, err := json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	var values map[string]json.RawMessage
	if err := json.Unmarshal(data, &values); err != nil {
		t.Fatal(err)
	}
	delete(values, "dockerProxy")
	delete(values, "containersProxy")
	delete(values, "network")
	delete(values, "resourceSaver")
	values["proxy"], _ = json.Marshal("https://proxy.example:8443")
	data, _ = json.Marshal(values)
	path := filepath.Join(root, "config", "settings.json")
	if err := AtomicWrite(path, data, 0600); err != nil {
		t.Fatal(err)
	}

	store, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	got := store.Snapshot()
	if got.DockerProxy.HTTP != "https://proxy.example:8443" || got.DockerProxy.HTTPS != got.DockerProxy.HTTP {
		t.Fatalf("legacy proxy was not split into both Docker proxy fields: %#v", got.DockerProxy)
	}
	if got.Network != api.DefaultNetworkSettings() {
		t.Fatalf("network defaults = %#v", got.Network)
	}
	if got.ResourceSaver != api.DefaultResourceSaverSettings() {
		t.Fatalf("resource saver defaults = %#v", got.ResourceSaver)
	}
	if err := store.Update(func(*api.Settings) error { return nil }); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), `"proxy"`) || !strings.Contains(string(data), `"dockerProxy"`) {
		t.Fatalf("legacy proxy field was not removed on persistence: %s", data)
	}
}

func TestRuntimeSettingsValidation(t *testing.T) {
	settings := Default(t.TempDir())
	cases := []struct {
		name   string
		change func(*api.Settings)
	}{
		{"proxy credentials", func(v *api.Settings) { v.DockerProxy.HTTP = "http://user:secret@proxy.example" }},
		{"unsupported SOCKS scheme", func(v *api.Settings) { v.ContainersProxy.SOCKS5 = "http://proxy.example:1080" }},
		{"invalid Docker subnet", func(v *api.Settings) { v.Network.DockerSubnet = "192.168.1.2/24" }},
		{"invalid network mode", func(v *api.Settings) { v.Network.DefaultNetworkingMode = "ipv5" }},
		{"invalid DNS inhibition", func(v *api.Settings) { v.Network.DNSInhibition = "ipv5" }},
		{"invalid port binding", func(v *api.Settings) { v.Network.DefaultPortBinding = "lan" }},
		{"invalid Resource Saver threshold", func(v *api.Settings) { v.ResourceSaver.IdleSeconds = 30 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			current := settings
			tc.change(&current)
			if err := Validate(current); err == nil {
				t.Fatal("invalid runtime settings passed validation")
			}
		})
	}
}

func TestCorruptSettingsDoNotSilentlyReset(t *testing.T) {
	root := t.TempDir()
	if err := AtomicWrite(filepath.Join(root, "config", "settings.json"), []byte(`{"version":`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(root); err == nil {
		t.Fatal("corrupt settings were silently replaced")
	}
}
