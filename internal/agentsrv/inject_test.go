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

package agentsrv

import (
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/access"
	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/egress"
	"github.com/a2aproject/a2a-serve/internal/sandboxnet"
)

func TestInjections(t *testing.T) {
	t.Parallel()
	cfg := &agentdir.Config{
		Secrets: []string{"OPENROUTER_API_KEY", "GH", "OTHER"},
		Inject: []agentdir.InjectRule{
			{Model: "openrouter", Key: "OPENROUTER_API_KEY"},
			{Host: "api.github.com", Header: "Authorization: Bearer $GH"},
		},
	}
	openrouter, _ := egress.LookupProvider("openrouter")
	placeholder := openrouter.Placeholder("OPENROUTER_API_KEY")
	modelKey := sandboxnet.Injection{
		Host: "openrouter.ai", Port: 443, Header: "Authorization", Value: "Bearer sk-or-real", Secrets: []string{"OPENROUTER_API_KEY"},
		Placeholder: placeholder, Key: "sk-or-real", Endpoints: openrouter.Endpoints,
	}
	tests := []struct {
		name       string
		env        []string
		wantEnv    []string
		wantInject []sandboxnet.Injection
	}{
		{
			name:    "every secret granted",
			env:     []string{"OPENROUTER_API_KEY=sk-or-real", "GH=ghp_real", "OTHER=o"},
			wantEnv: []string{"OPENROUTER_API_KEY=" + placeholder, "GH=" + egress.Placeholder, "OTHER=o"},
			wantInject: []sandboxnet.Injection{
				modelKey,
				{Host: "api.github.com", Port: 443, Header: "Authorization", Value: "Bearer ghp_real", Secrets: []string{"GH"}},
			},
		},
		{
			name:       "a secret withheld by the sandbox rules",
			env:        []string{"OPENROUTER_API_KEY=sk-or-real"},
			wantEnv:    []string{"OPENROUTER_API_KEY=" + placeholder},
			wantInject: []sandboxnet.Injection{modelKey},
		},
		{
			name:    "no secret granted",
			env:     []string{"OTHER=o"},
			wantEnv: []string{"OTHER=o"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env, inject, err := injections(cfg, tc.env)
			if err != nil {
				t.Fatalf("injections() error = %v, want nil", err)
			}
			if diff := cmp.Diff(tc.wantEnv, env); diff != "" {
				t.Fatalf("injections() wrong environment (-want +got) diff = %s", diff)
			}
			if diff := cmp.Diff(tc.wantInject, inject); diff != "" {
				t.Fatalf("injections() wrong injections (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestSecretEnvAddsTheGrantedValue(t *testing.T) {
	t.Parallel()
	cfg := &agentdir.Config{
		Secrets: []string{"ANTHROPIC_API_KEY", "GUEST_KEY"},
		Inject:  []agentdir.InjectRule{{Model: "anthropic", Key: "ANTHROPIC_API_KEY"}},
	}
	stored := agentdir.Secrets{"ANTHROPIC_API_KEY": "sk-owner", "GUEST_KEY": "sk-guest"}
	guest := access.Sandbox{Secrets: []access.SecretGrant{{Name: "ANTHROPIC_API_KEY", From: "GUEST_KEY"}, {Name: "MISSING", From: "MISSING"}}}

	env, inject, missing, err := secretEnv(cfg, guest, stored)

	if err != nil {
		t.Fatalf("secretEnv() error = %v, want nil", err)
	}
	anthropic, _ := egress.LookupProvider("anthropic")
	if diff := cmp.Diff([]string{"ANTHROPIC_API_KEY=" + anthropic.Placeholder("ANTHROPIC_API_KEY")}, env); diff != "" {
		t.Fatalf("secretEnv() wrong environment (-want +got) diff = %s", diff)
	}
	if len(inject) != 1 || inject[0].Key != "sk-guest" || inject[0].Value != "sk-guest" {
		t.Fatalf("secretEnv() injections = %+v, want the guest key added", inject)
	}
	if diff := cmp.Diff([]string{"MISSING"}, missing); diff != "" {
		t.Fatalf("secretEnv() wrong missing secrets (-want +got) diff = %s", diff)
	}
}

func TestInjectionsRefuseAValueAHeaderCantCarry(t *testing.T) {
	t.Parallel()
	cfg := &agentdir.Config{Secrets: []string{"GH"}, Inject: []agentdir.InjectRule{{Host: "api.github.com", Header: "Authorization: Bearer $GH"}}}
	if _, _, err := injections(cfg, []string{"GH=a\x00b"}); err == nil {
		t.Fatalf("injections() error = nil, want an error")
	}
}
