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

package serve

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"log/slog"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/core/egress"
)

func TestResolveInjectHeaders(t *testing.T) {
	t.Parallel()
	env := map[string]string{
		"GH_TOKEN":   "ghp_secret",
		"API_KEY":    "key_secret",
		"OVERRIDE":   "override-secret",
		"OPENAI_KEY": "sk-real",
		"USER_ENV":   "from-secret",
	}
	cfg := Config{
		Agent:     "opencode",
		Workspace: "memory",
		Sandbox:   SandboxDocker,
		Egress:    "allowlist",
		Env:       []string{"OVERRIDE=custom"},
		Secrets:   []string{"USER_ENV"},
		ModelKeys: []string{"openai=OPENAI_KEY"},
		InjectHeaders: []string{
			`api.github.com=Authorization:Bearer env:GH_TOKEN`,
			`api.example.com:8443=X-Api-Key: $API_KEY`,
			`override.example.com=Authorization: Bearer env:OVERRIDE`,
			`literal.example.com=X-Org: acme`,
			`shared.example.com=X-Shared: ${GH_TOKEN}`,
		},
		EgressMiddleware: []string{
			"api.anthropic.com=http://127.0.0.1:9000/anthropic",
		},
		Getenv: func(name string) string { return env[name] },
		Stdin:  strings.NewReader("host-token\n"),
	}

	got, err := cfg.resolve(context.Background())
	if err != nil {
		t.Fatalf("Config.resolve() error = %v", err)
	}
	if !got.hasInjectHeaders {
		t.Fatalf("Config.resolve() hasInjectHeaders = false, want true")
	}

	wantCreds := []egress.Credential{
		egress.ModelKey{Provider: "openai", Key: "sk-real"},
		egress.Header{Host: "api.github.com", Name: "Authorization", Value: "Bearer ghp_secret"},
		egress.Header{Host: "api.example.com:8443", Name: "X-Api-Key", Value: "key_secret"},
		egress.Header{Host: "override.example.com", Name: "Authorization", Value: "Bearer override-secret"},
		egress.Header{Host: "literal.example.com", Name: "X-Org", Value: "acme"},
		egress.Header{Host: "shared.example.com", Name: "X-Shared", Value: "ghp_secret"},
	}
	if diff := cmp.Diff(wantCreds, got.credentials); diff != "" {
		t.Fatalf("Config.resolve() credentials wrong result (-want +got) diff = %s", diff)
	}

	wantEnv := []string{
		"GH_TOKEN=a2a-serve-placeholder",
		"API_KEY=a2a-serve-placeholder",
		"ANTHROPIC_API_KEY=sk-ant-api03-a2a-serve-placeholder",
		"OVERRIDE=custom",
		"USER_ENV=from-secret",
	}
	if diff := cmp.Diff(wantEnv, got.env); diff != "" {
		t.Fatalf("Config.resolve() env wrong result (-want +got) diff = %s", diff)
	}
}

func TestNewBoundaryCACoversInjectedHeaders(t *testing.T) {
	t.Parallel()
	cfg := Config{
		Agent:         "opencode",
		Workspace:     "memory",
		Sandbox:       SandboxDocker,
		Egress:        "allowlist",
		InjectHeaders: []string{`api.github.com=Authorization:Bearer env:GH_TOKEN`},
		Getenv:        func(name string) string { return map[string]string{"GH_TOKEN": "gh-secret"}[name] },
	}
	set, err := cfg.resolve(context.Background())
	if err != nil {
		t.Fatalf("Config.resolve() error = %v", err)
	}

	b, proxy, mw, err := newBoundary(set, slog.New(slog.DiscardHandler), newTurns(), nil)
	if err != nil {
		t.Fatalf("newBoundary() error = %v", err)
	}
	if proxy != nil || mw != nil {
		t.Fatalf("newBoundary() proxy=%v mw=%v, want nil", proxy, mw)
	}
	t.Cleanup(func() {
		if err := b.Close(); err != nil {
			t.Errorf("Boundary.Close() error = %v", err)
		}
	})

	ep, err := b.Open("test", "ctx-1", set.credentials...)
	if err != nil {
		t.Fatalf("Boundary.Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := ep.Close(); err != nil {
			t.Errorf("Endpoint.Close() error = %v", err)
		}
	})

	block, _ := pem.Decode(ep.CA())
	if block == nil {
		t.Fatalf("Endpoint.CA() = nil, want a CA certificate")
	}
	caCert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("x509.ParseCertificate() error = %v", err)
	}
	wantDomains := []string{"api.github.com"}
	if diff := cmp.Diff(wantDomains, caCert.PermittedDNSDomains); diff != "" {
		t.Fatalf("Endpoint.CA() PermittedDNSDomains wrong result (-want +got) diff = %s", diff)
	}
}
