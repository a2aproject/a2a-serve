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

package network

import (
	"errors"
	"net/netip"
	"testing"

	"github.com/google/go-cmp/cmp"
	"tailscale.com/tailcfg"
)

func TestTailnetPeer(t *testing.T) {
	t.Parallel()
	local := []netip.Addr{netip.MustParseAddr("127.0.0.1"), netip.MustParseAddr("10.0.0.5"), netip.MustParseAddr("100.64.0.1"), netip.MustParseAddr("fd7a:115c:a1e0::1")}
	tests := []struct {
		name     string
		node     tailnetNode
		self     tailcfg.UserID
		localErr error
		want     *Peer
	}{
		{
			name: "own device",
			node: tailnetNode{id: "n1", machine: "laptop", login: "anna@example.com", user: 7, addrs: []netip.Addr{netip.MustParseAddr("100.64.0.2")}},
			self: 7,
			want: &Peer{Node: "n1", Machine: "laptop", Email: "anna@example.com", Self: true},
		},
		{
			name: "this machine",
			node: tailnetNode{id: "n2", machine: "desktop", login: "anna@example.com", user: 7, addrs: []netip.Addr{netip.MustParseAddr("100.64.0.1")}},
			self: 7,
			want: &Peer{Node: "n2", Machine: "desktop"},
		},
		{
			name: "this machine by its IPv6 address",
			node: tailnetNode{id: "n2", machine: "desktop", login: "anna@example.com", user: 7, addrs: []netip.Addr{netip.MustParseAddr("100.64.0.9"), netip.MustParseAddr("fd7a:115c:a1e0::1")}},
			self: 7,
			want: &Peer{Node: "n2", Machine: "desktop"},
		},
		{
			name:     "unknown addresses of this machine",
			node:     tailnetNode{id: "n1", machine: "laptop", login: "anna@example.com", user: 7, addrs: []netip.Addr{netip.MustParseAddr("100.64.0.2")}},
			self:     7,
			localErr: errors.New("netlink failed"),
			want:     &Peer{Node: "n1", Machine: "laptop"},
		},
		{
			name: "local address outside the tailnet ranges",
			node: tailnetNode{id: "n1", machine: "laptop", login: "anna@example.com", user: 7, addrs: []netip.Addr{netip.MustParseAddr("10.0.0.5")}},
			self: 7,
			want: &Peer{Node: "n1", Machine: "laptop", Email: "anna@example.com", Self: true},
		},
		{
			name: "server without its own user",
			node: tailnetNode{id: "n1", machine: "laptop", login: "anna@example.com", addrs: []netip.Addr{netip.MustParseAddr("100.64.0.2")}},
			want: &Peer{Node: "n1", Machine: "laptop", Email: "anna@example.com"},
		},
		{
			name: "another user",
			node: tailnetNode{id: "n3", machine: "bob-laptop", login: "bob@example.com", user: 8, addrs: []netip.Addr{netip.MustParseAddr("100.64.0.3")}},
			self: 7,
			want: &Peer{Node: "n3", Machine: "bob-laptop", Email: "bob@example.com"},
		},
		{
			name: "tagged device",
			node: tailnetNode{id: "n4", machine: "ci", tagged: true, login: "anna@example.com", user: 7, addrs: []netip.Addr{netip.MustParseAddr("100.64.0.4")}},
			self: 7,
			want: &Peer{Node: "n4", Machine: "ci"},
		},
		{
			name: "login without an email",
			node: tailnetNode{id: "n5", machine: "phone", login: "anna@github", user: 7, addrs: []netip.Addr{netip.MustParseAddr("100.64.0.5")}},
			self: 7,
			want: &Peer{Node: "n5", Machine: "phone", Self: true},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := tailnetPeer(tt.node, tt.self, local, tt.localErr)
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Fatalf("tailnetPeer() wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}
