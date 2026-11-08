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
)

// hostNetwork is the network of the host the server runs on.
type hostNetwork struct{}

var _ Network = hostNetwork{}

// OwnAddr implements [Network].
func (hostNetwork) OwnAddr(addr netip.Addr) (bool, error) {
	prefixes, err := interfacePrefixes()
	if err != nil {
		return false, err
	}
	for _, p := range prefixes {
		if p.Addr() == addr.Unmap() {
			return true, nil
		}
	}
	return false, nil
}

// interfacePrefixes returns the addresses of this host's interfaces with
// the prefixes of their links.
func interfacePrefixes() ([]netip.Prefix, error) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil, err
	}
	var out []netip.Prefix
	for _, a := range addrs {
		p, err := netip.ParsePrefix(a.String())
		if err != nil {
			continue
		}
		if p.Addr().Is4In6() {
			p = netip.PrefixFrom(p.Addr().Unmap(), max(p.Bits()-96, 0))
		}
		out = append(out, p)
	}
	return out, nil
}
