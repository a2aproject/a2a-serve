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
	"fmt"
	"os/exec"
	"strings"
	"testing"

	"github.com/a2aproject/a2a-serve/internal/agentprofile"
	"github.com/a2aproject/a2a-serve/internal/egressrules"
	"github.com/a2aproject/a2a-serve/internal/sandboxrt"
)

func TestExplain(t *testing.T) {
	t.Parallel()
	inject := []agentprofile.InjectRule{{Model: "openrouter", Key: "OPENROUTER_API_KEY"}}
	tests := []struct {
		name string
		err  error
		want string
	}{
		{name: "unknown agent", err: &agentprofile.UnknownAgentError{Name: "x", Known: []string{"claude", "opencode"}}, want: "unknown agent \"x\": known agents are [claude opencode], or pass --command for any ACP agent"},
		{name: "no image", err: &agentprofile.NoImageError{Agent: "x"}, want: "agent \"x\" has no container image; pass --image or --sandbox local"},
		{name: "not installed", err: &agentprofile.NotInstalledError{Command: "opencode"}, want: "opencode is not installed; install it, or run a2a-serve config set --sandbox docker, or run a2a-serve doctor"},
		{name: "workspace path", err: &agentprofile.WorkspacePathError{Src: "/w", Dst: "/x"}, want: "the first --mount is the workspace and must keep its path (/w), got /x"},
		{name: "nested agent", err: &agentprofile.NestedAgentsError{Roots: []string{"/w/a", "/w/b"}}, want: "another agent was created in /w/a, /w/b after this agent started, and its sandbox doesn't hide it yet; restart this agent with `a2a-serve restart`"},
		{name: "appeared", err: &agentprofile.AppearedError{Path: "/w/opencode.json"}, want: "/w/opencode.json appeared after this agent started, and its sandbox doesn't hide it yet; restart this agent with `a2a-serve restart`"},
		{name: "inject without a sandbox", err: &agentprofile.EgressError{Egress: agentprofile.Egress{Inject: inject}, Local: true}, want: "--inject-model-key can't apply: the agent runs with --sandbox local. The agent doesn't start, so that it can't read OPENROUTER_API_KEY. Fix that, or put the keys in the agent's environment: remove the rules with `a2a-serve config unset inject-model-key`, then store the keys again with `a2a-serve config set --secret OPENROUTER_API_KEY=-`"},
		{name: "allowlist without the boundary", err: &agentprofile.EgressError{Egress: agentprofile.Egress{Mode: "allowlist"}, Reason: "no tun"}, want: "--egress allowlist can't apply: no tun; run the server on Linux with a release build, or use --egress internet"},
		{name: "docker not installed", err: &sandboxrt.DockerError{Problem: "is not installed", Fix: "install Docker", Err: exec.ErrNotFound}, want: "docker is not installed; install Docker or run a2a-serve config set --sandbox local, or run a2a-serve doctor: executable file not found in $PATH"},
		{name: "docker host unreachable", err: &sandboxrt.DockerError{Problem: "is not reachable at unix:///x from DOCKER_HOST", Fix: "fix or unset DOCKER_HOST", Err: errors.New("exit status 1")}, want: "docker is not reachable at unix:///x from DOCKER_HOST; fix or unset DOCKER_HOST, or run a2a-serve config set --sandbox local, or run a2a-serve doctor: exit status 1"},
		{name: "process restarts", err: fmt.Errorf("refused: %w", ErrGrantChanged), want: "refused: the sandbox rules of this conversation changed, and its agent container restarts with them once its current tasks end; send the message again then"},
		{name: "process narrowed", err: ErrGrantNarrowed, want: "the sandbox rules of this conversation changed and give less, so its waiting tasks ended; send the message again"},
		{name: "inject rule", err: &InjectRuleError{Rule: agentprofile.InjectRule{Host: "api.github.com", Header: "Authorization: Bearer $GH"}, Err: errors.New("the value of GH has a character that a header can't carry")}, want: "--inject-header api.github.com=Authorization: Bearer $GH: the value of GH has a character that a header can't carry"},
		{name: "inject rule of an unknown provider", err: &InjectRuleError{Rule: agentprofile.InjectRule{Model: "acme", Key: "K"}, Err: egressrules.UnknownProvider("acme")}, want: "--inject-model-key acme=K: unknown model provider \"acme\": want " + strings.Join(egressrules.ProviderNames(), ", ") + "; use --inject-header for another host"},
		{name: "missing secret of a caller's process", err: &MissingSecretError{Name: "K"}, want: "the owner of the agent hasn't stored the secret K that this conversation's agent gets; ask them to store it"},
		{name: "missing secret of the main process", err: &MissingSecretError{Name: "K", Main: true}, want: "secret K is not set; pass --secret K again with the variable set"},
		{name: "wrapped", err: fmt.Errorf("starting opencode: %w", &agentprofile.NoImageError{Agent: "x"}), want: "starting opencode: agent \"x\" has no container image; pass --image or --sandbox local"},
		{name: "other error", err: errors.New("disk full"), want: "disk full"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := Explain(tc.err)
			if got.Error() != tc.want {
				t.Fatalf("Explain() = %q, want %q", got, tc.want)
			}
			if !errors.Is(got, tc.err) {
				t.Fatalf("Explain() = %v, want it to wrap %v", got, tc.err)
			}
		})
	}
}

func TestWarning(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		want string
	}{
		{name: "exposed agents", err: &agentprofile.NestedAgentsWarning{Roots: []string{"/w/sub"}, Exposed: true}, want: "found other agents in /w/sub; --sandbox local doesn't hide their .a2a directories, so the agent can read their secrets"},
		{name: "hidden agents", err: &agentprofile.NestedAgentsWarning{Roots: []string{"/w/sub"}}, want: "found other agents in /w/sub; the sandbox hides their .a2a directories"},
		{name: "no boundary", err: &agentprofile.NoBoundaryWarning{Reason: "no tun"}, want: "egress control is off: no tun. " + capitalize(UnrestrictedEffect)},
		{name: "no allow rules", err: agentprofile.ErrNoAllowRules, want: noAllowlistRules},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := Warning(tc.err); got != tc.want {
				t.Fatalf("Warning() = %q, want %q", got, tc.want)
			}
		})
	}
}
