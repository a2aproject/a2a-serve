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

package agentctl

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/access"
	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/network"
)

// fakeRelay serves a relay's /info, whose protobuf names the identity
// provider in field 1 when the relay admits people by sign-in.
func fakeRelay(t *testing.T, issuer string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if issuer == "" {
			return
		}
		if _, err := w.Write(append([]byte{0x0a, byte(len(issuer))}, issuer...)); err != nil {
			t.Errorf("Write() error = %v", err)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestNetworkOfRelay(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		issuer     string
		secrets    agentdir.Secrets
		wantSignIn bool
	}{
		{name: "token-only relay", secrets: agentdir.Secrets{agentdir.SecretRelayToken: "tok"}},
		{name: "sign-in relay joined with a token", issuer: "https://accounts.example.com", secrets: agentdir.Secrets{agentdir.SecretRelayToken: "tok"}, wantSignIn: true},
		{name: "relay joined by sign-in", secrets: agentdir.Secrets{}, wantSignIn: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := &agentdir.Config{Name: "coder", Harness: "opencode", Network: agentdir.NetworkConfig{Private: fakeRelay(t, tc.issuer)}}
			d := newAgent(t, cfg, tc.secrets)
			want := access.Network{Private: network.ProviderRelay, SignIn: tc.wantSignIn}
			if diff := cmp.Diff(want, networkOf(t.Context(), d, cfg)); diff != "" {
				t.Fatalf("networkOf() wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}
