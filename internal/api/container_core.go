package api

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
)

// LinuxPortBinding joins a requested Windows host binding to Docker's private
// Linux loopback port. These records travel only over the authenticated helper
// connection. Index identifies the binding in the container's sorted metadata.
type LinuxPortBinding struct {
	Container string `json:"container"`
	Index     int    `json:"index"`
	Port      Port   `json:"port"`
	RelayPort uint16 `json:"relayPort"`
}

type LinuxPortBindings struct {
	Bindings []LinuxPortBinding `json:"bindings"`
}

// ContainerPublishedPorts returns explicit Docker bindings with UCD's default
// loopback address. Dynamic host ports are represented by Published == 0.
func ContainerPublishedPorts(p ContainerCreateParams) ([]Port, error) {
	var result []Port
	if p.HostConfig == nil {
		return result, nil
	}
	for target, bindings := range p.HostConfig.PortBindings {
		protocol := strings.ToLower(string(target.Proto()))
		if !target.IsValid() || (protocol != "tcp" && protocol != "udp") {
			return nil, Fail("invalid_argument", fmt.Sprintf("invalid published target port %s; use TCP or UDP", target.String()))
		}
		for _, binding := range bindings {
			host := "127.0.0.1"
			if binding.HostIP.IsValid() {
				if binding.HostIP.Zone() != "" {
					return nil, Fail("invalid_argument", "published host IP cannot have a zone")
				}
				host = binding.HostIP.Unmap().String()
			}
			var published uint64
			if binding.HostPort != "" {
				var err error
				published, err = strconv.ParseUint(binding.HostPort, 10, 16)
				if err != nil {
					return nil, Fail("invalid_argument", "published host port must be a single port from 0 to 65535")
				}
			}
			result = append(result, Port{HostIP: host, Published: uint16(published), Target: target.Num(), Protocol: protocol})
		}
	}
	sort.Slice(result, func(i, j int) bool {
		a, b := result[i], result[j]
		if a.Target != b.Target {
			return a.Target < b.Target
		}
		if a.Protocol != b.Protocol {
			return a.Protocol < b.Protocol
		}
		if a.HostIP != b.HostIP {
			return a.HostIP < b.HostIP
		}
		return a.Published < b.Published
	})
	return result, nil
}

type ContainerCreateParams struct {
	Name             string                    `json:"name,omitempty"`
	Config           *container.Config         `json:"config"`
	HostConfig       *container.HostConfig     `json:"hostConfig,omitempty"`
	NetworkingConfig *network.NetworkingConfig `json:"networkingConfig,omitempty"`
}

type ContainerCreateResult struct {
	ID       string   `json:"id"`
	Warnings []string `json:"warnings,omitempty"`
}

type ContainerOperationParams struct {
	ID            string                   `json:"id"`
	Name          string                   `json:"name,omitempty"`
	Resources     *container.Resources     `json:"resources,omitempty"`
	RestartPolicy *container.RestartPolicy `json:"restartPolicy,omitempty"`
	PSArgs        string                   `json:"psArgs,omitempty"`
	Reference     string                   `json:"reference,omitempty"`
	Author        string                   `json:"author,omitempty"`
	Comment       string                   `json:"comment,omitempty"`
	Pause         *bool                    `json:"pause,omitempty"`
	Changes       []string                 `json:"changes,omitempty"`
}

type ContainerLogParams struct {
	ID         string `json:"id"`
	Tail       int    `json:"tail,omitempty"`
	Since      string `json:"since,omitempty"`
	Until      string `json:"until,omitempty"`
	Timestamps bool   `json:"timestamps,omitempty"`
	Stdout     *bool  `json:"stdout,omitempty"`
	Stderr     *bool  `json:"stderr,omitempty"`
}

type ExecParams struct {
	ID         string   `json:"id,omitempty"`
	SessionID  string   `json:"sessionID,omitempty"`
	Command    []string `json:"command,omitempty"`
	User       string   `json:"user,omitempty"`
	WorkingDir string   `json:"workingDir,omitempty"`
	Env        []string `json:"env,omitempty"`
	Rows       uint     `json:"rows,omitempty"`
	Cols       uint     `json:"cols,omitempty"`
	MaxBytes   int      `json:"maxBytes,omitempty"`
	Data       []byte   `json:"data,omitempty"`
}

type ExecOpenResult struct {
	SessionID string `json:"sessionID"`
}
type ExecReadResult struct {
	Data     []byte `json:"data"`
	EOF      bool   `json:"eof"`
	ExitCode *int   `json:"exitCode,omitempty"`
}

type ContainerFileParams struct {
	ID        string `json:"id"`
	Path      string `json:"path"`
	Data      []byte `json:"data,omitempty"`
	Mode      uint32 `json:"mode,omitempty"`
	Recursive bool   `json:"recursive,omitempty"`
	Confirmed bool   `json:"confirmed,omitempty"`
}

type ContainerFilePrepareResult struct {
	Prepared  bool `json:"prepared"`
	Restarted bool `json:"restarted"`
}

type ContainerFileEntry struct {
	Name     string    `json:"name"`
	Path     string    `json:"path"`
	Type     string    `json:"type"`
	Size     int64     `json:"size"`
	Modified time.Time `json:"modified"`
}
type ContainerFileList struct {
	Entries []ContainerFileEntry `json:"entries"`
}
type BinaryResult struct {
	Data []byte `json:"data"`
	Name string `json:"name,omitempty"`
}
