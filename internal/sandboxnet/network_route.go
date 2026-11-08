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

import "net/netip"

// routeDestination returns the bytes of addr for a lookup of its route,
// and whether they are IPv6: 4 bytes for an IPv4 address, IPv4-mapped ones
// included, or else 16.
func routeDestination(addr netip.Addr) (ip []byte, v6 bool) {
	addr = addr.Unmap()
	if addr.Is4() {
		b := addr.As4()
		return b[:], false
	}
	b := addr.As16()
	return b[:], true
}
