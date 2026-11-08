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
	"net/netip"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// DefaultRoute implements [Network]: the kernel's route to addr must come
// from the main table and leave through the gateway and device of one of
// its default routes.
func (hostNetwork) DefaultRoute(addr netip.Addr) (bool, error) {
	addr = addr.Unmap()
	routes, err := netlink.RouteGet(addr.AsSlice())
	if errors.Is(err, unix.ENETUNREACH) || errors.Is(err, unix.EHOSTUNREACH) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if len(routes) == 0 {
		return false, nil
	}
	got := routes[0]
	if got.Gw == nil || got.Table != 0 && got.Table != unix.RT_TABLE_MAIN {
		return false, nil
	}
	family := netlink.FAMILY_V4
	if addr.Is6() {
		family = netlink.FAMILY_V6
	}
	main, err := netlink.RouteListFiltered(family, &netlink.Route{Table: unix.RT_TABLE_MAIN}, netlink.RT_FILTER_TABLE)
	if err != nil {
		return false, err
	}
	for _, r := range main {
		if r.Dst != nil {
			if ones, _ := r.Dst.Mask.Size(); ones != 0 {
				continue
			}
		}
		if r.LinkIndex == got.LinkIndex && r.Gw.Equal(got.Gw) {
			return true, nil
		}
		for _, nh := range r.MultiPath {
			if nh.LinkIndex == got.LinkIndex && nh.Gw.Equal(got.Gw) {
				return true, nil
			}
		}
	}
	return false, nil
}
