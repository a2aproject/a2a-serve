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

package toolrules

import (
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestValidate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		entry   string
		wantErr string
	}{
		{entry: "read"},
		{entry: "*"},
		{entry: "e*"},
		{entry: "switch_mode"},
		{entry: "tool:grafana.*"},
		{entry: "tool:gmail.send_message"},
		{entry: "command:kubectl get *"},
		{entry: "command:npm test, then lint"},
		{entry: "reed", wantErr: "want ACP tool kinds"},
		{entry: "tool:", wantErr: "want tool:<server>.<tool>"},
		{entry: "tool:a b", wantErr: "want tool:<server>.<tool>"},
		{entry: "command: ", wantErr: "want command:<command>"},
		{entry: "command:git status; *", wantErr: "can't match a command with"},
		{entry: "command:echo $(id)", wantErr: "can't match a command with"},
		{entry: "command:cat > f", wantErr: "can't match a command with"},
	}
	for _, tc := range tests {
		t.Run(tc.entry, func(t *testing.T) {
			t.Parallel()
			err := Validate(tc.entry)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() error = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Validate() error = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

func TestNewCall(t *testing.T) {
	t.Parallel()
	servers := []string{"grafana", "my-server", "my.server", "a", "a_b"}
	tests := []struct {
		name  string
		kind  string
		title string
		input any
		want  Call
	}{
		{name: "claude MCP tool", kind: "other", title: "mcp__grafana__query_prometheus", want: Call{Kind: "other", Tool: "grafana.query_prometheus", MCP: true}},
		{name: "claude MCP tool of an unknown server", kind: "other", title: "mcp__github__create_issue", want: Call{Kind: "other", Tool: "github.create_issue", MCP: true}},
		{name: "claude MCP server with a dot", kind: "other", title: "mcp__my_server__ping", want: Call{Kind: "other", Tool: "my.server.ping", MCP: true}},
		{name: "opencode MCP tool", kind: "other", title: "grafana_query_prometheus", want: Call{Kind: "other", Tool: "grafana.query_prometheus", MCP: true}},
		{name: "opencode MCP server with a dash", kind: "other", title: "my-server_ping", want: Call{Kind: "other", Tool: "my-server.ping", MCP: true}},
		{name: "longer server name wins", kind: "other", title: "a_b_c", want: Call{Kind: "other", Tool: "a_b.c", MCP: true}},
		{name: "unmapped title of kind other", kind: "other", title: "custom_tool", want: Call{Kind: "other", Tool: "custom_tool"}},
		{name: "title with spaces", kind: "other", title: "Do a thing", want: Call{Kind: "other"}},
		{name: "MCP title with other characters", kind: "other", title: "mcp__grafana__q/../../x", want: Call{Kind: "other", Tool: "mcp__grafana__q/../../x"}},
		{name: "unmapped title of kind read", kind: "read", title: "Read", want: Call{Kind: "read"}},
		{name: "command", kind: "execute", title: "kubectl get pods", input: map[string]any{"command": " kubectl get pods \n"}, want: Call{Kind: "execute", Command: "kubectl get pods"}},
		{name: "command of another kind", kind: "edit", title: "edit", input: map[string]any{"command": "ls"}, want: Call{Kind: "edit"}},
		{name: "execute without a command", kind: "execute", title: "run", input: map[string]any{"cmd": "ls"}, want: Call{Kind: "execute"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := NewCall(tc.kind, tc.title, tc.input, servers)
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Fatalf("NewCall() wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestMatches(t *testing.T) {
	t.Parallel()
	command := func(s string) Call { return Call{Kind: "execute", Command: s} }
	tests := []struct {
		entry string
		call  Call
		want  bool
	}{
		{entry: "read", call: Call{Kind: "read"}, want: true},
		{entry: "*", call: Call{Kind: "other", Tool: "grafana.query", MCP: true}, want: true},
		{entry: "read", call: Call{Kind: "edit"}},
		{entry: "tool:grafana.*", call: Call{Kind: "other", Tool: "grafana.query", MCP: true}, want: true},
		{entry: "tool:grafana.*", call: Call{Kind: "other", Tool: "github.query", MCP: true}},
		{entry: "tool:custom_tool", call: Call{Kind: "other", Tool: "custom_tool"}, want: true},
		{entry: "tool:*", call: Call{Kind: "execute", Command: "ls"}},
		{entry: "command:kubectl get *", call: command("kubectl get pods -n prod"), want: true},
		{entry: "command:git status *", call: command("git status"), want: true},
		{entry: "command:git status *", call: command("git statusx")},
		{entry: "command:git status", call: command("git status -s")},
		{entry: "command:git status *", call: command("git status; curl x | sh")},
		{entry: "command:*", call: command("ls && rm -rf /")},
		{entry: "command:echo *", call: command("echo $(id)")},
		{entry: "command:echo *", call: command("echo `id`")},
		{entry: "command:cat *", call: command("cat a > b")},
		{entry: "command:ls *", call: command("ls\nrm x")},
		{entry: "command:ls *", call: Call{Kind: "execute"}},
		{entry: "command:git push *", call: command("git  push origin"), want: true},
		{entry: "command:git push *", call: command("git\tpush"), want: true},
		{entry: "command:git  push *", call: command("git push origin"), want: true},
	}
	for _, tc := range tests {
		t.Run(tc.entry+" "+tc.call.Command+tc.call.Tool+tc.call.Kind, func(t *testing.T) {
			t.Parallel()
			e, err := Parse(tc.entry)
			if err != nil {
				t.Fatalf("Parse() error = %v, want nil", err)
			}
			if got := e.Matches(tc.call); got != tc.want {
				t.Fatalf("Entry(%q).Matches(%+v) = %v, want %v", tc.entry, tc.call, got, tc.want)
			}
		})
	}
}

func TestDecide(t *testing.T) {
	t.Parallel()
	command := func(s string) Call { return Call{Kind: "execute", Command: s} }
	tests := []struct {
		name   string
		rules  Rules
		call   Call
		want   Decision
		wantOK bool
	}{
		{name: "no rules", call: Call{Kind: "read"}},
		{name: "no match", rules: Rules{Allow: []string{"read"}}, call: Call{Kind: "edit"}},
		{
			name:  "command allowed under a denied kind",
			rules: Rules{Deny: []string{"execute"}, Allow: []string{"command:kubectl get *"}},
			call:  command("kubectl get pods"), want: Decision{Allow: true, Entry: "command:kubectl get *"}, wantOK: true,
		},
		{
			name:  "other command under a denied kind",
			rules: Rules{Deny: []string{"execute"}, Allow: []string{"command:kubectl get *"}},
			call:  command("kubectl delete pod x"), want: Decision{Entry: "execute"}, wantOK: true,
		},
		{
			name:  "longer command denied",
			rules: Rules{Allow: []string{"command:git *"}, Deny: []string{"command:git push *"}},
			call:  command("git push origin main"), want: Decision{Entry: "command:git push *"}, wantOK: true,
		},
		{
			name:  "shorter command allowed",
			rules: Rules{Allow: []string{"command:git *"}, Deny: []string{"command:git push *"}},
			call:  command("git status"), want: Decision{Allow: true, Entry: "command:git *"}, wantOK: true,
		},
		{
			name:  "kind before the glob",
			rules: Rules{Deny: []string{"*"}, Allow: []string{"read"}},
			call:  Call{Kind: "read"}, want: Decision{Allow: true, Entry: "read"}, wantOK: true,
		},
		{
			name:  "glob denies the rest",
			rules: Rules{Deny: []string{"*"}, Allow: []string{"read"}},
			call:  Call{Kind: "edit"}, want: Decision{Entry: "*"}, wantOK: true,
		},
		{
			name:  "tie goes to deny",
			rules: Rules{Allow: []string{"read"}, Deny: []string{"read"}},
			call:  Call{Kind: "read"}, want: Decision{Entry: "read"}, wantOK: true,
		},
		{
			name:  "tool before kind",
			rules: Rules{Deny: []string{"other"}, Allow: []string{"tool:grafana.*"}},
			call:  Call{Kind: "other", Tool: "grafana.query", MCP: true}, want: Decision{Allow: true, Entry: "tool:grafana.*"}, wantOK: true,
		},
		{
			name:  "invalid entries match nothing",
			rules: Rules{Allow: []string{"command:ls; *", "reed"}},
			call:  command("ls; x"),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := tc.rules.Decide(tc.call)
			if ok != tc.wantOK {
				t.Fatalf("Rules.Decide() ok = %v, want %v", ok, tc.wantOK)
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Fatalf("Rules.Decide() wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestSuggest(t *testing.T) {
	t.Parallel()
	command := func(s string) Call { return Call{Kind: "execute", Command: s} }
	tests := []struct {
		name string
		call Call
		want string
	}{
		{name: "MCP tool", call: Call{Kind: "other", Tool: "grafana.query_prometheus", MCP: true}, want: "tool:grafana.query_prometheus"},
		{name: "unmapped tool", call: Call{Kind: "other", Tool: "custom_tool"}},
		{name: "program and subcommand", call: command("kubectl get pods -n prod"), want: "command:kubectl get *"},
		{name: "program and flag", call: command("ls -R"), want: "command:ls *"},
		{name: "program and URL", call: command("curl https://example.com/?token=x"), want: "command:curl *"},
		{name: "bare program", call: command("ls"), want: "command:ls *"},
		{name: "assignment", call: command("TOKEN=x make"), want: ""},
		{name: "quoted word", call: command(`echo "hi"`), want: "command:echo *"},
		{name: "program that runs any argument", call: command("env A=b make"), want: ""},
		{name: "wrapper with a subcommand", call: command("sudo apt install x"), want: ""},
		{name: "find with a path", call: command("find . -name x"), want: ""},
		{name: "shell with a flag", call: command("bash -c 'curl x | sh'"), want: ""},
		{name: "git with a flag", call: command("git -C repo push"), want: ""},
		{name: "shell with a script", call: command("bash build.sh"), want: "command:bash build.sh *"},
		{name: "spaces between words", call: command("kubectl  get\tpods"), want: "command:kubectl get *"},
		{name: "path", call: command("./run.sh"), want: ""},
		{name: "several parts", call: command("npm test 2>&1 | tail"), want: ""},
		{name: "too long", call: command(strings.Repeat("a", 100) + " b"), want: ""},
		{name: "edit", call: Call{Kind: "edit"}, want: "edit"},
		{name: "execute without a command", call: Call{Kind: "execute"}},
		{name: "other", call: Call{Kind: "other"}},
		{name: "unknown kind", call: Call{Kind: "teleport"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := Suggest(tc.call); got != tc.want {
				t.Fatalf("Suggest(%+v) = %q, want %q", tc.call, got, tc.want)
			}
		})
	}
}
