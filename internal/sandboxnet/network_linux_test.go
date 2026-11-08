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
	"net"
	"net/netip"
	"testing"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// fakeRoutes routes each address of its map, and the rest as other.
type fakeRoutes struct {
	routes map[string]netlink.Route
	other  *netlink.Route
}

func (f fakeRoutes) get(ip net.IP) ([]netlink.Route, error) {
	if r, ok := f.routes[ip.String()]; ok {
		return []netlink.Route{r}, nil
	}
	if f.other != nil {
		return []netlink.Route{*f.other}, nil
	}
	return nil, unix.ENETUNREACH
}

func TestViaInternetRoute(t *testing.T) {
	t.Parallel()
	const (
		eth0, tun0, tailscale0, wg0 = 2, 9, 5, 7
		main, tailnet, wireguard    = unix.RT_TABLE_MAIN, 52, 51820
	)
	router, vpn := net.ParseIP("192.168.1.1"), net.ParseIP("10.8.0.1")
	lan := netlink.Route{Table: main, LinkIndex: eth0}
	tests := []struct {
		name   string
		routes fakeRoutes
		addr   string
		want   bool
	}{
		{name: "internet through the router", routes: fakeRoutes{other: &netlink.Route{Table: main, LinkIndex: eth0, Gw: router}}, addr: "1.1.1.1", want: true},
		{name: "LAN host", routes: fakeRoutes{routes: map[string]netlink.Route{"192.168.1.5": lan}, other: &netlink.Route{Table: main, LinkIndex: eth0, Gw: router}}, addr: "192.168.1.5"},
		{name: "Tailscale exit node", routes: fakeRoutes{other: &netlink.Route{Table: tailnet, LinkIndex: tailscale0}}, addr: "1.1.1.1", want: true},
		{name: "LAN host with an exit node", routes: fakeRoutes{routes: map[string]netlink.Route{"192.168.1.5": lan}, other: &netlink.Route{Table: tailnet, LinkIndex: tailscale0}}, addr: "192.168.1.5"},
		{name: "wg-quick full tunnel", routes: fakeRoutes{other: &netlink.Route{Table: wireguard, LinkIndex: wg0}}, addr: "140.82.121.4", want: true},
		{name: "OpenVPN redirect-gateway def1", routes: fakeRoutes{other: &netlink.Route{Table: main, LinkIndex: tun0, Gw: vpn}}, addr: "140.82.121.4", want: true},
		{name: "public network of a split-tunnel VPN", routes: fakeRoutes{routes: map[string]netlink.Route{"203.0.113.5": {Table: main, LinkIndex: tun0, Gw: vpn}}, other: &netlink.Route{Table: main, LinkIndex: eth0, Gw: router}}, addr: "203.0.113.5"},
		{name: "IPv6 without a route", routes: fakeRoutes{routes: map[string]netlink.Route{"8.8.8.8": {Table: main, LinkIndex: eth0, Gw: router}}}, addr: "2606:4700::1"},
		{name: "no internet route", routes: fakeRoutes{routes: map[string]netlink.Route{"1.1.1.1": lan}}, addr: "1.1.1.1"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := viaInternetRoute(tc.routes.get, netip.MustParseAddr(tc.addr))
			if err != nil {
				t.Fatalf("viaInternetRoute(%s) error = %v", tc.addr, err)
			}
			if got != tc.want {
				t.Fatalf("viaInternetRoute(%s) = %v, want %v", tc.addr, got, tc.want)
			}
		})
	}
}
