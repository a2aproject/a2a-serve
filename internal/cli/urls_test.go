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

package cli

import (
	"testing"

	"github.com/a2aproject/a2a-serve/internal/controlapi"
)

func TestStatusURL(t *testing.T) {
	t.Parallel()
	all := &controlapi.Status{Name: "coder", Local: "http://127.0.0.1:4000", Private: "http://coder.tail.ts.net", Public: "https://coder.example.com"}
	localOnlyStatus := &controlapi.Status{Name: "coder", Local: "http://127.0.0.1:4000"}
	tests := []struct {
		name    string
		status  *controlapi.Status
		scope   string
		want    string
		wantErr string
	}{
		{name: "best is public", status: all, want: "https://coder.example.com"},
		{name: "best is private without public", status: &controlapi.Status{Local: "http://127.0.0.1:4000", Private: "https://coder.tail.ts.net"}, want: "https://coder.tail.ts.net"},
		{name: "best is local without networks", status: localOnlyStatus, want: "http://127.0.0.1:4000"},
		{name: "local", status: all, scope: scopeLocal, want: "http://127.0.0.1:4000"},
		{name: "private", status: all, scope: scopePrivate, want: "http://coder.tail.ts.net"},
		{name: "public", status: all, scope: scopePublic, want: "https://coder.example.com"},
		{name: "no private URL", status: localOnlyStatus, scope: scopePrivate, wantErr: "coder has no private URL; add one with `a2a-serve config set --private tailscale` (or a relay URL), then `a2a-serve restart`"},
		{name: "no public URL", status: localOnlyStatus, scope: scopePublic, wantErr: "coder has no public URL; add one with `a2a-serve config set --public tailscale` (or a relay URL), then `a2a-serve restart`"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := statusURL(tc.status, tc.scope)
			if tc.wantErr != "" {
				if err == nil || err.Error() != tc.wantErr {
					t.Fatalf("statusURL(%q) error = %v, want %q", tc.scope, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("statusURL(%q) error = %v, want nil", tc.scope, err)
			}
			if got != tc.want {
				t.Fatalf("statusURL(%q) = %q, want %q", tc.scope, got, tc.want)
			}
		})
	}
}

func TestStatusInspectorURL(t *testing.T) {
	t.Parallel()
	const plainHTTP = "the inspector isn't served on http://192.168.1.5:4000, because its login link and session would cross the network unencrypted; run `a2a-serve inspect --url local` on the agent's machine"
	tests := []struct {
		name    string
		status  *controlapi.Status
		scope   string
		want    string
		wantErr string
	}{
		{name: "local", status: &controlapi.Status{Local: "http://127.0.0.1:4000", Private: "http://192.168.1.5:4000"}, scope: scopeLocal, want: "http://127.0.0.1:4000"},
		{name: "public", status: &controlapi.Status{Local: "http://127.0.0.1:4000", Public: "https://coder.example.com"}, scope: scopePublic, wantErr: "the inspector isn't served on the public URL; want local or private"},
		{name: "tailnet", status: &controlapi.Status{Local: "http://127.0.0.1:4000", Private: "https://coder.tail.ts.net"}, scope: scopePrivate, want: "https://coder.tail.ts.net"},
		{name: "tailnet over plain HTTP", status: &controlapi.Status{Local: "http://127.0.0.1:4000", Private: "http://coder.hs.test", PrivateInspector: true}, scope: scopePrivate, want: "http://coder.hs.test"},
		{name: "plain HTTP", status: &controlapi.Status{Local: "http://127.0.0.1:4000", Private: "http://192.168.1.5:4000"}, scope: scopePrivate, wantErr: plainHTTP},
		{name: "no private URL", status: &controlapi.Status{Name: "coder", Local: "http://127.0.0.1:4000"}, scope: scopePrivate, wantErr: "coder has no private URL; add one with `a2a-serve config set --private tailscale` (or a relay URL), then `a2a-serve restart`"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := inspectorURL(tc.status, tc.scope)
			if tc.wantErr != "" {
				if err == nil || err.Error() != tc.wantErr {
					t.Fatalf("inspectorURL(%q) error = %v, want %q", tc.scope, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("inspectorURL(%q) error = %v, want nil", tc.scope, err)
			}
			if got != tc.want {
				t.Fatalf("inspectorURL(%q) = %q, want %q", tc.scope, got, tc.want)
			}
		})
	}
}

func TestLocalOnly(t *testing.T) {
	t.Parallel()
	tests := []struct {
		url  string
		want bool
	}{
		{url: "http://127.0.0.1:4000", want: true},
		{url: "http://localhost:4000/n/grok", want: true},
		{url: "http://[::1]:4000", want: true},
		{url: "http://coder.tail.ts.net", want: false},
		{url: "https://coder.example.com", want: false},
		{url: "http://192.168.1.10:4000", want: false},
	}
	for _, tc := range tests {
		t.Run(tc.url, func(t *testing.T) {
			t.Parallel()
			if got := localOnly(tc.url); got != tc.want {
				t.Fatalf("localOnly(%q) = %v, want %v", tc.url, got, tc.want)
			}
		})
	}
}
