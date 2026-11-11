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

package agentcard

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/network"
)

func TestBuildDescription(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		cfg  *agentdir.Config
		want string
	}{
		{name: "acp", cfg: &agentdir.Config{Name: "coder", Harness: "acp"}, want: "coder is an acp coding agent reached through `a2a-serve`. " + inputRequiredNote},
		{name: "opencode", cfg: &agentdir.Config{Name: "coder", Harness: "opencode"}, want: "coder is an opencode coding agent reached through `a2a-serve`. " + inputRequiredNote},
		{name: "claude", cfg: &agentdir.Config{Name: "coder", Harness: "claude"}, want: "coder is a claude coding agent reached through `a2a-serve`. " + inputRequiredNote},
		{name: "configured", cfg: &agentdir.Config{Name: "coder", Harness: "claude", Description: "Writes Go."}, want: "Writes Go. " + inputRequiredNote},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := Build(tc.cfg, Listener{Kind: network.ListenerPrivate, URL: "http://127.0.0.1:9000"}).Description; got != tc.want {
				t.Fatalf("Build().Description = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestBuildSkills(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		card agentdir.CardConfig
		want []a2a.AgentSkill
	}{
		{
			name: "default",
			want: []a2a.AgentSkill{{ID: "coding", Name: "Coding agent", Description: "Works in its project workspace with opencode.", Tags: []string{"coding", "opencode"}}},
		},
		{
			name: "configured",
			card: agentdir.CardConfig{Skills: []agentdir.CardSkill{
				{ID: "release-notes", Name: "Release notes", Description: "Writes release notes from merged pull requests.", Tags: []string{"git", "docs"}},
				{ID: "review"},
			}},
			want: []a2a.AgentSkill{
				{ID: "release-notes", Name: "Release notes", Description: "Writes release notes from merged pull requests.", Tags: []string{"git", "docs"}},
				{ID: "review", Name: "review", Tags: []string{}},
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := &agentdir.Config{Name: "coder", Harness: "opencode", Card: tc.card}
			got := Build(cfg, Listener{Kind: network.ListenerLocal, URL: "http://127.0.0.1:9000"}).Skills
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Fatalf("Build().Skills wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestBuildModes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		voice      agentdir.VoiceConfig
		wantInput  []string
		wantOutput []string
	}{
		{
			name:       "without voice",
			wantInput:  []string{"text/plain", "application/octet-stream"},
			wantOutput: []string{"text/plain", "application/json"},
		},
		{
			name:       "with voice",
			voice:      agentdir.VoiceConfig{Service: "openai", Key: "secret:VOICE_API_KEY"},
			wantInput:  []string{"text/plain", "application/octet-stream", "audio/ogg", "audio/mpeg", "audio/wav"},
			wantOutput: []string{"text/plain", "application/json", "audio/ogg"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			card := Build(&agentdir.Config{Name: "coder", Harness: "opencode", Voice: tc.voice}, Listener{Kind: network.ListenerLocal, URL: "http://127.0.0.1:9000"})
			if diff := cmp.Diff(tc.wantInput, card.DefaultInputModes); diff != "" {
				t.Fatalf("Build().DefaultInputModes wrong result (-want +got) diff = %s", diff)
			}
			if diff := cmp.Diff(tc.wantOutput, card.DefaultOutputModes); diff != "" {
				t.Fatalf("Build().DefaultOutputModes wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestBuildSkillTagsServeAsArray(t *testing.T) {
	t.Parallel()
	cfg := &agentdir.Config{Name: "coder", Harness: "opencode", Card: agentdir.CardConfig{Skills: []agentdir.CardSkill{{ID: "review"}}}}
	data, err := json.Marshal(Build(cfg, Listener{Kind: network.ListenerLocal, URL: "http://127.0.0.1:9000"}).Skills)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	if got := string(data); !strings.Contains(got, `"tags":[]`) || strings.Contains(got, "null") {
		t.Fatalf("json.Marshal(Build().Skills) = %s, want tags as an empty array", got)
	}
}

func TestBuildSecurity(t *testing.T) {
	t.Parallel()
	signIn := a2a.OpenIDConnectSecurityScheme{OpenIDConnectURL: "https://accounts.google.com/.well-known/openid-configuration"}
	tests := []struct {
		name     string
		kind     string
		policy   agentdir.PolicyConfig
		signIn   a2a.SecurityScheme
		wantReqs a2a.SecurityRequirementsOptions
	}{
		{name: "local", kind: network.ListenerLocal, wantReqs: a2a.SecurityRequirementsOptions{{"pairing": {}}}},
		{name: "private open to everyone", kind: network.ListenerPrivate},
		{name: "private for pairings", kind: network.ListenerPrivate, policy: agentdir.PolicyConfig{PrivateAllowIncoming: agentdir.AccessPaired}, wantReqs: a2a.SecurityRequirementsOptions{{"pairing": {}}}},
		{name: "private ignores sign-in", kind: network.ListenerPrivate, policy: agentdir.PolicyConfig{PrivateAllowIncoming: agentdir.AccessPaired}, signIn: signIn, wantReqs: a2a.SecurityRequirementsOptions{{"pairing": {}}}},
		{name: "public for pairings", kind: network.ListenerPublic, wantReqs: a2a.SecurityRequirementsOptions{{"pairing": {}}}},
		{name: "public with sign-in", kind: network.ListenerPublic, signIn: signIn, wantReqs: a2a.SecurityRequirementsOptions{{"pairing": {}}, {"google": {}}}},
		{name: "public open to anyone", kind: network.ListenerPublic, policy: agentdir.PolicyConfig{PublicAllowIncoming: agentdir.AccessAnyone}, signIn: signIn},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := &agentdir.Config{Name: "coder", Harness: "opencode", Policy: tc.policy}
			card := Build(cfg, Listener{Kind: tc.kind, URL: "http://127.0.0.1:9000", SignIn: tc.signIn})
			if diff := cmp.Diff(tc.wantReqs, card.SecurityRequirements); diff != "" {
				t.Fatalf("Build(%q).SecurityRequirements wrong result (-want +got) diff = %s", tc.kind, diff)
			}
			if _, got := card.SecuritySchemes["pairing"]; got != (len(tc.wantReqs) > 0) {
				t.Fatalf("Build(%q) advertises the pairing scheme = %v, want %v", tc.kind, got, len(tc.wantReqs) > 0)
			}
		})
	}
}
