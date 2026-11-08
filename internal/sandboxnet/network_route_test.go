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
	"net/netip"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestRouteDestination(t *testing.T) {
	t.Parallel()
	tests := []struct {
		addr   string
		wantIP []byte
		wantV6 bool
	}{
		{addr: "8.8.8.8", wantIP: []byte{8, 8, 8, 8}},
		{addr: "::ffff:192.168.1.5", wantIP: []byte{192, 168, 1, 5}},
		{addr: "2606:4700::1111", wantIP: netip.MustParseAddr("2606:4700::1111").AsSlice(), wantV6: true},
		{addr: "fe80::1", wantIP: netip.MustParseAddr("fe80::1").AsSlice(), wantV6: true},
	}
	for _, tc := range tests {
		ip, v6 := routeDestination(netip.MustParseAddr(tc.addr))
		if diff := cmp.Diff(tc.wantIP, ip); diff != "" || v6 != tc.wantV6 {
			t.Errorf("routeDestination(%s) = %v, %v, want %v, %v", tc.addr, ip, v6, tc.wantIP, tc.wantV6)
		}
	}
}
