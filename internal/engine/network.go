package engine

import (
	"context"
	"fmt"

	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
	"github.com/skyri/ucd/internal/api"
)

func (b *Backend) networkCall(ctx context.Context, req api.Request) (any, error) {
	p, err := api.Decode[api.NetworkParams](req)
	if err != nil {
		return nil, err
	}
	if req.Method == "network.list" {
		result, err := b.docker.NetworkList(ctx, client.NetworkListOptions{})
		if err != nil {
			return nil, fmt.Errorf("list networks: %w", err)
		}
		out := make([]api.Network, 0, len(result.Items))
		for _, item := range result.Items {
			out = append(out, api.Network{Summary: item, Engine: b.id})
		}
		return out, nil
	}
	id := p.ID
	if id == "" {
		id = p.Name
	}
	if id == "" {
		return nil, api.Fail("invalid_argument", "network name or ID is required")
	}
	switch req.Method {
	case "network.create":
		create := network.CreateRequest{
			Name: id, Driver: p.Driver, EnableIPv4: p.EnableIPv4, EnableIPv6: p.EnableIPv6,
			IPAM: p.IPAM, Internal: p.Internal, Attachable: p.Attachable,
			Options: p.Options, Labels: p.Labels,
		}
		if b.id == "linux" {
			if err := ApplyNetworkDefaults(&create, b.RuntimeConfiguration().Network); err != nil {
				return nil, api.Fail("invalid_argument", err.Error())
			}
		}
		result, err := b.docker.NetworkCreate(ctx, id, client.NetworkCreateOptions{
			Driver: create.Driver, EnableIPv4: create.EnableIPv4, EnableIPv6: create.EnableIPv6,
			IPAM: create.IPAM, Internal: create.Internal, Attachable: create.Attachable,
			Options: create.Options, Labels: create.Labels,
		})
		if err != nil {
			return nil, fmt.Errorf("create network: %w", err)
		}
		return map[string]any{"id": result.ID, "warnings": result.Warning}, nil
	case "network.inspect":
		result, err := b.docker.NetworkInspect(ctx, id, client.NetworkInspectOptions{})
		if err != nil {
			return nil, fmt.Errorf("inspect network: %w", err)
		}
		return result.Network, nil
	case "network.remove":
		_, err := b.docker.NetworkRemove(ctx, id, client.NetworkRemoveOptions{})
		if err != nil {
			return nil, fmt.Errorf("remove network: %w", err)
		}
		return nil, nil
	case "network.connect":
		if p.Container == "" {
			return nil, api.Fail("invalid_argument", "container ID is required")
		}
		_, err := b.docker.NetworkConnect(ctx, id, client.NetworkConnectOptions{Container: p.Container, EndpointConfig: &network.EndpointSettings{Aliases: p.Aliases}})
		if err != nil {
			return nil, fmt.Errorf("connect network: %w", err)
		}
		return nil, nil
	case "network.disconnect":
		if p.Container == "" {
			return nil, api.Fail("invalid_argument", "container ID is required")
		}
		_, err := b.docker.NetworkDisconnect(ctx, id, client.NetworkDisconnectOptions{Container: p.Container, Force: p.Force})
		if err != nil {
			return nil, fmt.Errorf("disconnect network: %w", err)
		}
		return nil, nil
	default:
		return nil, api.Fail("invalid_argument", "unknown network method: "+req.Method)
	}
}
