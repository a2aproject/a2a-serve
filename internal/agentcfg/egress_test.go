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

package agentcfg

import (
	"errors"
	"strings"
	"testing"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/agentprofile"
	"github.com/a2aproject/a2a-serve/internal/sandboxrt"
)

func TestDecideEgress(t *testing.T) {
	t.Parallel()
	fail := func() error { return errors.New("no tun") }
	ok := func() error { return nil }
	noLauncher := func() (string, error) { return "", errors.New("no launcher") }
	launcher := func() (string, error) { return "/usr/bin/a2a-serve", nil }
	linux := sandboxrt.EgressEnv{GOOS: "linux", Launcher: launcher, TUN: ok}
	with := func(f func(*sandboxrt.EgressEnv)) sandboxrt.EgressEnv { e := linux; f(&e); return e }
	tests := []struct {
		name     string
		cfg      agentdir.Config
		env      sandboxrt.EgressEnv
		boundary bool
		warning  string
	}{
		{name: "docker", env: linux, boundary: true},
		{name: "docker without a launcher", env: with(func(e *sandboxrt.EgressEnv) { e.Launcher = noLauncher }), warning: "egress control is off: no launcher. The agent reaches every network"},
		{name: "docker without a TUN device", env: with(func(e *sandboxrt.EgressEnv) { e.TUN = fail }), warning: "egress control is off: no tun"},
		{name: "image for another architecture", env: with(func(e *sandboxrt.EgressEnv) { e.ImageArch = "s390x" }), warning: "the agent's image is for s390x"},
		{name: "docker desktop", env: with(func(e *sandboxrt.EgressEnv) { e.GOOS = "darwin"; e.Launcher = noLauncher; e.TUN = fail }), boundary: true},
		{name: "forced relay", env: with(func(e *sandboxrt.EgressEnv) { e.ForceRelay = true; e.Launcher = noLauncher }), boundary: true},
		{name: "relay image to pull", cfg: agentdir.Config{Egress: agentdir.EgressConfig{Mode: "allowlist", Allow: []string{"nas.lan"}}}, env: with(func(e *sandboxrt.EgressEnv) {
			e.ForceRelay = true
			e.RelayImage = func() error { return &sandboxrt.MissingRelayImage{Image: "img", Pull: true} }
		}), boundary: true, warning: "the egress container's image img isn't on this machine"},
		{name: "relay image unknown", cfg: agentdir.Config{Egress: agentdir.EgressConfig{Mode: "allowlist", Allow: []string{"nas.lan"}}}, env: with(func(e *sandboxrt.EgressEnv) {
			e.ForceRelay = true
			e.RelayImage = func() error { return errors.New("no commit") }
		}), warning: "--egress allowlist can't apply: no commit"},
		{name: "allowlist without a launcher", cfg: agentdir.Config{Egress: agentdir.EgressConfig{Mode: "allowlist", Allow: []string{"nas.lan"}}}, env: with(func(e *sandboxrt.EgressEnv) { e.Launcher = noLauncher }), warning: "--egress allowlist can't apply: no launcher"},
		{name: "allowlist without rules", cfg: agentdir.Config{Egress: agentdir.EgressConfig{Mode: "allowlist"}}, env: linux, boundary: true, warning: "no --egress-allow rules"},
		{name: "inject without a launcher", cfg: agentdir.Config{Inject: []agentdir.InjectRule{{Host: "api.github.com", Header: "Authorization: Bearer $GH"}}}, env: with(func(e *sandboxrt.EgressEnv) { e.Launcher = noLauncher }), warning: "--inject-header can't apply: no launcher. The agent doesn't start, so that it can't read GH."},
		{name: "unrestricted", cfg: agentdir.Config{Egress: agentdir.EgressConfig{Mode: "unrestricted"}}, env: with(func(e *sandboxrt.EgressEnv) { e.Launcher = noLauncher })},
		{name: "local", cfg: agentdir.Config{Sandbox: agentprofile.SandboxLocal}, env: linux},
		{name: "local with rules", cfg: agentdir.Config{Sandbox: agentprofile.SandboxLocal, Egress: agentdir.EgressConfig{Allow: []string{"nas.lan"}}}, env: linux, warning: "--egress-allow has no effect with --sandbox local: " + UnrestrictedEffect + "; use --sandbox docker, or remove the rules with `a2a-serve config unset egress-allow`"},
		{name: "local with a mode", cfg: agentdir.Config{Sandbox: agentprofile.SandboxLocal, Egress: agentdir.EgressConfig{Mode: "allowlist"}}, env: linux, warning: "--egress has no effect with --sandbox local: " + UnrestrictedEffect + "; use --sandbox docker, or remove the setting with `a2a-serve config unset egress`"},
		{name: "local with a mode and rules", cfg: agentdir.Config{Sandbox: agentprofile.SandboxLocal, Egress: agentdir.EgressConfig{Mode: "allowlist", Allow: []string{"nas.lan"}}}, env: linux, warning: "--egress and --egress-allow have no effect with --sandbox local: " + UnrestrictedEffect + "; use --sandbox docker, or remove the settings with `a2a-serve config unset egress` and `a2a-serve config unset egress-allow`"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := DecideEgress(&tc.cfg, tc.env.Probe)
			if got.Boundary != tc.boundary {
				t.Fatalf("DecideEgress().Boundary = %v, want %v", got.Boundary, tc.boundary)
			}
			if tc.warning == "" && got.Warning != "" || !strings.Contains(got.Warning, tc.warning) {
				t.Fatalf("DecideEgress().Warning = %q, want it to contain %q", got.Warning, tc.warning)
			}
		})
	}
}

func TestEgressProblemWarning(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		cfg  agentdir.Config
		want string
	}{
		{name: "internet", want: "egress control is off: the agent's container can't set up its network: operation not permitted. The agent reaches every network"},
		{name: "allowlist", cfg: agentdir.Config{Egress: agentdir.EgressConfig{Mode: "allowlist"}}, want: "--egress allowlist can't apply: the agent's container can't set up its network: operation not permitted"},
		{
			name: "inject", cfg: agentdir.Config{Inject: []agentdir.InjectRule{{Model: "openrouter", Key: "OPENROUTER_API_KEY"}}},
			want: "--inject-model-key can't apply: the agent's container can't set up its network: operation not permitted. The agent doesn't start, so that it can't read OPENROUTER_API_KEY. Fix that, or put the keys in the agent's environment: remove the rules with `a2a-serve config unset inject-model-key`, then store the keys again with `a2a-serve config set --secret OPENROUTER_API_KEY=-`",
		},
	}
	two := egressProblem(agentprofile.Egress{Inject: []agentdir.InjectRule{{Model: "openrouter", Key: "K"}, {Host: "api.github.com", Header: "Authorization: Bearer $GH"}}}, "x")
	if want := "remove the rules with `a2a-serve config unset inject-model-key` and `a2a-serve config unset inject-header`, then store the keys again with `a2a-serve config set --secret K=-` and `a2a-serve config set --secret GH=-`"; !strings.Contains(two, want) {
		t.Fatalf("egressProblem() = %q, want it to contain %q", two, want)
	}
	for _, tc := range tests {
		got := Warning(&agentprofile.EgressError{Egress: Egress(&tc.cfg), Reason: "the agent's container can't set up its network: operation not permitted"})
		if tc.name == "internet" {
			got = Warning(&agentprofile.NoBoundaryWarning{Reason: "the agent's container can't set up its network: operation not permitted"})
		}
		if !strings.HasPrefix(got, tc.want) {
			t.Fatalf("Warning(%s) = %q, want it to start with %q", tc.name, got, tc.want)
		}
	}
}

func TestDecideEgressProbesOnlyForTheBoundary(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		cfg       agentdir.Config
		wantProbe bool
	}{
		{name: "docker", wantProbe: true},
		{name: "allowlist", cfg: agentdir.Config{Egress: agentdir.EgressConfig{Mode: "allowlist"}}, wantProbe: true},
		{name: "unrestricted", cfg: agentdir.Config{Egress: agentdir.EgressConfig{Mode: "unrestricted"}}},
		{name: "local", cfg: agentdir.Config{Sandbox: agentprofile.SandboxLocal, Egress: agentdir.EgressConfig{Allow: []string{"nas.lan"}}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			probed := false
			DecideEgress(&tc.cfg, func() sandboxrt.Capabilities {
				probed = true
				return sandboxrt.Capabilities{}
			})
			if probed != tc.wantProbe {
				t.Fatalf("DecideEgress() probed the machine = %v, want %v", probed, tc.wantProbe)
			}
		})
	}
}
