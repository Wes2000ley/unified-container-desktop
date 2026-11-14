// Package state stores machine metadata. Docker remains authoritative for runtime state.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sync"

	"github.com/skyri/ucd/internal/api"
)

type Store struct {
	mu       sync.Mutex
	path     string
	settings api.Settings
}

func Default(root string) api.Settings {
	return api.Settings{Version: 1, DataRoot: root, WindowsEndpoint: `npipe:////./pipe/UCDDocker`, HelperURL: "https://localhost:7443",
		CACert: filepath.Join(root, "certificates", "ca.pem"), ClientCert: filepath.Join(root, "certificates", "client.pem"), ClientKey: filepath.Join(root, "certificates", "client-key.pem"),
		WSL:      api.WSLSettings{Distro: "UCD-Linux", ServiceAccount: "UcdService", CPUs: 4, MemoryMB: 4096, DiskGB: 128, ProfileDirectory: filepath.Join(root, "service-profile"), InstallDirectory: filepath.Join(root, "wsl")},
		Projects: []api.Project{}, Network: api.DefaultNetworkSettings(), ResourceSaver: api.DefaultResourceSaverSettings()}
}

func Open(root string) (*Store, error) {
	if !filepath.IsAbs(root) {
		return nil, fmt.Errorf("machine data root must be absolute: %q", root)
	}
	s := &Store{path: filepath.Join(root, "config", "settings.json"), settings: Default(root)}
	b, err := os.ReadFile(s.path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read machine settings: %w", err)
	}
	if err == nil {
		if err = json.Unmarshal(b, &s.settings); err != nil {
			return nil, fmt.Errorf("decode %s: %w", s.path, err)
		}
		if err = migrateProxySettings(b, &s.settings); err != nil {
			return nil, fmt.Errorf("migrate %s: %w", s.path, err)
		}
	}
	if err = Validate(s.settings); err != nil {
		return nil, err
	}
	if s.settings.DataRoot != root {
		return nil, fmt.Errorf("settings dataRoot %q differs from service data root %q", s.settings.DataRoot, root)
	}
	return s, nil
}

func Validate(s api.Settings) error {
	if s.Version != 1 {
		return fmt.Errorf("unsupported settings version %d", s.Version)
	}
	if !filepath.IsAbs(s.DataRoot) {
		return fmt.Errorf("dataRoot must be absolute")
	}
	if s.WSL.CPUs < 1 || s.WSL.CPUs > 128 {
		return fmt.Errorf("WSL CPUs must be between 1 and 128")
	}
	if s.WSL.MemoryMB < 512 || s.WSL.MemoryMB > 1048576 {
		return fmt.Errorf("WSL memory must be between 512 and 1048576 MiB")
	}
	if s.WSL.DiskGB < 16 || s.WSL.DiskGB > 8192 {
		return fmt.Errorf("WSL disk size must be between 16 and 8192 GiB")
	}
	if s.WSL.Distro == "" || s.WSL.ServiceAccount == "" {
		return fmt.Errorf("WSL distro and service account are required")
	}
	if err := api.ValidateProxySettings("Docker", s.DockerProxy); err != nil {
		return err
	}
	if err := api.ValidateProxySettings("container", s.ContainersProxy); err != nil {
		return err
	}
	if err := api.ValidateNetworkSettings(s.Network); err != nil {
		return err
	}
	if err := api.ValidateResourceSaverSettings(s.ResourceSaver); err != nil {
		return err
	}
	for _, value := range s.RegistryMirrors {
		if value == "" {
			continue
		}
		u, err := url.Parse(value)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return fmt.Errorf("proxy and registry mirrors must be HTTP(S) URLs")
		}
		if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return fmt.Errorf("credentials, queries, and fragments must not be embedded in registry mirror URLs")
		}
	}
	ids := map[string]bool{}
	for _, p := range s.Projects {
		if !api.ValidName(p.ID) || ids[p.ID] {
			return fmt.Errorf("invalid or duplicate project ID %q", p.ID)
		}
		ids[p.ID] = true
		engines := map[string]bool{}
		for _, part := range p.Parts {
			if !api.ValidEngine(part.Engine) || engines[part.Engine] || !api.ValidName(part.Name) {
				return fmt.Errorf("invalid parts for project %q", p.ID)
			}
			engines[part.Engine] = true
		}
	}
	return nil
}

func migrateProxySettings(data []byte, settings *api.Settings) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	if _, hasCurrentProxy := fields["dockerProxy"]; hasCurrentProxy {
		return nil
	}
	legacy, ok := fields["proxy"]
	if !ok {
		return nil
	}
	var proxy string
	if err := json.Unmarshal(legacy, &proxy); err != nil {
		return fmt.Errorf("decode prior proxy setting: %w", err)
	}
	if proxy != "" {
		settings.DockerProxy.HTTP = proxy
		settings.DockerProxy.HTTPS = proxy
	}
	return nil
}

func clone(s api.Settings) api.Settings {
	s.RegistryMirrors = append([]string{}, s.RegistryMirrors...)
	s.Projects = append([]api.Project{}, s.Projects...)
	for i := range s.Projects {
		s.Projects[i].Parts = append([]api.ProjectPart{}, s.Projects[i].Parts...)
		s.Projects[i].StartupOrder = append([]string{}, s.Projects[i].StartupOrder...)
	}
	return s
}
func (s *Store) Snapshot() api.Settings { s.mu.Lock(); defer s.mu.Unlock(); return clone(s.settings) }

// Update publishes new metadata only after its atomic disk replacement succeeds.
func (s *Store) Update(change func(*api.Settings) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := clone(s.settings)
	if err := change(&next); err != nil {
		return err
	}
	if err := Validate(next); err != nil {
		return err
	}
	b, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return err
	}
	if err = AtomicWrite(s.path, append(b, '\n'), 0600); err != nil {
		return fmt.Errorf("save machine settings: %w", err)
	}
	s.settings = next
	return nil
}

func AtomicWrite(path string, b []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".ucd-*")
	if err != nil {
		return err
	}
	temp := f.Name()
	defer os.Remove(temp)
	if err = f.Chmod(mode); err != nil {
		f.Close()
		return err
	}
	if _, err = f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(temp, path)
}
