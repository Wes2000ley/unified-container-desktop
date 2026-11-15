//go:build windows

package main

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/skyri/ucd/internal/api"
	"github.com/skyri/ucd/internal/dnsfilter"
	"github.com/skyri/ucd/internal/platform"
)

type aliasCaller func(context.Context, api.Request) (any, error)

func (f aliasCaller) Call(ctx context.Context, r api.Request) (any, error) { return f(ctx, r) }

func TestHostAliasSynchronizationUsesAuthenticatedHelperAndStops(t *testing.T) {
	address, err := platform.HostIPv4()
	if err != nil {
		t.Skipf("no usable host IPv4 route: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	called := make(chan api.Request, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		syncHostAliases(ctx, aliasCaller(func(ctx context.Context, r api.Request) (any, error) { called <- r; return nil, nil }), &api.EventBus{})
	}()
	select {
	case request := <-called:
		var aliases dnsfilter.HostAliases
		if err := json.Unmarshal(request.Params, &aliases); err != nil {
			t.Fatal(err)
		}
		if request.Method != "engine.internal.hostaliases" || aliases.HostIPv4 != address {
			t.Fatalf("wrong helper request: %#v", request)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("host alias synchronization did not start")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("host alias synchronization did not stop")
	}
	for _, role := range []string{"administrator", "operator", "reader"} {
		if err := api.Authorize(api.Principal{SID: "test", Role: role}, "engine.internal.hostaliases"); err == nil {
			t.Fatalf("internal alias operation exposed to %s", role)
		}
	}
}
