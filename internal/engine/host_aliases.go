package engine

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"path/filepath"
	"sort"
	"strings"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
	"github.com/skyri/ucd/internal/api"
	"github.com/skyri/ucd/internal/dnsfilter"
)

const maxHostsFile = 1 << 20

func (b *Backend) configureHostAliases(req api.Request) error {
	if b.id != "linux" {
		return errors.New("host alias DNS configuration is available only on Linux")
	}
	aliases, err := api.Decode[dnsfilter.HostAliases](req)
	if err != nil {
		return err
	}
	if !aliases.HostIPv4.Is4() || aliases.HostIPv4.IsUnspecified() || aliases.HostIPv4.IsLoopback() {
		return errors.New("Docker host alias requires a host-owned IPv4 address")
	}
	data, err := json.Marshal(aliases)
	if err != nil {
		return err
	}
	return writeConfigFile(filepath.Join(b.dataRoot, "config", "hostaliases.json"), append(data, '\n'))
}

// HostGateway returns the selected Windows network's gateway. Compose usually
// creates its own NAT network; the built-in nat gateway belongs to a different
// network and is not a usable host endpoint for those containers.
func (b *Backend) HostGateway(ctx context.Context, mode string) (netip.Addr, error) {
	if mode == "" || mode == "default" {
		mode = "nat"
	}
	result, err := b.docker.NetworkInspect(ctx, mode, client.NetworkInspectOptions{})
	if err != nil {
		return netip.Addr{}, fmt.Errorf("inspect Windows NAT gateway for Docker host aliases: %w", err)
	}
	// Transparent/overlay drivers retain their native DNS and gateway setup.
	// UCD's automatic Windows aliases are a NAT-network convenience.
	if result.Network.Driver != "nat" {
		return netip.Addr{}, nil
	}
	var gateway netip.Addr
	for _, config := range result.Network.IPAM.Config {
		if !config.Gateway.Is4() {
			continue
		}
		if gateway.IsValid() && gateway != config.Gateway {
			return netip.Addr{}, errors.New("Windows NAT has multiple IPv4 gateways; cannot select a Docker host alias address")
		}
		gateway = config.Gateway
	}
	if !gateway.IsValid() {
		return netip.Addr{}, errors.New("Windows NAT has no IPv4 gateway for Docker host aliases")
	}
	return gateway, nil
}

// Windows Docker currently retains ExtraHosts in inspection but does not write
// them into the container. Copying the file while it is stopped makes aliases
// available to the first application process, without an entrypoint wrapper.
func (b *Backend) prepareWindowsHostAliases(ctx context.Context, id string, host *container.HostConfig) error {
	if b.id != "windows" || (host != nil && host.NetworkMode == "none") {
		return nil
	}
	mode := ""
	if host != nil {
		mode = string(host.NetworkMode)
	}
	gateway, err := b.HostGateway(ctx, mode)
	if err != nil {
		return err
	}
	if !gateway.IsValid() {
		return nil
	}
	address, err := b.hostIPv4()
	if err != nil {
		return err
	}
	entries := []string{"host.docker.internal:" + address.String(), "gateway.docker.internal:" + gateway.String()}
	if host != nil {
		entries = append(entries, host.ExtraHosts...)
	}
	const hostsPath = `C:\Windows\System32\drivers\etc\hosts`
	result, err := b.docker.CopyFromContainer(ctx, id, client.CopyFromContainerOptions{SourcePath: hostsPath})
	if err != nil {
		return fmt.Errorf("read Windows container hosts file: %w", err)
	}
	defer result.Content.Close()
	reader := tar.NewReader(result.Content)
	header, err := reader.Next()
	if err != nil {
		return fmt.Errorf("read Windows container hosts archive: %w", err)
	}
	if (header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA) || header.Size < 0 || header.Size > maxHostsFile {
		return errors.New("Windows container hosts file must be a regular file no larger than 1 MiB")
	}
	data, err := io.ReadAll(io.LimitReader(reader, maxHostsFile+1))
	if err != nil {
		return fmt.Errorf("read Windows container hosts content: %w", err)
	}
	data, err = hostsWithAliases(data, entries, gateway)
	if err != nil {
		return err
	}
	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	updated := *header
	updated.Name, updated.Size = "hosts", int64(len(data))
	if err := writer.WriteHeader(&updated); err != nil {
		return fmt.Errorf("encode Windows container hosts header: %w", err)
	}
	if _, err := writer.Write(data); err != nil {
		return fmt.Errorf("encode Windows container hosts content: %w", err)
	}
	if err := writer.Close(); err != nil {
		return fmt.Errorf("finish Windows container hosts archive: %w", err)
	}
	if _, err := b.docker.CopyToContainer(ctx, id, client.CopyToContainerOptions{DestinationPath: `C:\Windows\System32\drivers\etc`, Content: &archive}); err != nil {
		return fmt.Errorf("write Windows container host aliases: %w", err)
	}
	return nil
}

func hostsWithAliases(data []byte, entries []string, gateway netip.Addr) ([]byte, error) {
	if len(data) > maxHostsFile || bytes.ContainsRune(data, '\x00') {
		return nil, errors.New("hosts file exceeds 1 MiB or contains NUL characters")
	}
	aliases := make(map[string]string, len(entries))
	for _, entry := range entries {
		name, address, ok := strings.Cut(entry, ":")
		if !ok || name == "" || strings.ContainsAny(name, " \t\r\n\x00#=") {
			return nil, fmt.Errorf("invalid Docker extra host %q", entry)
		}
		if address == "host-gateway" {
			address = gateway.String()
		}
		ip, err := netip.ParseAddr(address)
		if err != nil || ip.Zone() != "" {
			return nil, fmt.Errorf("Docker extra host %q must have an IP address or host-gateway", name)
		}
		aliases[strings.ToLower(strings.TrimSuffix(name, "."))] = ip.String()
	}
	const begin, end = "# BEGIN UCD DOCKER HOST ALIASES", "# END UCD DOCKER HOST ALIASES"
	var lines []string
	inBlock := false
	for _, line := range strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n") {
		switch strings.TrimSpace(line) {
		case begin:
			if inBlock {
				return nil, errors.New("hosts file has nested UCD alias blocks")
			}
			inBlock = true
			continue
		case end:
			if !inBlock {
				return nil, errors.New("hosts file has an unmatched UCD alias block")
			}
			inBlock = false
			continue
		}
		if inBlock {
			continue
		}
		body, comment, commented := strings.Cut(line, "#")
		fields := strings.Fields(body)
		if len(fields) > 1 {
			kept := fields[:1]
			for _, name := range fields[1:] {
				if _, replaced := aliases[strings.ToLower(strings.TrimSuffix(name, "."))]; !replaced {
					kept = append(kept, name)
				}
			}
			if len(kept) != len(fields) {
				line = ""
				if len(kept) > 1 {
					line = strings.Join(kept, "\t")
				}
				if commented {
					line += " #" + comment
				}
			}
		}
		lines = append(lines, line)
	}
	if inBlock {
		return nil, errors.New("hosts file has an unterminated UCD alias block")
	}
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	keys := make([]string, 0, len(aliases))
	for name := range aliases {
		keys = append(keys, name)
	}
	sort.Strings(keys)
	lines = append(lines, begin)
	for _, name := range keys {
		lines = append(lines, aliases[name]+"\t"+name)
	}
	lines = append(lines, end, "")
	result := []byte(strings.Join(lines, "\r\n"))
	if len(result) > maxHostsFile {
		return nil, errors.New("hosts file with Docker aliases exceeds 1 MiB")
	}
	return result, nil
}

func (b *Backend) finishContainerCreate(ctx context.Context, result client.ContainerCreateResult, err error, host *container.HostConfig) (client.ContainerCreateResult, error) {
	if err != nil {
		return result, err
	}
	if err := b.prepareWindowsHostAliases(ctx, result.ID, host); err != nil {
		return result, fmt.Errorf("container %s was created but Docker host aliases could not be configured: %w", result.ID, err)
	}
	return result, nil
}
