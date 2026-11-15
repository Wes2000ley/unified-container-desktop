//go:build windows

package engine

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
	"github.com/skyri/ucd/internal/local"
)

// Opt in with an owned Windows probe image whose entrypoint supports `dns NAME`.
// This uses the installed authenticated broker, not an unauthenticated daemon.
func TestWindowsHostAliasesInstalledEngine(t *testing.T) {
	image := os.Getenv("UCD_WINDOWS_NETWORK_PROBE_IMAGE")
	if image == "" {
		t.Skip("set UCD_WINDOWS_NETWORK_PROBE_IMAGE to run physical-host Windows acceptance")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	relay, err := local.NewClient().DockerRelay(ctx, "windows")
	if err != nil {
		t.Fatal(err)
	}
	defer relay.Close()
	b, err := New("windows", relay.Endpoint(), t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	gateway, err := b.HostGateway(ctx, "nat")
	if err != nil {
		t.Fatal(err)
	}
	address, err := b.hostIPv4()
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct{ name, alias, override string }{
		{"host at initial startup", "host.docker.internal", ""},
		{"gateway at initial startup", "gateway.docker.internal", ""},
		{"explicit Compose-compatible extra host", "host.docker.internal", "192.0.2.55"},
	} {
		t.Run(test.name, func(t *testing.T) {
			host := &container.HostConfig{Isolation: container.IsolationHyperV}
			want := gateway.String()
			if test.alias == "host.docker.internal" {
				want = address.String()
			}
			if test.override != "" {
				host.ExtraHosts = []string{test.alias + ":" + test.override}
				want = test.override
			}
			options := client.ContainerCreateOptions{Config: &container.Config{Image: image, Cmd: []string{"dns", test.alias}, Labels: map[string]string{"com.ucd.acceptance": "host-aliases"}}, HostConfig: host}
			result, err := b.docker.ContainerCreate(ctx, options)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				clean, stop := context.WithTimeout(context.Background(), 30*time.Second)
				defer stop()
				if _, err := b.docker.ContainerRemove(clean, result.ID, client.ContainerRemoveOptions{Force: true}); err != nil {
					t.Errorf("remove owned fixture %s: %v", result.ID, err)
				}
			})
			if _, err = b.finishContainerCreate(ctx, result, nil, host); err != nil {
				t.Fatal(err)
			}
			if _, err = b.docker.ContainerStart(ctx, result.ID, client.ContainerStartOptions{}); err != nil {
				t.Fatal(err)
			}
			for {
				details, err := b.docker.ContainerInspect(ctx, result.ID, client.ContainerInspectOptions{})
				if err != nil {
					t.Fatal(err)
				}
				if !details.Container.State.Running {
					if details.Container.State.ExitCode != 0 {
						t.Fatalf("initial process exited %d", details.Container.State.ExitCode)
					}
					break
				}
				select {
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				case <-time.After(100 * time.Millisecond):
				}
			}
			logs, err := b.docker.ContainerLogs(ctx, result.ID, client.ContainerLogsOptions{ShowStdout: true, ShowStderr: true})
			if err != nil {
				t.Fatal(err)
			}
			data, err := io.ReadAll(logs)
			logs.Close()
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(data), `["`+want+`"]`) {
				t.Fatalf("initial DNS result = %q, want %s", data, want)
			}
		})
	}
}
