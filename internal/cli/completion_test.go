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
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/credstore"
)

// completions returns the candidates of `a2a-serve __complete args`.
func completions(t *testing.T, creds *credstore.Store, args ...string) []string {
	t.Helper()
	out, err := runCMDWithCreds(t, creds, append([]string{"__complete"}, args...)...)
	if err != nil {
		t.Fatalf("a2a-serve __complete %v error = %v", args, err)
	}
	var got []string
	for line := range strings.SplitSeq(out, "\n") {
		if strings.HasPrefix(line, ":") {
			break
		}
		name, _, _ := strings.Cut(line, "\t")
		got = append(got, name)
	}
	return got
}

func TestCompletion(t *testing.T) {
	t.Parallel()
	const agentURL = "https://agent.example.com"
	tests := []struct {
		name string
		args []string
		want []string
	}{
		{name: "private", args: []string{"config", "set", "--private", ""}, want: []string{"tailscale", "lan", "none"}},
		{name: "public", args: []string{"start", "opencode", "--public", ""}, want: []string{"tailscale", "ngrok", "none"}},
		{name: "sandbox", args: []string{"start", "claude", "--sandbox", ""}, want: []string{"docker", "local"}},
		{name: "agent", args: []string{"config", "set", "--agent", ""}, want: []string{"claude", "opencode", "acp"}},
		{name: "channel kinds", args: []string{"channel", "rm", ""}, want: []string{"telegram", "discord", "email"}},
		{name: "kind", args: []string{"pair", "set", "anna", "--kind", ""}, want: []string{"person", "device", "agent"}},
		{name: "joined agents", args: []string{"pair", "rm", "--dir", "/nonexistent-a2a-agent", ""}, want: []string{agentURL}},
		{name: "contacts of a stopped agent", args: []string{"contact", "rm", "--dir", "/nonexistent-a2a-agent", ""}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			creds := &credstore.Store{Path: filepath.Join(t.TempDir(), "credentials.json")}
			if err := creds.Put(credstore.Credential{URL: agentURL, Token: "tok"}); err != nil {
				t.Fatalf("creds.Put() error = %v", err)
			}

			got := completions(t, creds, tc.args...)

			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Fatalf("a2a-serve __complete %v wrong result (-want +got) diff = %s", tc.args, diff)
			}
		})
	}
}

func TestCompletionCommands(t *testing.T) {
	t.Parallel()
	creds := &credstore.Store{Path: filepath.Join(t.TempDir(), "credentials.json")}

	got := completions(t, creds, "")

	for _, want := range []string{"start", "pair", "channel"} {
		if !slices.Contains(got, want) {
			t.Fatalf("a2a-serve __complete \"\" = %v, want %q among them", got, want)
		}
	}
	run := completions(t, creds, "start", "")
	for _, want := range []string{"opencode", "claude", "acp", "echo", "proxy", "exec"} {
		if !slices.Contains(run, want) {
			t.Fatalf("a2a-serve __complete run \"\" = %v, want %q among them", run, want)
		}
	}
}

func TestCompletionScript(t *testing.T) {
	t.Parallel()
	for _, shell := range []string{"bash", "zsh", "fish", "powershell"} {
		t.Run(shell, func(t *testing.T) {
			t.Parallel()
			out, err := runCMD(t, "completion", shell)
			if err != nil {
				t.Fatalf("a2a-serve completion %s error = %v", shell, err)
			}
			if !strings.Contains(out, scriptProgram) {
				t.Fatalf("a2a-serve completion %s = %q, want a script for %s", shell, out, scriptProgram)
			}
		})
	}
}
