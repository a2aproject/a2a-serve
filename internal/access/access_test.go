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

package access

import (
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/network"
	"github.com/a2aproject/a2a-serve/internal/store"
)

func TestPolicy(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		settings agentdir.PolicyConfig
		want     auth.Policy
	}{
		{name: "defaults", want: auth.Policy{Authenticated: []string{"private"}}},
		{name: "closed private network", settings: agentdir.PolicyConfig{PrivateAllowIncoming: agentdir.AccessPaired}, want: auth.Policy{}},
		{
			name: "public URL open to anyone", settings: agentdir.PolicyConfig{PublicAllowIncoming: agentdir.AccessAnyone},
			want: auth.Policy{Authenticated: []string{"private"}, Anonymous: []string{"anyone"}},
		},
		{
			name:     "emails",
			settings: agentdir.PolicyConfig{PrivateAllowIncoming: agentdir.AccessPaired, AllowEmails: []string{"*@example.com"}},
			want:     auth.Policy{Authenticated: []string{"email:*@example.com"}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if diff := cmp.Diff(tc.want, Policy(tc.settings)); diff != "" {
				t.Fatalf("Policy() wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestNetworkOf(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		cfg  agentdir.NetworkConfig
		want Network
	}{
		{name: "none"},
		{name: "tailscale", cfg: agentdir.NetworkConfig{Private: "tailscale", Public: "tailscale"}, want: Network{Private: network.ProviderTailscale, Public: true}},
		{name: "relay", cfg: agentdir.NetworkConfig{Private: "https://relay.example.com"}, want: Network{Private: network.ProviderRelay}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if diff := cmp.Diff(tc.want, NetworkOf(&agentdir.Config{Network: tc.cfg})); diff != "" {
				t.Fatalf("NetworkOf() wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestCountPairings(t *testing.T) {
	t.Parallel()
	got := CountPairings([]store.Pairing{
		{Name: auth.LocalPairing, Role: auth.RoleOwner},
		{Name: "phone", Role: auth.RoleOwner},
		{Name: "anna", Role: auth.RoleAuthenticated},
	})
	if diff := cmp.Diff(Pairings{All: 2, Owners: 1}, got); diff != "" {
		t.Fatalf("CountPairings() wrong result (-want +got) diff = %s", diff)
	}
}

func TestDescribe(t *testing.T) {
	t.Parallel()
	tailnet := Network{Private: network.ProviderTailscale, Public: true}
	tests := []struct {
		name     string
		settings agentdir.PolicyConfig
		net      Network
		pairings Pairings
		want     *Description
	}{
		{
			name: "this machine only",
			want: &Description{
				Owner: []string{"self"}, Authenticated: []string{"private"}, Anonymous: []string{},
				Summary: []string{"you: this machine", "nobody else"},
			},
		},
		{
			name: "tailnet with a public URL", settings: agentdir.PolicyConfig{AllowEmails: []string{"*@example.com"}}, net: tailnet, pairings: Pairings{All: 2, Owners: 1},
			want: &Description{
				PrivateAllowIncoming: "everyone", PublicAllowIncoming: "paired",
				Owner: []string{"self"}, Authenticated: []string{"private", "email:*@example.com"}, Anonymous: []string{},
				Pairings: 2, OwnerPairings: 1, Others: []string{"everyone on the tailnet", "emails matching *@example.com"},
				Summary: []string{
					"you: this machine and your other tailnet devices that don't run an agent",
					"everyone on the tailnet, emails matching *@example.com, 2 pairings (1 owner)",
					"the public URL needs a pairing or Google sign-in",
				},
			},
		},
		{
			name: "closed tailnet, public URL open to anyone", settings: agentdir.PolicyConfig{PrivateAllowIncoming: "paired", PublicAllowIncoming: "anyone"}, net: tailnet, pairings: Pairings{All: 1},
			want: &Description{
				PrivateAllowIncoming: "paired", PublicAllowIncoming: "anyone",
				Owner: []string{"self"}, Authenticated: []string{}, Anonymous: []string{"anyone"},
				Pairings: 1, Others: []string{"anyone on the public URL"},
				Summary: []string{
					"you: this machine and your other tailnet devices that don't run an agent",
					"anyone on the public URL, 1 pairing",
					"the tailnet needs a pairing",
					"the public URL is open to anyone",
				},
			},
		},
		{
			name: "anyone with a messenger and no public URL", settings: agentdir.PolicyConfig{PublicAllowIncoming: "anyone"}, net: Network{Messenger: true},
			want: &Description{
				Owner: []string{"self"}, Authenticated: []string{"private"}, Anonymous: []string{"anyone"},
				Others:  []string{"anyone in a direct chat"},
				Summary: []string{"you: this machine", "anyone in a direct chat"},
			},
		},
		{
			name: "anyone without a public URL or messenger", settings: agentdir.PolicyConfig{PublicAllowIncoming: "anyone"},
			want: &Description{
				Owner: []string{"self"}, Authenticated: []string{"private"}, Anonymous: []string{"anyone"},
				Others:  []string{"anyone (no public URL or messenger is set up yet)"},
				Summary: []string{"you: this machine", "anyone (no public URL or messenger is set up yet)"},
			},
		},
		{
			name: "relay", net: Network{Private: network.ProviderRelay, Public: true},
			want: &Description{
				PrivateAllowIncoming: "everyone", PublicAllowIncoming: "paired",
				Owner: []string{"self"}, Authenticated: []string{"private"}, Anonymous: []string{},
				Others: []string{"everyone on the relay"},
				Summary: []string{
					"you: this machine and your devices on the relay",
					"everyone on the relay",
					"the public URL needs a pairing",
				},
			},
		},
		{
			name: "closed relay", settings: agentdir.PolicyConfig{PrivateAllowIncoming: "paired"}, net: Network{Private: network.ProviderRelay},
			want: &Description{
				PrivateAllowIncoming: "paired",
				Owner:                []string{"self"}, Authenticated: []string{}, Anonymous: []string{},
				Summary: []string{
					"you: this machine and your devices on the relay",
					"nobody else",
					"the relay needs a pairing",
				},
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if diff := cmp.Diff(tc.want, Describe(tc.settings, tc.net, tc.pairings)); diff != "" {
				t.Fatalf("Describe() wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}
