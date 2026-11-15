//go:build !windows

package engine

import (
	"errors"
	"net/netip"
)

func windowsHostIPv4() (netip.Addr, error) {
	return netip.Addr{}, errors.New("Windows host aliases require a Windows controller")
}
