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
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/clierr"
	"github.com/a2aproject/a2a-serve/internal/controlapi"
	"github.com/a2aproject/a2a-serve/internal/output"
	"github.com/a2aproject/a2a-serve/internal/project"
)

func TestStatusStoppedJSON(t *testing.T) {
	t.Parallel()
	d := newTestAgent(t, &agentdir.Config{Name: "coder", Harness: "opencode"})

	out, err := runCMD(t, "-o", "json", "status", "--dir", d.Root)
	if err != nil {
		t.Fatalf("a2a-serve status error = %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("json.Unmarshal(%q) error = %v", out, err)
	}
	want := map[string]any{"name": "coder", "agent": "opencode", "dir": d.Root, "running": false}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("a2a-serve status -o json wrong result (-want +got) diff = %s", diff)
	}
}

func TestStatusURLScope(t *testing.T) {
	t.Parallel()
	d := newTestAgent(t, &agentdir.Config{Name: "coder", Harness: "opencode", Port: 4100})
	empty := t.TempDir()
	tests := []struct {
		name     string
		args     []string
		wantOut  string
		wantExit int
	}{
		{name: "scope after the flag", args: []string{"status", "--url", "local", "--dir", d.Root}, wantOut: "http://127.0.0.1:4100\n"},
		{name: "scope joined with =", args: []string{"status", "--url=local", "--dir", d.Root}, wantOut: "http://127.0.0.1:4100\n"},
		{name: "scope of a stopped agent which needs the server", args: []string{"status", "--url", "private", "--dir", d.Root}, wantExit: 6},
		{name: "flag without a scope of a stopped agent", args: []string{"status", "--url", "--dir", d.Root}, wantExit: 6},
		{name: "scope without the flag", args: []string{"status", "local", "--dir", d.Root}, wantExit: 2},
		{name: "second argument", args: []string{"status", "--url", "local", "extra", "--dir", d.Root}, wantExit: 2},
		{name: "unknown scope before the directory lookup", args: []string{"status", "--url", "bogus", "--dir", empty}, wantExit: 2},
		{name: "unknown scope joined with =", args: []string{"status", "--url=bogus", "--dir", empty}, wantExit: 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			out, err := runCMD(t, tc.args...)
			gotExit := 0
			if err != nil {
				gotExit = clierr.Classify(err).Exit
			}
			if gotExit != tc.wantExit {
				t.Fatalf("a2a-serve %s error = %v (exit %d), want exit %d", strings.Join(tc.args, " "), err, gotExit, tc.wantExit)
			}
			if out != tc.wantOut {
				t.Fatalf("a2a-serve %s = %q, want %q", strings.Join(tc.args, " "), out, tc.wantOut)
			}
		})
	}
}

func TestStatusURLLocalBeforeTheFirstStart(t *testing.T) {
	t.Parallel()
	d := newTestAgent(t, &agentdir.Config{Name: "coder", Harness: "opencode"})

	_, err := runCMD(t, "status", "--url", "local", "--dir", d.Root)

	got := classify(err, nil)
	if err == nil || got.Code != clierr.CodeNotRunning {
		t.Fatalf("a2a-serve status --url local error = %v (%s), want %s", err, got.Code, clierr.CodeNotRunning)
	}
	for _, want := range []string{"no port has been picked yet", "first start"} {
		if !strings.Contains(got.Message, want) {
			t.Fatalf("a2a-serve status --url local error = %q, want %q", got.Message, want)
		}
	}
	if !strings.Contains(got.Hint, "a2a-serve start --detach") {
		t.Fatalf("a2a-serve status --url local hint = %q, want a hint to start it", got.Hint)
	}
}

func TestInspectScopeValidatedFirst(t *testing.T) {
	t.Parallel()
	empty := t.TempDir()

	_, err := runCMD(t, "inspect", "--url", "public", "--dir", empty)

	want := "the inspector isn't served on the public URL; want local or private"
	if err == nil || err.Error() != want {
		t.Fatalf("a2a-serve inspect --url public error = %v, want %q", err, want)
	}
	if got := clierr.Classify(err).Exit; got != 2 {
		t.Fatalf("a2a-serve inspect --url public exit = %d, want 2", got)
	}
}

func TestPrintStatusAccess(t *testing.T) {
	t.Parallel()
	var out strings.Builder
	cfg := &globalConfig{Printer: output.NewPrinter(&out, output.ModeText)}
	st := &controlapi.Status{
		Name: "coder", Harness: "opencode", Started: time.Now(), Local: "http://127.0.0.1:4000", Private: "https://coder.tail1234.ts.net",
		Access: &controlapi.Access{Summary: []string{"you: this machine and your tailnet devices", "everyone on the tailnet, 2 pairings", "the public URL needs a pairing"}},
	}
	if err := printStatus(cfg, st, nil, nil, "", ""); err != nil {
		t.Fatalf("printStatus() error = %v", err)
	}
	want := "  access   you: this machine and your tailnet devices\n" +
		"           everyone on the tailnet, 2 pairings\n" +
		"           the public URL needs a pairing\n"
	if !strings.Contains(out.String(), want) {
		t.Fatalf("printStatus() = %q, want the access block %q", out.String(), want)
	}
}

func TestPrintStatusMCPMissing(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		stored agentdir.Secrets
		want   string
	}{
		{
			name: "unset",
			want: "  mcp      grafana: uvx mcp-grafana ! not passed to the agent, unset: GRAFANA_TOKEN, GRAFANA_URL (pass them with --secret)\n",
		},
		{
			name:   "stored after the start",
			stored: agentdir.Secrets{"GRAFANA_TOKEN": "tok"},
			want: "  mcp      grafana: uvx mcp-grafana ! not passed to the agent, unset: GRAFANA_URL (pass them with --secret);" +
				" stored, passed after `a2a-serve restart`: GRAFANA_TOKEN\n",
		},
		{
			name:   "all stored",
			stored: agentdir.Secrets{"GRAFANA_TOKEN": "tok", "GRAFANA_URL": "https://g.example.com"},
			want:   "  mcp      grafana: uvx mcp-grafana ! stored, passed after `a2a-serve restart`: GRAFANA_TOKEN, GRAFANA_URL\n",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var out strings.Builder
			cfg := &globalConfig{Printer: output.NewPrinter(&out, output.ModeText)}
			st := &controlapi.Status{
				Name: "coder", Harness: "opencode", Started: time.Now(), Local: "http://127.0.0.1:4000",
				MCPServers: []controlapi.MCPServerStatus{{Name: "grafana", Summary: "uvx mcp-grafana", Missing: []string{"GRAFANA_TOKEN", "GRAFANA_URL"}}},
			}
			next := project.Vars(&agentdir.Config{Secrets: []string{"GRAFANA_TOKEN", "GRAFANA_URL"}}, tc.stored)
			if err := printStatus(cfg, st, nil, next, "", ""); err != nil {
				t.Fatalf("printStatus() error = %v", err)
			}
			if !strings.Contains(out.String(), tc.want) {
				t.Fatalf("printStatus() = %q, want the MCP line %q", out.String(), tc.want)
			}
		})
	}
}

func TestPrintStatusTools(t *testing.T) {
	t.Parallel()
	var out strings.Builder
	cfg := &globalConfig{Printer: output.NewPrinter(&out, output.ModeText)}
	st := &controlapi.Status{
		Name: "coder", Harness: "opencode", Started: time.Now(), Local: "http://127.0.0.1:4000",
		AllowTools: []string{"read", "tool:grafana.*", "command:kubectl get *"}, DenyTools: []string{"command:git push *"},
	}
	if err := printStatus(cfg, st, nil, nil, "", ""); err != nil {
		t.Fatalf("printStatus() error = %v", err)
	}
	want := "  tools    allow read, tool:grafana.*, command:kubectl get *\n" +
		"           deny command:git push *\n"
	if !strings.Contains(out.String(), want) {
		t.Fatalf("printStatus() = %q, want the tools block %q", out.String(), want)
	}
}

func TestPrintStatusNetworkProblems(t *testing.T) {
	t.Parallel()
	removed := &controlapi.NetworkProblem{State: "removed", Problem: "the relay https://relay.example.com removed this agent", Fix: "ask the relay's operator to let it back"}
	down := &controlapi.NetworkProblem{State: "disconnected", Problem: "not connected to the relay https://relay.example.com; reconnecting"}
	tests := []struct {
		name string
		st   controlapi.Status
		want string
	}{
		{
			name: "removed",
			st:   controlapi.Status{PrivateProblem: removed, PublicProblem: removed},
			want: "  local    http://127.0.0.1:4000\n" +
				"  private  ! the relay https://relay.example.com removed this agent; ask the relay's operator to let it back\n" +
				"  public   ! the relay https://relay.example.com removed this agent; ask the relay's operator to let it back\n",
		},
		{
			name: "disconnected",
			st:   controlapi.Status{Private: "http://agentmesh.local/mesh/p1/a2a/coder", PrivateProblem: down},
			want: "  local    http://127.0.0.1:4000\n" +
				"  private  http://agentmesh.local/mesh/p1/a2a/coder\n" +
				"           ! not connected to the relay https://relay.example.com; reconnecting\n",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var out strings.Builder
			cfg := &globalConfig{Printer: output.NewPrinter(&out, output.ModeText)}
			st := tc.st
			st.Name, st.Harness, st.Started, st.Local = "coder", "opencode", time.Now(), "http://127.0.0.1:4000"
			if err := printStatus(cfg, &st, nil, nil, "", ""); err != nil {
				t.Fatalf("printStatus() error = %v", err)
			}
			if !strings.Contains(out.String(), tc.want) {
				t.Fatalf("printStatus() = %q, want the URL block %q", out.String(), tc.want)
			}
		})
	}
}

func TestStatusURLOfRemovedAgent(t *testing.T) {
	t.Parallel()
	removed := &controlapi.NetworkProblem{State: "removed", Problem: "the relay https://relay.example.com removed this agent", Fix: "ask the relay's operator to let it back"}
	st := &controlapi.Status{Name: "coder", Local: "http://127.0.0.1:4000", PrivateProblem: removed, PublicProblem: removed}
	for _, scope := range []string{scopePrivate, scopePublic} {
		_, err := statusURL(st, scope)
		if err == nil || !strings.Contains(err.Error(), removed.Line()) {
			t.Fatalf("statusURL(%q) error = %v, want %q", scope, err, removed.Line())
		}
	}
	got, err := statusURL(st, "")
	if err != nil || got != st.Local {
		t.Fatalf("statusURL(\"\") = (%q, %v), want %q", got, err, st.Local)
	}
}

func TestStatusJSONNamesAgentOnce(t *testing.T) {
	t.Parallel()
	st := &controlapi.Status{Name: "coder", Harness: "opencode", Dir: "/work"}

	data, err := json.Marshal(statusJSON{Name: st.Name, Harness: st.Harness, Dir: st.Dir, Running: true, Status: st})
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("json.Unmarshal(%q) error = %v", data, err)
	}
	if _, ok := got["harness"]; ok {
		t.Fatalf("status -o json = %s, want no harness key", data)
	}
	if got["agent"] != "opencode" {
		t.Fatalf("status -o json agent = %v, want %q", got["agent"], "opencode")
	}
}

func TestStatusHelp(t *testing.T) {
	t.Parallel()

	out, err := runCMD(t, "status", "--help")

	if err != nil {
		t.Fatalf("a2a-serve status --help error = %v", err)
	}
	if strings.Contains(out, "[=") {
		t.Fatalf("a2a-serve status --help = %q, want no [=value] after --url", out)
	}
	for _, want := range []string{"--url scope  ", "--url=local does the same. --url local also works while the agent is\nstopped,"} {
		if !strings.Contains(out, want) {
			t.Fatalf("a2a-serve status --help = %q, want %q", out, want)
		}
	}
	for line := range strings.Lines(strings.SplitN(out, "Usage:", 2)[0]) {
		if len(line) > 80 {
			t.Fatalf("a2a-serve status --help has the line %q, want at most 80 characters", line)
		}
	}
}
