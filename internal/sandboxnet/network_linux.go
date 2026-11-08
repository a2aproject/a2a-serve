// Copyright 2026 The A2A Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package sandboxnet

import (
	"errors"
	"net"
	"net/netip"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// routeGetter returns the kernel's routes to ip, as [netlink.RouteGet]
// does.
type routeGetter func(ip net.IP) ([]netlink.Route, error)

// DefaultRoute implements [Network].
func (hostNetwork) DefaultRoute(addr netip.Addr) (bool, error) {
	return viaInternetRoute(netlink.RouteGet, addr)
}

// CheckInternetRoute checks that this host has a route to the internet,
// without which the sandbox reaches no public host.
func CheckInternetRoute() error {
	if _, ok, err := routeTo(netlink.RouteGet, internetProbe4); err != nil || !ok {
		return errors.Join(errNoInternetRoute, err)
	}
	return nil
}

// viaInternetRoute reports whether the kernel routes addr as it routes a
// public probe address: through the same table, device and gateway. This
// holds for the internet behind a full-tunnel VPN or a Tailscale exit
// node too, and fails for the LAN, a split-tunnel VPN and bridges.
func viaInternetRoute(get routeGetter, addr netip.Addr) (bool, error) {
	addr = addr.Unmap()
	probe := internetProbe4
	if addr.Is6() {
		probe = internetProbe6
	}
	got, ok, err := routeTo(get, addr)
	if err != nil || !ok {
		return false, err
	}
	want, ok, err := routeTo(get, probe)
	if err != nil || !ok {
		return false, err
	}
	return got.Table == want.Table && got.LinkIndex == want.LinkIndex && got.Gw.Equal(want.Gw), nil
}

// routeTo returns the route the kernel picks for addr, or false when it
// has none.
func routeTo(get routeGetter, addr netip.Addr) (netlink.Route, bool, error) {
	routes, err := get(addr.AsSlice())
	if errors.Is(err, unix.ENETUNREACH) || errors.Is(err, unix.EHOSTUNREACH) {
		return netlink.Route{}, false, nil
	}
	if err != nil {
		return netlink.Route{}, false, err
	}
	if len(routes) == 0 {
		return netlink.Route{}, false, nil
	}
	return routes[0], true, nil
}
