package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// ConfigureServiceDockerEnvironment confines Compose and its Buildx subprocess
// to the machine configuration and configured engine. Call before starting workers.
func ConfigureServiceDockerEnvironment(dataRoot string) error {
	if !filepath.IsAbs(dataRoot) {
		return fmt.Errorf("machine data root must be absolute")
	}
	dataRoot = filepath.Clean(dataRoot)
	cache := filepath.Join(dataRoot, "cache")
	if err := os.MkdirAll(cache, 0700); err != nil {
		return fmt.Errorf("create machine cache directory: %w", err)
	}
	info, err := os.Lstat(cache)
	if err != nil {
		return fmt.Errorf("inspect machine cache directory: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("machine cache must be a real directory")
	}
	cacheVariable := "XDG_CACHE_HOME"
	if runtime.GOOS == "windows" {
		cacheVariable = "LocalAppData"
	}
	// Compose asks Docker Desktop for Hub credentials at UserCacheDir first.
	// A service-owned cache keeps that lookup away from interactive users.
	if err := os.Setenv(cacheVariable, cache); err != nil {
		return fmt.Errorf("set machine cache directory: %w", err)
	}
	for _, key := range []string{
		"DOCKER_AUTH_CONFIG", "DOCKER_CONTEXT", "DOCKER_HOST", "DOCKER_TLS", "DOCKER_TLS_VERIFY", "DOCKER_CERT_PATH", "DOCKER_API_VERSION", "DOCKER_BUILDKIT",
		"BUILDX_CONFIG", "BUILDX_BUILDER", "BUILDKIT_HOST",
	} {
		if err := os.Unsetenv(key); err != nil {
			return fmt.Errorf("clear service environment %s: %w", key, err)
		}
	}
	if err := os.Setenv("DOCKER_CONFIG", filepath.Join(dataRoot, "docker-config")); err != nil {
		return fmt.Errorf("set machine Docker configuration directory: %w", err)
	}
	return nil
}
