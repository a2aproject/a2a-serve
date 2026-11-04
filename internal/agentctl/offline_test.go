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

	"github.com/a2aproject/a2a-serve/internal/agentdir"
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

func TestOthersAllowedOnRelay(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		issuer  string
		secrets agentdir.Secrets
		want    []string
	}{
		{name: "token-only relay", secrets: agentdir.Secrets{agentdir.SecretRelayToken: "tok"}, want: []string{"everyone on the relay"}},
		{name: "sign-in relay joined with a token", issuer: "https://accounts.example.com", secrets: agentdir.Secrets{agentdir.SecretRelayToken: "tok"}},
		{name: "relay joined by sign-in", secrets: agentdir.Secrets{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := &agentdir.Config{Name: "coder", Harness: "opencode", Network: agentdir.NetworkConfig{Private: fakeRelay(t, tc.issuer)}}
			d := newAgent(t, cfg, tc.secrets)
			if diff := cmp.Diff(tc.want, othersAllowed(t.Context(), d, cfg)); diff != "" {
				t.Fatalf("othersAllowed() wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}
