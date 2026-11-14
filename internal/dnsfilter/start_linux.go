//go:build linux

package dnsfilter

import (
	"context"
	"errors"
	"fmt"
	"net"
	"path/filepath"

	dockerclient "github.com/moby/moby/client"
)

// Start serves DNS on the UCD-owned addresses assigned to Linux loopback until
// ctx is canceled. It sends one readiness notification after all UDP and TCP
// listeners bind. Queries use explicit upstreams or the distro's current
// resolver configuration.
func Start(ctx context.Context, configPath string, ready chan<- struct{}) error {
	if ctx == nil {
		return errors.New("DNS filter context is required")
	}
	if ready == nil {
		return errors.New("DNS filter readiness channel is required")
	}
	if configPath == "" || !filepath.IsAbs(configPath) {
		return errors.New("DNS filter configuration path must be absolute")
	}
	if err := ensureLoopbackDNSAddresses(); err != nil {
		return fmt.Errorf("configure DNS addresses on Linux loopback: %w", err)
	}
	settings := &settingsFile{path: configPath}
	if _, err := settings.load(); err != nil {
		return err
	}
	resolvers := &resolverFile{path: "/etc/resolv.conf"}
	docker, err := dockerclient.New(dockerclient.WithHost("unix:///var/run/docker.sock"))
	if err != nil {
		return fmt.Errorf("create DNS policy Docker client: %w", err)
	}
	defer docker.Close()
	policies := &networkPolicies{docker: docker}

	addresses := []string{
		net.JoinHostPort(IPv4Address, serverPort),
		net.JoinHostPort(IPv6Address, serverPort),
	}
	return serveAddresses(ctx, addresses, settings, resolvers, policies, exchange, ready)
}

func serveAddresses(ctx context.Context, addresses []string, settings *settingsFile, resolvers *resolverFile, policies *networkPolicies, forward exchangeFunc, ready chan<- struct{}) error {
	if len(addresses) == 0 {
		return errors.New("DNS filter requires at least one listener address")
	}
	serverCtx, stopServers := context.WithCancel(ctx)
	defer stopServers()
	started := make(chan struct{}, len(addresses))
	results := make(chan error, len(addresses))
	for _, address := range addresses {
		address := address
		go func() {
			results <- serve(serverCtx, address, settings, resolvers, policies, forward, started)
		}()
	}

	remaining := len(addresses)
	readyCount := 0
	startedEvents := (<-chan struct{})(started)
	contextDone := ctx.Done()
	readySent := false
	var firstErr error
	for remaining > 0 {
		if !readySent && readyCount == len(addresses) {
			if ctx.Err() == nil {
				ready <- struct{}{}
			}
			readySent = true
			startedEvents = nil
		}
		select {
		case <-startedEvents:
			readyCount++
		case err := <-results:
			remaining--
			if err == nil && ctx.Err() == nil {
				err = errors.New("DNS listener stopped unexpectedly")
			}
			if err != nil && ctx.Err() == nil && firstErr == nil {
				firstErr = err
				stopServers()
				startedEvents = nil
				contextDone = nil
			}
		case <-contextDone:
			stopServers()
			startedEvents = nil
			contextDone = nil
		}
	}
	if ctx.Err() != nil {
		return nil
	}
	return firstErr
}
