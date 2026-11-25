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

package a2aserve_test

import (
	"errors"
	"slices"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/core/a2aserve"
	"github.com/a2aproject/a2a-serve/internal/agentprofile"
)

func TestProfile(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		profile   a2aserve.Profile
		wantName  string
		wantImage string
		wantArgv  []string
	}{
		{name: "opencode", profile: a2aserve.Opencode(), wantName: "opencode", wantImage: agentprofile.ImageRepo + "opencode:" + agentprofile.ImageTag, wantArgv: []string{"opencode", "acp"}},
		{name: "claude", profile: a2aserve.Claude(), wantName: "claude", wantImage: agentprofile.LocalImage("claude"), wantArgv: []string{"claude-agent-acp"}},
		{name: "command", profile: a2aserve.Command("/usr/bin/gemini", "--experimental-acp"), wantName: "gemini", wantArgv: []string{"/usr/bin/gemini", "--experimental-acp"}},
		{name: "command named like a profile has no image", profile: a2aserve.Command("opencode", "acp"), wantName: "opencode", wantArgv: []string{"opencode", "acp"}},
		{name: "command without argv", profile: a2aserve.Command()},
		{name: "zero value", profile: a2aserve.Profile{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.profile.Name(); got != tc.wantName {
				t.Errorf("Profile.Name() = %q, want %q", got, tc.wantName)
			}
			if got := tc.profile.Image(); got != tc.wantImage {
				t.Errorf("Profile.Image() = %q, want %q", got, tc.wantImage)
			}
			if diff := cmp.Diff(tc.wantArgv, tc.profile.Argv("some-model")); diff != "" {
				t.Errorf("Profile.Argv() wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestLookupProfile(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		agent     string
		argv      []string
		wantName  string
		wantImage string
		wantArgv  []string
		wantErr   bool
	}{
		{name: "opencode", agent: "opencode", wantName: "opencode", wantImage: agentprofile.ImageRepo + "opencode:" + agentprofile.ImageTag, wantArgv: []string{"opencode", "acp"}},
		{name: "claude", agent: "claude", wantName: "claude", wantImage: agentprofile.LocalImage("claude"), wantArgv: []string{"claude-agent-acp"}},
		{name: "argv replaces the command and keeps the image", agent: "opencode", argv: []string{"opencode", "acp", "--print-logs"}, wantName: "opencode", wantImage: agentprofile.ImageRepo + "opencode:" + agentprofile.ImageTag, wantArgv: []string{"opencode", "acp", "--print-logs"}},
		{name: "unknown name with argv", agent: "gemini", argv: []string{"gemini", "--experimental-acp"}, wantName: "gemini", wantArgv: []string{"gemini", "--experimental-acp"}},
		{name: "unknown name", agent: "gemini", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p, err := a2aserve.LookupProfile(tc.agent, tc.argv)
			if (err != nil) != tc.wantErr {
				t.Fatalf("a2aserve.LookupProfile() error = %v, want error %v", err, tc.wantErr)
			}
			if err != nil {
				return
			}
			if got := p.Name(); got != tc.wantName {
				t.Errorf("Profile.Name() = %q, want %q", got, tc.wantName)
			}
			if got := p.Image(); got != tc.wantImage {
				t.Errorf("Profile.Image() = %q, want %q", got, tc.wantImage)
			}
			if diff := cmp.Diff(tc.wantArgv, p.Argv("")); diff != "" {
				t.Errorf("Profile.Argv() wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestLookupProfileUnknownAgentError(t *testing.T) {
	t.Parallel()
	_, err := a2aserve.LookupProfile("gemini", nil)
	var unknown *agentprofile.UnknownAgentError
	if !errors.As(err, &unknown) {
		t.Fatalf("a2aserve.LookupProfile() error = %v, want an *UnknownAgentError", err)
	}
	if diff := cmp.Diff(&agentprofile.UnknownAgentError{Name: "gemini", Known: a2aserve.ProfileNames()}, unknown); diff != "" {
		t.Errorf("a2aserve.LookupProfile() error wrong result (-want +got) diff = %s", diff)
	}
}

func TestLookupProfileArgvIsCopied(t *testing.T) {
	t.Parallel()
	argv := []string{"gemini", "--experimental-acp"}
	p, err := a2aserve.LookupProfile("gemini", argv)
	if err != nil {
		t.Fatalf("a2aserve.LookupProfile() error = %v", err)
	}
	argv[1] = "changed"
	if diff := cmp.Diff([]string{"gemini", "--experimental-acp"}, p.Argv("")); diff != "" {
		t.Errorf("Profile.Argv() after changing argv wrong result (-want +got) diff = %s", diff)
	}
}

func TestProfileNames(t *testing.T) {
	t.Parallel()
	got := a2aserve.ProfileNames()
	if diff := cmp.Diff([]string{"claude", "opencode"}, got); diff != "" {
		t.Errorf("a2aserve.ProfileNames() wrong result (-want +got) diff = %s", diff)
	}
	if !slices.IsSorted(got) {
		t.Errorf("a2aserve.ProfileNames() = %v, want sorted", got)
	}
}
