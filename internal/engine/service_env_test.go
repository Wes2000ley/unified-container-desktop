package engine

import (
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/docker/cli/cli/config/configfile"
	"github.com/docker/cli/cli/config/types"
)

func TestServiceDockerEnvironment(t *testing.T) {
	if os.Getenv("UCD_TEST_SERVICE_ENV") == "1" {
		root := os.Getenv("UCD_TEST_DATA_ROOT")
		cfg := configfile.New(filepath.Join(root, "docker-config", "config.json"))
		cfg.AuthConfigs["registry.invalid"] = types.AuthConfig{Username: "machine-user", Password: "machine-password"}
		before, err := cfg.GetAuthConfig("registry.invalid")
		if err != nil || before.Username != "environment-user" {
			t.Fatal("synthetic inherited credentials did not reach the SDK before service setup")
		}
		if err := ConfigureServiceDockerEnvironment(root); err != nil {
			t.Fatal(err)
		}
		after, err := cfg.GetAuthConfig("registry.invalid")
		if err != nil || after.Username != "machine-user" || after.Password != "machine-password" {
			t.Fatal("service did not use its machine registry credentials")
		}
		for _, key := range []string{"DOCKER_AUTH_CONFIG", "DOCKER_CONTEXT", "DOCKER_HOST", "DOCKER_TLS", "DOCKER_TLS_VERIFY", "DOCKER_CERT_PATH", "DOCKER_API_VERSION", "DOCKER_BUILDKIT", "BUILDX_CONFIG", "BUILDX_BUILDER", "BUILDKIT_HOST"} {
			if _, exists := os.LookupEnv(key); exists {
				t.Fatalf("service retained %s", key)
			}
		}
		if os.Getenv("DOCKER_CONFIG") != filepath.Join(root, "docker-config") {
			t.Fatal("service Docker config is not the machine directory")
		}
		cache, err := os.UserCacheDir()
		if err != nil || cache != filepath.Join(root, "cache") {
			t.Fatal("service cache is not the machine directory")
		}
		if info, err := os.Stat(cache); err != nil || !info.IsDir() {
			t.Fatal("machine cache directory was not created")
		}
		return
	}

	// Give the child only synthetic values; do not inspect the caller's auth environment.
	root := t.TempDir()
	auth := base64.StdEncoding.EncodeToString([]byte("environment-user:environment-password"))
	cmd := exec.Command(os.Args[0], "-test.run=^TestServiceDockerEnvironment$")
	cmd.Env = []string{
		"UCD_TEST_SERVICE_ENV=1", "UCD_TEST_DATA_ROOT=" + root,
		fmt.Sprintf(`DOCKER_AUTH_CONFIG={"auths":{"registry.invalid":{"auth":%q}}}`, auth),
		"DOCKER_CONFIG=other", "DOCKER_CONTEXT=other", "DOCKER_HOST=tcp://127.0.0.1:1", "DOCKER_TLS=1", "DOCKER_TLS_VERIFY=1", "DOCKER_CERT_PATH=other", "DOCKER_API_VERSION=1.0", "DOCKER_BUILDKIT=1",
		"BUILDX_CONFIG=other", "BUILDX_BUILDER=other", "BUILDKIT_HOST=tcp://127.0.0.1:2", "LocalAppData=foreign-cache", "XDG_CACHE_HOME=/foreign-cache",
	}
	if systemRoot := os.Getenv("SystemRoot"); systemRoot != "" {
		cmd.Env = append(cmd.Env, "SystemRoot="+systemRoot)
	}
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("isolated service environment test failed: %v\n%s", err, output)
	}
}
