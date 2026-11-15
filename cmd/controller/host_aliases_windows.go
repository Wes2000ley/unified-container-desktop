//go:build windows

package main

import (
	"context"
	"net/netip"
	"time"

	"github.com/skyri/ucd/internal/api"
	"github.com/skyri/ucd/internal/dnsfilter"
	"github.com/skyri/ucd/internal/platform"
)

// Use the direct authenticated helper, rather than the Resource Saver gate:
// publishing an address must not start Docker or keep an idle engine awake.
func syncHostAliases(ctx context.Context, target api.Caller, events *api.EventBus) {
	changes, stop, err := platform.HostAddressChanges()
	if err != nil {
		events.Publish(api.Event{Type: "host_aliases.failed", Engine: "linux", Message: err.Error()})
		return
	}
	defer func() {
		if err := stop(); err != nil {
			events.Publish(api.Event{Type: "host_aliases.failed", Engine: "linux", Message: err.Error()})
		}
	}()
	var last netip.Addr
	var previousError string
	retry := time.NewTimer(0)
	defer retry.Stop()
	retryC := retry.C
	for {
		select {
		case <-ctx.Done():
			return
		case <-changes:
		case <-retryC:
			retryC = nil
		}
		check, cancel := context.WithTimeout(ctx, 10*time.Second)
		address, err := platform.HostIPv4()
		if err == nil && address != last {
			request, _ := api.NewRequest("engine.internal.hostaliases", "linux", dnsfilter.HostAliases{HostIPv4: address})
			_, err = target.Call(check, request)
			if err == nil {
				last = address
			}
		}
		cancel()
		if ctx.Err() != nil {
			return
		}
		message := ""
		if err != nil {
			message = err.Error()
		}
		if message != previousError {
			if err != nil {
				events.Publish(api.Event{Type: "host_aliases.failed", Engine: "linux", Message: message})
			} else {
				events.Publish(api.Event{Type: "host_aliases.restored", Engine: "linux", Message: "Docker host alias address synchronized"})
			}
			previousError = message
		}
		if err != nil {
			retry.Reset(5 * time.Second)
			retryC = retry.C
		} else {
			retry.Stop()
			retryC = nil
		}
	}
}
