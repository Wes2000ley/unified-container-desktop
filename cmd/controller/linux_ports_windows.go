//go:build windows

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sort"
	"strconv"
	"time"

	"github.com/moby/moby/client"
	"github.com/skyri/ucd/internal/api"
	"github.com/skyri/ucd/internal/engine"
)

type linuxPortKey struct {
	container string
	index     int
}

type linuxHostForward struct {
	binding api.LinuxPortBinding
	close   func()
}

type linuxHostPorts struct {
	helper   api.Caller
	forwards map[linuxPortKey]linuxHostForward
}

func (p *linuxHostPorts) close() {
	for key, forward := range p.forwards {
		forward.close()
		delete(p.forwards, key)
	}
}

func (p *linuxHostPorts) publish(ctx context.Context) error {
	state := api.LinuxPortBindings{Bindings: []api.LinuxPortBinding{}}
	for _, forward := range p.forwards {
		state.Bindings = append(state.Bindings, forward.binding)
	}
	sort.Slice(state.Bindings, func(i, j int) bool {
		a, b := state.Bindings[i], state.Bindings[j]
		if a.Container == b.Container {
			return a.Index < b.Index
		}
		return a.Container < b.Container
	})
	request, err := api.NewRequest("engine.internal.portstate", "linux", state)
	if err != nil {
		return err
	}
	_, err = p.helper.Call(ctx, request)
	return err
}

// Windows owns public sockets; Docker owns private WSL loopback endpoints.
// This avoids WSL's public-port reservation/path that Windows NAT cannot reach.
func (p *linuxHostPorts) reconcile(ctx context.Context) error {
	fail := func(err error) error {
		p.close()
		clear, stop := context.WithTimeout(ctx, 3*time.Second)
		defer stop()
		if clearErr := p.publish(clear); clearErr != nil {
			return errors.Join(err, fmt.Errorf("clear Linux host-port state: %w", clearErr))
		}
		return err
	}
	result, err := p.helper.Call(ctx, api.Request{Method: "engine.internal.portbindings", Engine: "linux"})
	if err != nil {
		return fail(fmt.Errorf("read Linux published ports: %w", err))
	}
	data, err := json.Marshal(result)
	var snapshot api.LinuxPortBindings
	if err == nil {
		err = json.Unmarshal(data, &snapshot)
	}
	if err != nil {
		return fail(fmt.Errorf("decode Linux published ports: %w", err))
	}
	desired := make(map[linuxPortKey]api.LinuxPortBinding, len(snapshot.Bindings))
	for _, binding := range snapshot.Bindings {
		key := linuxPortKey{binding.Container, binding.Index}
		if binding.Container == "" || binding.Index < 0 || binding.RelayPort == 0 {
			return fail(errors.New("Linux published endpoint has an invalid identity or private port"))
		}
		if _, exists := desired[key]; exists {
			return fail(errors.New("Linux published endpoint has a duplicate identity"))
		}
		desired[key] = binding
	}
	for key, forward := range p.forwards {
		want, ok := desired[key]
		got := forward.binding
		if !ok || got.RelayPort != want.RelayPort || got.Port.HostIP != want.Port.HostIP || got.Port.Target != want.Port.Target || got.Port.Protocol != want.Port.Protocol || (want.Port.Published != 0 && got.Port.Published != want.Port.Published) {
			forward.close()
			delete(p.forwards, key)
		}
	}
	if p.forwards == nil {
		p.forwards = make(map[linuxPortKey]linuxHostForward)
	}
	keys := make([]linuxPortKey, 0, len(desired))
	for key := range desired {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].container == keys[j].container {
			return keys[i].index < keys[j].index
		}
		return keys[i].container < keys[j].container
	})
	var failures []error
	for _, key := range keys {
		want := desired[key]
		if _, ok := p.forwards[key]; ok {
			continue
		}
		port := want.Port
		target := net.JoinHostPort("127.0.0.1", strconv.Itoa(int(want.RelayPort)))
		actual, closeForward, err := engine.StartHostPortForward(port, target)
		if err != nil {
			failures = append(failures, fmt.Errorf("container %s cannot bind Linux %s %s:%d: %w", key.container, port.Protocol, port.HostIP, port.Published, err))
			continue
		}
		want.Port = actual
		p.forwards[key] = linuxHostForward{binding: want, close: closeForward}
	}
	if err := p.publish(ctx); err != nil {
		failures = append(failures, fail(fmt.Errorf("publish Linux host-port state: %w", err)))
	}
	return errors.Join(failures...)
}

// Docker events drive reconciliation. Timers only retry unavailable engines or
// failed listeners; reading status never starts a sleeping Linux Docker service.
func runLinuxHostPorts(ctx context.Context, helper api.Caller, docker *client.Client, events *api.EventBus) {
	ports := linuxHostPorts{helper: helper}
	defer func() {
		ports.close()
		cleanup, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = ports.publish(cleanup)
	}()
	last := time.Now().Add(-time.Second).UnixNano()
	backoff := time.Second
	previousError := ""
	report := func(err error) {
		message := ""
		if err != nil {
			message = err.Error()
		}
		if message == previousError || ctx.Err() != nil {
			return
		}
		kind, text := "engine.ports_reconciled", "Linux published Windows host ports reconciled"
		if err != nil {
			kind, text = "engine.port_error", message
		}
		events.Publish(api.Event{Engine: "linux", Type: kind, Message: text})
		previousError = message
	}
	for ctx.Err() == nil {
		streamCtx, cancel := context.WithCancel(ctx)
		since := fmt.Sprintf("%d.%09d", last/int64(time.Second), last%int64(time.Second))
		stream := docker.Events(streamCtx, client.EventsListOptions{Since: since})
		retry := time.NewTimer(time.Hour)
		retry.Stop()
		var retryC <-chan time.Time
		retryDelay := time.Second
		reconcile := func() {
			check, stop := context.WithTimeout(ctx, 15*time.Second)
			err := ports.reconcile(check)
			stop()
			report(err)
			retry.Stop()
			retryC = nil
			if err != nil && ctx.Err() == nil {
				retry.Reset(retryDelay)
				retryC = retry.C
				retryDelay = min(30*time.Second, retryDelay*2)
			} else {
				retryDelay = time.Second
			}
		}
		reconcile()
		messages := stream.Messages
	streamLoop:
		for {
			select {
			case <-ctx.Done():
				break streamLoop
			case <-retryC:
				retryC = nil
				reconcile()
			case event, ok := <-messages:
				if !ok {
					messages = nil
					continue
				}
				if event.TimeNano > last {
					last = event.TimeNano
				}
				backoff = time.Second
				if linuxPortsEvent(string(event.Type), string(event.Action)) {
					reconcile()
				}
			case err := <-stream.Err:
				if err == nil {
					err = errors.New("Linux Docker event stream disconnected")
				}
				report(err)
				break streamLoop
			}
		}
		retry.Stop()
		cancel()
		ports.close()
		if ctx.Err() != nil {
			return
		}
		// The helper remains available while Resource Saver stops Docker. Clear
		// its public-port projection as soon as the event connection is lost.
		clear, stop := context.WithTimeout(ctx, 3*time.Second)
		if err := ports.publish(clear); err != nil {
			report(fmt.Errorf("clear disconnected Linux host-port state: %w", err))
		}
		stop()
		pause := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			pause.Stop()
			return
		case <-pause.C:
		}
		backoff = min(30*time.Second, backoff*2)
	}
}

func linuxPortsEvent(kind, action string) bool {
	if kind == "network" {
		return action == "connect" || action == "disconnect" || action == "destroy"
	}
	if kind == "container" {
		switch action {
		case "start", "stop", "die", "destroy", "restart", "pause", "unpause":
			return true
		}
	}
	return false
}
