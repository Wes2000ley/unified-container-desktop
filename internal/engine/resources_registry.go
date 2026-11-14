package engine

import (
	"context"
	"encoding/base64"
	"fmt"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/distribution/reference"
	"github.com/docker/cli/cli/config/types"
	"github.com/moby/moby/api/pkg/authconfig"
	"github.com/moby/moby/api/types/registry"
	"github.com/moby/moby/client"
	"github.com/skyri/ucd/internal/api"
)

const dockerHubServer = "https://index.docker.io/v1/"

type registrySnapshotContextKey struct{}

func withRegistrySnapshot(ctx context.Context, snapshot api.RegistrySnapshot) context.Context {
	return context.WithValue(ctx, registrySnapshotContextKey{}, snapshot)
}

func registrySnapshotFromContext(ctx context.Context) (api.RegistrySnapshot, bool) {
	snapshot, ok := ctx.Value(registrySnapshotContextKey{}).(api.RegistrySnapshot)
	return snapshot, ok
}

// machineRegistryAuth deliberately reads the machine configuration directly.
// Docker CLI credential lookup can otherwise consult DOCKER_AUTH_CONFIG from
// the service environment, which is outside this application's config.
func (b *Backend) machineRegistryAuth(ctx context.Context, image string) (string, []string, error) {
	ref, err := reference.ParseNormalizedNamed(image)
	if err != nil {
		return "", nil, err
	}
	domain := reference.Domain(ref)
	server, err := registryServer(domain)
	if err != nil {
		return "", nil, err
	}
	var auth types.AuthConfig
	if snapshot, ok := registrySnapshotFromContext(ctx); ok {
		auth = registryAuthConfig(server, snapshot.AuthConfigs[server])
	} else {
		b.configMu.Lock()
		defer b.configMu.Unlock()
		if b.registryConfig.CredentialsStore != "" || b.registryConfig.CredentialHelpers[server] != "" || b.registryConfig.CredentialHelpers[domain] != "" {
			return "", nil, fmt.Errorf("machine Docker configuration uses an external credential helper for this registry; configure machine file credentials before continuing")
		}
		auth = b.registryConfig.AuthConfigs[server]
	}
	token, err := authconfig.Encode(registry.AuthConfig(auth))
	if err != nil {
		return "", nil, err
	}
	return token, []string{token, auth.Password, auth.IdentityToken, auth.RegistryToken}, nil
}

func registryServer(server string) (string, error) {
	server = strings.ToLower(server)
	if server == "" || server == "docker.io" || server == "index.docker.io" || server == dockerHubServer {
		return dockerHubServer, nil
	}
	if strings.ContainsAny(server, "/?#@ \t\r\n\\") || strings.Contains(server, "://") {
		return "", api.Fail("invalid_argument", "registry must be a hostname with an optional port")
	}
	u, err := url.Parse("https://" + server)
	if err != nil || u.Hostname() == "" || u.Host != server || u.User != nil {
		return "", api.Fail("invalid_argument", "invalid registry hostname")
	}
	return server, nil
}

func registryServerURL(server string) (string, error) {
	server = strings.TrimSpace(server)
	if server == dockerHubServer {
		return dockerHubServer, nil
	}
	if strings.Contains(server, "://") {
		u, err := url.Parse(server)
		if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
			return "", api.Fail("invalid_argument", "registry URL must be an HTTPS origin without credentials, a query, or a path")
		}
		server = u.Host
	}
	return registryServer(server)
}

func (b *Backend) registryCall(ctx context.Context, req api.Request) (any, error) {
	switch req.Method {
	case "registry.internal.snapshot":
		return b.registrySnapshot(), nil
	case "registry.internal.call":
		p, err := api.Decode[api.RegistryOperation](req)
		if err != nil {
			return nil, err
		}
		if !needsRegistryAuth(p.Request) {
			return nil, api.Fail("permission_denied", "internal registry calls require an image or Compose operation that uses registry credentials")
		}
		if p.Request.Engine != "" && p.Request.Engine != b.id {
			return nil, api.Fail("invalid_argument", "registry operation targets a different engine")
		}
		p.Request.Engine = b.id
		return b.Call(withRegistrySnapshot(ctx, p.Snapshot), p.Request)
	case "registry.internal.validate":
		p, err := api.Decode[api.RegistryParams](req)
		if err != nil {
			return nil, err
		}
		return b.validateRegistryLogin(ctx, p)
	case "registry.internal.store":
		p, err := api.Decode[api.RegistryAuthParams](req)
		if err != nil {
			return nil, err
		}
		server, err := registryServerURL(p.Server)
		if err != nil {
			return nil, err
		}
		auth := registryAuthConfig(server, p.Auth)
		if auth.Username == "" && auth.Password == "" && auth.IdentityToken == "" && auth.RegistryToken == "" {
			return nil, api.Fail("invalid_argument", "registry authentication result is empty")
		}
		return nil, b.saveRegistryAuth(server, &auth)
	case "registry.credential.get", "registry.credential.store", "registry.credential.erase", "registry.credential.list":
		return b.registryCredentialCall(req)
	}

	p, err := api.Decode[api.RegistryParams](req)
	if err != nil {
		return nil, err
	}
	if len(p.Server) > 255 || len(p.Username) > 1024 || len(p.Password) > 4096 {
		return nil, api.Fail("invalid_argument", "registry credentials exceed the supported length")
	}
	if req.Method == "registry.list" {
		b.configMu.Lock()
		defer b.configMu.Unlock()
		out := make([]map[string]string, 0, len(b.registryConfig.AuthConfigs))
		for server, auth := range b.registryConfig.AuthConfigs {
			username := auth.Username
			if auth.IdentityToken != "" || auth.RegistryToken != "" {
				username = "<token>"
			}
			out = append(out, map[string]string{"server": server, "username": username})
		}
		sort.Slice(out, func(i, j int) bool { return out[i]["server"] < out[j]["server"] })
		return out, nil
	}
	server, err := registryServerURL(p.Server)
	if err != nil {
		return nil, err
	}
	b.configMu.Lock()
	unsupportedHelper := b.registryConfig.CredentialsStore != "" || b.registryConfig.CredentialHelpers[server] != ""
	b.configMu.Unlock()
	if unsupportedHelper {
		return nil, fmt.Errorf("machine Docker configuration uses an external credential helper for this registry; configure machine file credentials before continuing")
	}
	var auth types.AuthConfig
	switch req.Method {
	case "registry.login":
		validated, err := b.validateRegistryLogin(ctx, p)
		if err != nil {
			return nil, err
		}
		auth = registryAuthConfig(server, validated)
	case "registry.logout":
	default:
		return nil, api.Fail("invalid_argument", "unknown registry method: "+req.Method)
	}
	if req.Method == "registry.login" {
		if err := b.saveRegistryAuth(server, &auth); err != nil {
			return nil, err
		}
	} else if err := b.saveRegistryAuth(server, nil); err != nil {
		return nil, err
	}
	return nil, nil
}

func (b *Backend) validateRegistryLogin(ctx context.Context, p api.RegistryParams) (api.RegistryAuth, error) {
	if len(p.Server) > 255 || len(p.Username) > 1024 || len(p.Password) > 4096 {
		return api.RegistryAuth{}, api.Fail("invalid_argument", "registry credentials exceed the supported length")
	}
	server, err := registryServerURL(p.Server)
	if err != nil {
		return api.RegistryAuth{}, err
	}
	if p.Username == "" || p.Password == "" {
		return api.RegistryAuth{}, api.Fail("invalid_argument", "registry username and password or token are required")
	}
	b.configMu.Lock()
	unsupportedHelper := b.registryConfig.CredentialsStore != "" || b.registryConfig.CredentialHelpers[server] != ""
	b.configMu.Unlock()
	if unsupportedHelper {
		return api.RegistryAuth{}, fmt.Errorf("machine Docker configuration uses an external credential helper for this registry; configure machine file credentials before continuing")
	}
	result, err := b.docker.RegistryLogin(ctx, client.RegistryLoginOptions{ServerAddress: server, Username: p.Username, Password: p.Password})
	if err != nil {
		return api.RegistryAuth{}, fmt.Errorf("registry authentication failed: %s", redactRegistryError(err.Error(), p.Username, p.Password))
	}
	validated := api.RegistryAuth{Server: server, Username: p.Username, Password: p.Password}
	if result.Auth.IdentityToken != "" {
		validated.Username = ""
		validated.Password = ""
		validated.IdentityToken = result.Auth.IdentityToken
	}
	return validated, nil
}

func registryAuthConfig(server string, auth api.RegistryAuth) types.AuthConfig {
	return types.AuthConfig{
		ServerAddress: server,
		Username:      auth.Username,
		Password:      auth.Password,
		IdentityToken: auth.IdentityToken,
		RegistryToken: auth.RegistryToken,
	}
}

func (b *Backend) registrySnapshot() api.RegistrySnapshot {
	b.configMu.Lock()
	defer b.configMu.Unlock()
	result := api.RegistrySnapshot{AuthConfigs: make(map[string]api.RegistryAuth, len(b.registryConfig.AuthConfigs))}
	for server, auth := range b.registryConfig.AuthConfigs {
		result.AuthConfigs[server] = api.RegistryAuth{
			Username: auth.Username, Password: auth.Password,
			IdentityToken: auth.IdentityToken, RegistryToken: auth.RegistryToken,
		}
	}
	return result
}

func needsRegistryAuth(request api.Request) bool {
	switch request.Method {
	case "image.pull", "image.push", "image.build":
		return true
	case "transfer.open":
		p, err := api.Decode[api.TransferOpenParams](request)
		return err == nil && p.Operation == "image.build"
	case "compose.action":
		p, err := api.Decode[api.ComposeParams](request)
		return err == nil && (p.Action == "up" || p.Action == "build" || p.Action == "pull")
	default:
		return false
	}
}

func (b *Backend) registryCredentialCall(req api.Request) (any, error) {
	if req.Method == "registry.credential.list" {
		b.configMu.Lock()
		defer b.configMu.Unlock()
		out := make(map[string]string, len(b.registryConfig.AuthConfigs))
		for server, auth := range b.registryConfig.AuthConfigs {
			username := auth.Username
			if auth.IdentityToken != "" || auth.RegistryToken != "" {
				username = "<token>"
			}
			out[server] = username
		}
		return out, nil
	}
	p, err := api.Decode[api.RegistryCredentialParams](req)
	if err != nil {
		return nil, err
	}
	if len(p.ServerURL) > 1024 || len(p.Username) > 1024 || len(p.Secret) > 4096 {
		return nil, api.Fail("invalid_argument", "registry credentials exceed the supported length")
	}
	server, err := registryServerURL(p.ServerURL)
	if err != nil {
		return nil, err
	}
	switch req.Method {
	case "registry.credential.get":
		b.configMu.Lock()
		auth, ok := b.registryConfig.AuthConfigs[server]
		b.configMu.Unlock()
		if !ok {
			return nil, api.Fail("not_found", "credentials not found in native keychain")
		}
		credential := api.RegistryCredentialParams{ServerURL: p.ServerURL, Username: auth.Username, Secret: auth.Password}
		if auth.IdentityToken != "" {
			credential.Username, credential.Secret = "<token>", auth.IdentityToken
		} else if auth.RegistryToken != "" {
			credential.Username, credential.Secret = "<token>", auth.RegistryToken
		}
		return credential, nil
	case "registry.credential.store":
		if p.Username == "" || p.Secret == "" {
			return nil, api.Fail("invalid_argument", "registry credential username and secret are required")
		}
		auth := &types.AuthConfig{ServerAddress: server}
		if p.Username == "<token>" {
			auth.IdentityToken = p.Secret
		} else {
			auth.Username, auth.Password = p.Username, p.Secret
		}
		return nil, b.saveRegistryAuth(server, auth)
	case "registry.credential.erase":
		return nil, b.saveRegistryAuth(server, nil)
	default:
		return nil, api.Fail("invalid_argument", "unknown registry credential method: "+req.Method)
	}
}

func (b *Backend) saveRegistryAuth(server string, auth *types.AuthConfig) error {
	b.configMu.Lock()
	defer b.configMu.Unlock()
	if b.registryConfig.CredentialsStore != "" || b.registryConfig.CredentialHelpers[server] != "" {
		return fmt.Errorf("machine Docker configuration uses an external credential helper for this registry; configure machine file credentials before continuing")
	}
	copyConfig := *b.registryConfig
	copyConfig.AuthConfigs = maps.Clone(b.registryConfig.AuthConfigs)
	if copyConfig.AuthConfigs == nil {
		copyConfig.AuthConfigs = make(map[string]types.AuthConfig)
	}
	copyConfig.HTTPHeaders = maps.Clone(b.registryConfig.HTTPHeaders)
	if auth != nil {
		copyConfig.AuthConfigs[server] = *auth
	} else {
		delete(copyConfig.AuthConfigs, server)
	}
	dir := filepath.Join(b.dataRoot, "docker-config")
	info, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("inspect machine Docker configuration directory: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("machine Docker configuration directory must not be a symbolic link")
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return fmt.Errorf("open machine Docker configuration: %w", err)
	}
	defer root.Close()
	if err := noSymlinks(root, "config.json"); err != nil {
		return fmt.Errorf("inspect machine Docker configuration: %w", err)
	}
	temp, err := randomName()
	if err != nil {
		return err
	}
	file, err := root.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return fmt.Errorf("create machine Docker configuration: %w", err)
	}
	defer root.Remove(temp)
	err = copyConfig.SaveToWriter(file)
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil {
		err = root.Rename(temp, "config.json")
	}
	if err != nil {
		return fmt.Errorf("save machine Docker configuration: %w", err)
	}
	b.registryConfig.AuthConfigs = copyConfig.AuthConfigs
	return nil
}

func redactRegistryError(message, username, secret string) string {
	if secret == "" {
		return message
	}
	for _, value := range []string{secret, base64.StdEncoding.EncodeToString([]byte(secret)), base64.URLEncoding.EncodeToString([]byte(secret)), base64.StdEncoding.EncodeToString([]byte(username + ":" + secret)), base64.URLEncoding.EncodeToString([]byte(username + ":" + secret))} {
		message = strings.ReplaceAll(message, value, "[redacted]")
	}
	return message
}
