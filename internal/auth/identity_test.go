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

package auth

import (
	"testing"

	"github.com/a2aproject/a2a-serve/internal/network"
)

func TestIdentityDescribe(t *testing.T) {
	t.Parallel()
	relay := func(id Identity) Identity {
		id.Network, id.Provider = network.ListenerPrivate, network.ProviderAgentMesh
		return id
	}
	tests := []struct {
		name string
		id   Identity
		want string
	}{
		{name: "machine", id: relay(Identity{Node: "12D3KooW"}), want: "the machine 12D3KooW on the relay"},
		{name: "person on the tailnet", id: Identity{Network: network.ListenerPrivate, Provider: network.ProviderTailscale, Email: "anna@example.com"}, want: "anna@example.com on the tailnet"},
		{name: "agent of a machine", id: relay(Identity{Node: "12D3KooW", Hops: 1}), want: "the agent on the machine 12D3KooW on the relay"},
		{name: "agent of a person", id: relay(Identity{Node: "12D3KooW", Email: "anna@example.com", EmailSource: EmailFromNetwork, Hops: 1}), want: "an agent of anna@example.com on the relay"},
		{name: "agent with a pairing", id: Identity{Network: network.ListenerPublic, Pairing: "anna", Hops: 1}, want: "an agent with the pairing anna on the public URL"},
		{name: "own agent", id: relay(Identity{Node: "12D3KooW", Self: true, Hops: 1}), want: "one of your agents on the relay"},
		{name: "unknown agent", id: Identity{Network: network.ListenerPublic, Hops: 2}, want: "an unknown agent on the public URL"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.id.Describe(); got != tc.want {
				t.Fatalf("Identity.Describe() = %q, want %q", got, tc.want)
			}
		})
	}
}
