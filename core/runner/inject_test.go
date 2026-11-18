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

package runner

import (
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/agentprofile"
	"github.com/a2aproject/a2a-serve/internal/boundary"
	"github.com/a2aproject/a2a-serve/internal/egressrules"
)

func TestSecretEnv(t *testing.T) {
	t.Parallel()
	rules := []agentprofile.InjectRule{
		{Model: "openrouter", Key: "OPENROUTER_API_KEY"},
		{Host: "api.github.com", Header: "Authorization: Bearer $GH"},
	}
	stored := map[string]string{"OPENROUTER_API_KEY": "sk-or-real", "GH": "ghp_real", "OTHER": "o", "GUEST_KEY": "sk-or-guest"}
	openrouter, _ := egressrules.LookupProvider("openrouter")
	placeholder := openrouter.Placeholder("OPENROUTER_API_KEY")
	modelKey := func(key string) boundary.Injection {
		return boundary.Injection{
			Host: "openrouter.ai", Port: 443, Header: "Authorization", Value: "Bearer " + key, Secrets: []string{"OPENROUTER_API_KEY"},
			Model: true, Endpoints: openrouter.Endpoints,
		}
	}
	grant := func(name, from string) SecretGrant { return SecretGrant{Name: name, From: from} }
	tests := []struct {
		name        string
		grants      []SecretGrant
		wantEnv     []string
		wantInject  []boundary.Injection
		wantMissing []string
	}{
		{
			name:    "every secret granted",
			grants:  []SecretGrant{grant("OPENROUTER_API_KEY", "OPENROUTER_API_KEY"), grant("GH", "GH"), grant("OTHER", "OTHER")},
			wantEnv: []string{"OPENROUTER_API_KEY=" + placeholder, "GH=" + egressrules.Placeholder, "OTHER=o"},
			wantInject: []boundary.Injection{
				modelKey("sk-or-real"),
				{Host: "api.github.com", Port: 443, Header: "Authorization", Value: "Bearer ghp_real", Secrets: []string{"GH"}},
			},
		},
		{
			name:       "another secret under an added name",
			grants:     []SecretGrant{grant("OPENROUTER_API_KEY", "GUEST_KEY")},
			wantEnv:    []string{"OPENROUTER_API_KEY=" + placeholder},
			wantInject: []boundary.Injection{modelKey("sk-or-guest")},
		},
		{
			name:    "an added secret under a name no rule adds",
			grants:  []SecretGrant{grant("TOKEN", "GH")},
			wantEnv: []string{"TOKEN=" + egressrules.Placeholder},
		},
		{
			name:        "no secret granted, one missing",
			grants:      []SecretGrant{grant("OTHER", "OTHER"), grant("GONE", "GONE")},
			wantEnv:     []string{"OTHER=o"},
			wantMissing: []string{"GONE"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env, inject, missing, err := secretEnv(rules, Grant{Secrets: tc.grants}, stored)
			if err != nil {
				t.Fatalf("secretEnv() error = %v, want nil", err)
			}
			if diff := cmp.Diff(tc.wantEnv, env); diff != "" {
				t.Fatalf("secretEnv() wrong environment (-want +got) diff = %s", diff)
			}
			if diff := cmp.Diff(tc.wantInject, inject); diff != "" {
				t.Fatalf("secretEnv() wrong injections (-want +got) diff = %s", diff)
			}
			if diff := cmp.Diff(tc.wantMissing, missing); diff != "" {
				t.Fatalf("secretEnv() wrong missing secrets (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestSecretEnvRefusesAValueAHeaderCantCarry(t *testing.T) {
	t.Parallel()
	rules := []agentprofile.InjectRule{{Host: "api.github.com", Header: "Authorization: Bearer $GH"}}
	g := Grant{Secrets: []SecretGrant{{Name: "GH", From: "GH"}}}
	if _, _, _, err := secretEnv(rules, g, map[string]string{"GH": "a\x00b"}); err == nil {
		t.Fatalf("secretEnv() error = nil, want an error")
	}
}
