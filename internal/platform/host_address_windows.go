//go:build windows

package platform

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"syscall"

	"golang.org/x/sys/windows"
)

// HostIPv4 selects a local address on Windows' preferred outbound interface.
// GetBestInterfaceEx only queries the routing table; no packet is transmitted.
// The WSL mirrored host-address loopback option makes this address reachable
// from Linux, unlike Windows' excluded HNS NAT adapters.
func HostIPv4() (netip.Addr, error) {
	var index uint32
	if err := windows.GetBestInterfaceEx(&windows.SockaddrInet4{Addr: [4]byte{1, 1, 1, 1}}, &index); err != nil {
		return netip.Addr{}, fmt.Errorf("select Windows host interface for Docker alias: %w", err)
	}
	iface, err := net.InterfaceByIndex(int(index))
	if err != nil {
		return netip.Addr{}, fmt.Errorf("find Windows host interface: %w", err)
	}
	addresses, err := iface.Addrs()
	if err != nil {
		return netip.Addr{}, fmt.Errorf("read Windows host interface addresses: %w", err)
	}
	for _, address := range addresses {
		prefix, err := netip.ParsePrefix(address.String())
		if err == nil && prefix.Addr().Is4() && !prefix.Addr().IsLoopback() && !prefix.Addr().IsLinkLocalUnicast() && !prefix.Addr().IsUnspecified() {
			return prefix.Addr(), nil
		}
	}
	return netip.Addr{}, errors.New("Windows preferred interface has no usable IPv4 address for host.docker.internal")
}

// HostAddressChanges watches Windows address and route notifications. The caller
// must stop the registrations after its receiver exits. Bursts are coalesced;
// a notification means to take a fresh routing snapshot, not count changes.
func HostAddressChanges() (<-chan struct{}, func() error, error) {
	changed := make(chan struct{}, 1)
	callback := syscall.NewCallback(func(_, _, _ uintptr) uintptr {
		select {
		case changed <- struct{}{}:
		default:
		}
		return 0
	})
	var addressHandle, routeHandle windows.Handle
	if err := windows.NotifyUnicastIpAddressChange(windows.AF_INET, callback, nil, false, &addressHandle); err != nil {
		return nil, nil, fmt.Errorf("watch Windows IPv4 addresses: %w", err)
	}
	if err := windows.NotifyRouteChange2(windows.AF_INET, callback, nil, false, &routeHandle); err != nil {
		return nil, nil, errors.Join(fmt.Errorf("watch Windows routes: %w", err), windows.CancelMibChangeNotify2(addressHandle))
	}
	return changed, func() error {
		return errors.Join(windows.CancelMibChangeNotify2(addressHandle), windows.CancelMibChangeNotify2(routeHandle))
	}, nil
}
