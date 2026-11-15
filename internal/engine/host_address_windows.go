//go:build windows

package engine

import (
	"net/netip"

	"github.com/skyri/ucd/internal/platform"
)

func windowsHostIPv4() (netip.Addr, error) { return platform.HostIPv4() }
