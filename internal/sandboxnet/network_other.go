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

//go:build !linux

package sandboxnet

import "net/netip"

// DefaultRoute implements [Network]. Without the routing table, it counts
// the addresses on the links of this host's interfaces as private, and
// misses networks that a VPN routes.
func (hostNetwork) DefaultRoute(addr netip.Addr) (bool, error) {
	prefixes, err := interfacePrefixes()
	if err != nil {
		return false, err
	}
	for _, p := range prefixes {
		if p.Contains(addr.Unmap()) {
			return false, nil
		}
	}
	return true, nil
}
