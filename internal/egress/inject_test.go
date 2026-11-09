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

package egress

import (
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestParseHeaderRule(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in          string
		want        HeaderRule
		wantString  string
		wantSecrets []string
	}{
		{
			in:          "api.github.com=Authorization: Bearer $GITHUB_TOKEN",
			want:        HeaderRule{Host: "api.github.com", Port: 443, Name: "Authorization", Value: "Bearer $GITHUB_TOKEN"},
			wantString:  "api.github.com=Authorization: Bearer $GITHUB_TOKEN",
			wantSecrets: []string{"GITHUB_TOKEN"},
		},
		{
			in:          " API.Example.com:8443 = x-token:${A}:${B}-$A ",
			want:        HeaderRule{Host: "api.example.com", Port: 8443, Name: "X-Token", Value: "${A}:${B}-$A"},
			wantString:  "api.example.com:8443=X-Token: ${A}:${B}-$A",
			wantSecrets: []string{"A", "B"},
		},
		{
			in:          "api.example.com:443=Authorization: $K",
			want:        HeaderRule{Host: "api.example.com", Port: 443, Name: "Authorization", Value: "$K"},
			wantString:  "api.example.com=Authorization: $K",
			wantSecrets: []string{"K"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			got, err := ParseHeaderRule(tc.in)
			if err != nil {
				t.Fatalf("ParseHeaderRule(%q) error = %v, want nil", tc.in, err)
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Fatalf("ParseHeaderRule(%q) wrong result (-want +got) diff = %s", tc.in, diff)
			}
			if got.String() != tc.wantString {
				t.Fatalf("HeaderRule.String() = %q, want %q", got.String(), tc.wantString)
			}
			if diff := cmp.Diff(tc.wantSecrets, got.Secrets()); diff != "" {
				t.Fatalf("HeaderRule.Secrets() wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestParseHeaderRuleRejects(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in, want string
	}{
		{in: "api.github.com", want: "want host=Name: value"},
		{in: "api.github.com=Authorization", want: "want a header Name: value"},
		{in: "api.github.com=Authorization: Bearer ghp_literal", want: "names no secret"},
		{in: "api.github.com=Authorization: $5 $K", want: "a $ starts"},
		{in: "api.github.com=Host: $K", want: "can't set the header Host"},
		{in: "api.github.com=Proxy-Authorization: $K", want: "can't set the header"},
		{in: "1.2.3.4=Authorization: $K", want: "IP address"},
		{in: "*.github.com=Authorization: $K", want: "one host name"},
		{in: "api.github.com:0=Authorization: $K", want: "invalid port"},
		{in: "bad host=Authorization: $K", want: "invalid host name"},
	}
	for _, tc := range tests {
		_, err := ParseHeaderRule(tc.in)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("ParseHeaderRule(%q) error = %v, want one containing %q", tc.in, err, tc.want)
		}
		if err != nil && strings.Contains(err.Error(), "ghp_") {
			t.Errorf("ParseHeaderRule(%q) error = %v, want it without the value", tc.in, err)
		}
	}
}

func TestHeaderRuleExpand(t *testing.T) {
	t.Parallel()
	r, err := ParseHeaderRule("h.example=X-Auth: ${A}/$B/$A")
	if err != nil {
		t.Fatalf("ParseHeaderRule() error = %v, want nil", err)
	}
	got := r.Expand(func(name string) string { return strings.ToLower(name) + "1" })
	if want := "a1/b1/a1"; got != want {
		t.Fatalf("HeaderRule.Expand() = %q, want %q", got, want)
	}
}

func TestMatchEndpoint(t *testing.T) {
	t.Parallel()
	gemini, _ := LookupProvider("gemini")
	endpoints := append([]string{"POST /v1/messages", "GET /v1/models/*"}, gemini.Endpoints...)
	tests := []struct {
		method, path string
		want         bool
	}{
		{method: "POST", path: "/v1/messages", want: true},
		{method: "GET", path: "/v1/messages"},
		{method: "POST", path: "/v1/messages/batches"},
		{method: "GET", path: "/v1/models/claude", want: true},
		{method: "GET", path: "/v1/models/claude/x"},
		{method: "GET", path: "/v1/files"},
		{method: "POST", path: "/v1beta/models/gemini-2.5-flash:streamGenerateContent", want: true},
		{method: "POST", path: "/v1beta/models/gemini-2.5-flash:batchGenerateContent"},
		{method: "GET", path: "/v1beta/models/gemini-2.5-flash/operations"},
		{method: "GET", path: "/v1beta/files"},
	}
	for _, tc := range tests {
		if got := MatchEndpoint(endpoints, tc.method, tc.path); got != tc.want {
			t.Errorf("MatchEndpoint(%q, %q) = %v, want %v", tc.method, tc.path, got, tc.want)
		}
	}
}

func TestProviderCredential(t *testing.T) {
	t.Parallel()
	tests := []struct {
		provider, key                      string
		wantHeader, wantScheme, wantPrefix string
	}{
		{provider: "anthropic", key: "ANTHROPIC_API_KEY", wantHeader: "X-Api-Key", wantPrefix: "sk-ant-api03-"},
		{provider: "anthropic", key: "CLAUDE_CODE_OAUTH_TOKEN", wantHeader: "Authorization", wantScheme: "Bearer ", wantPrefix: "sk-ant-oat01-"},
		{provider: "gemini", key: "GEMINI_API_KEY", wantHeader: "X-Goog-Api-Key", wantPrefix: "AIza"},
		{provider: "openai", key: "OPENAI_API_KEY", wantHeader: "Authorization", wantScheme: "Bearer ", wantPrefix: "sk-"},
		{provider: "openrouter", key: "MY_KEY", wantHeader: "Authorization", wantScheme: "Bearer ", wantPrefix: "sk-or-v1-"},
	}
	for _, tc := range tests {
		p, ok := LookupProvider(tc.provider)
		if !ok {
			t.Fatalf("LookupProvider(%q) = false, want true", tc.provider)
		}
		header, scheme := p.Credential(tc.key)
		if header != tc.wantHeader || scheme != tc.wantScheme {
			t.Errorf("%s.Credential(%q) = %q, %q, want %q, %q", tc.provider, tc.key, header, scheme, tc.wantHeader, tc.wantScheme)
		}
		if got := p.Placeholder(tc.key); !strings.HasPrefix(got, tc.wantPrefix) {
			t.Errorf("%s.Placeholder(%q) = %q, want the prefix %q", tc.provider, tc.key, got, tc.wantPrefix)
		}
	}
	if _, ok := LookupProvider("mistral"); ok {
		t.Fatalf("LookupProvider(%q) = true, want false", "mistral")
	}
}
