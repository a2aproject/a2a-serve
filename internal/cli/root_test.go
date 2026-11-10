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
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/shlex"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/clierr"
	"github.com/a2aproject/a2a-serve/internal/credstore"
	"github.com/a2aproject/a2a-serve/internal/output"
)

func TestVerboseEnablesDebugLogs(t *testing.T) {
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })
	dir := newTestAgent(t, &agentdir.Config{Name: "coder", Harness: "opencode"}).Root
	tests := []struct {
		name string
		args []string
		want bool
	}{
		{name: "without -v", args: []string{"version"}, want: false},
		{name: "-v", args: []string{"-v", "version"}, want: true},
		{name: "--verbose on a command with its own pre-run", args: []string{"config", "show", "--verbose", "--dir", dir}, want: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelInfo})))
			if _, err := runCMD(t, tc.args...); err != nil {
				t.Fatalf("a2a-serve %s error = %v", strings.Join(tc.args, " "), err)
			}
			if got := slog.Default().Enabled(t.Context(), slog.LevelDebug); got != tc.want {
				t.Fatalf("debug logs enabled after a2a-serve %s = %v, want %v", strings.Join(tc.args, " "), got, tc.want)
			}
		})
	}
}

func TestUsageHints(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		file    string
		noAgent bool
		args    []string
		want    string
	}{
		{name: "unknown flag of a subcommand", args: []string{"pair", "rm", "--bogus"}, want: "run `a2a-serve pair rm --help` for its flags"},
		{name: "unknown flag of a command", args: []string{"status", "--bogus"}, want: "run `a2a-serve status --help` for its flags"},
		{name: "config set flag on start", args: []string{"start", "opencode", "--dir", "{dir}", "--tailscale-authkey", "x"}, want: "--tailscale-authkey is a setting of `a2a-serve config set`: run `a2a-serve config set --tailscale-authkey <value> --dir {dir}`, then `a2a-serve start opencode --dir {dir}`"},
		{name: "config set flag with a value on start", args: []string{"start", "claude", "--dir", "{dir}", "--memory=8g"}, want: "--memory is a setting of `a2a-serve config set`: run `a2a-serve config set --memory <value> --dir {dir}`, then `a2a-serve start claude --dir {dir}`"},
		{name: "config set flag on start of a new agent", noAgent: true, args: []string{"start", "opencode", "--dir", "{dir}", "--tailscale-authkey", "x"}, want: "--tailscale-authkey is a setting of `a2a-serve config set`: run `a2a-serve config set --agent opencode --tailscale-authkey <value> --dir {dir}` to create the agent, then `a2a-serve start opencode --dir {dir}`"},
		{name: "config set flag on start without an agent", noAgent: true, args: []string{"start", "--dir", "{dir}", "--memory", "8g"}, want: "--memory is a setting of `a2a-serve config set`: run `a2a-serve config set --agent <agent> --memory <value> --dir {dir}` to create the agent, then `a2a-serve start --dir {dir}`"},
		{name: "unknown command", args: []string{"bogus"}, want: clierr.UsageHint},
		{name: "unknown pairing", args: []string{"pair", "rm", "nobody", "--dir", "{dir}"}, want: "run `a2a-serve pair list` to see the pairings"},
		{name: "unknown contact", args: []string{"contact", "rm", "nobody", "--dir", "{dir}"}, want: "run `a2a-serve contact list` to see the contacts"},
		{name: "unknown messenger", args: []string{"messenger", "rm", "telegram", "--dir", "{dir}"}, want: "run `a2a-serve messenger list` to see the messengers"},
		{name: "invalid setting", args: []string{"config", "unset", "nope", "--dir", "{dir}"}, want: "run `a2a-serve config unset --help` for its flags"},
		{name: "invalid flag value", args: []string{"config", "set", "--deny-tools", "exec", "--dir", "{dir}"}, want: "run `a2a-serve config set --help` for its flags"},
		{
			name: "invalid entry in the file",
			file: "name: coder\nagent: opencode\napprovals:\n    deny: [exec]\n",
			args: []string{"config", "set", "--deny-tools", "execute", "--dir", "{dir}"},
			want: "fix it with `a2a-serve config unset deny-tools exec`",
		},
		{
			name: "invalid file on start",
			file: "name: coder\nagent: opencode\nsandbox: vm\n",
			args: []string{"start", "--dir", "{dir}"},
			want: "fix it with `a2a-serve config unset sandbox`",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			conf := &agentdir.Config{Name: "coder", Harness: "opencode"}
			if tc.noAgent {
				conf = nil
			}
			d := newTestAgent(t, conf)
			if tc.file != "" {
				if err := os.WriteFile(d.ConfigPath(), []byte(tc.file), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			args := slices.Clone(tc.args)
			for i, a := range args {
				if a == "{dir}" {
					args[i] = d.Root
				}
			}
			want := strings.ReplaceAll(tc.want, "{dir}", d.Root)
			cfg := &globalConfig{credentials: &credstore.Store{Path: filepath.Join(t.TempDir(), "credentials.json")}}
			cfg.Printer = output.NewPrinter(io.Discard, output.ModeText)
			root := newRootCmd(cfg)
			root.SetOut(io.Discard)
			root.SetArgs(args)
			cmd, err := root.ExecuteC()
			if err == nil {
				t.Fatalf("a2a-serve %s error = nil, want a usage error", strings.Join(tc.args, " "))
			}
			got := classify(err, cmd)
			if got.Exit != 2 {
				t.Fatalf("a2a-serve %s exit = %d, want 2", strings.Join(tc.args, " "), got.Exit)
			}
			if got.Hint != want {
				t.Fatalf("a2a-serve %s hint = %q, want %q", strings.Join(tc.args, " "), got.Hint, want)
			}
		})
	}
}

func TestConfigSetOnlyHintCommandRuns(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		args []string
	}{
		{name: "harness", args: []string{"start", "opencode", "--dir", "{dir}", "--tailscale-authkey", "x"}},
		{name: "without a harness", args: []string{"start", "--dir", "{dir}", "--tailscale-authkey", "x"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := newTestAgent(t, nil)
			args := slices.Clone(tc.args)
			args[slices.Index(args, "{dir}")] = d.Root
			cfg := &globalConfig{credentials: &credstore.Store{Path: filepath.Join(t.TempDir(), "credentials.json")}}
			cfg.Printer = output.NewPrinter(io.Discard, output.ModeText)
			root := newRootCmd(cfg)
			root.SetOut(io.Discard)
			root.SetArgs(args)
			cmd, err := root.ExecuteC()
			if err == nil {
				t.Fatalf("a2a-serve %s error = nil, want an unknown flag", strings.Join(args, " "))
			}
			hint := classify(err, cmd).Hint
			_, rest, _ := strings.Cut(hint, "run `")
			printed, _, _ := strings.Cut(rest, "`")
			words, err := shlex.Split(printed)
			if err != nil {
				t.Fatalf("shlex.Split(%q) error = %v", printed, err)
			}
			for i, w := range words {
				switch w {
				case "<value>":
					words[i] = "secret"
				case "<agent>":
					words[i] = "opencode"
				}
			}
			if words[0] != "a2a-serve" {
				t.Fatalf("hint command = %q, want a2a-serve command", printed)
			}
			if _, err := runCMD(t, words[1:]...); err != nil {
				t.Fatalf("%s error = %v", printed, err)
			}
			if !d.Exists() {
				t.Fatalf("%s left no agent in %s", printed, d.Root)
			}
		})
	}
}

func TestUnknownCommandSuggestions(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		args     []string
		wantErr  string
		wantHint string
	}{
		{
			name: "root", args: []string{"stat"},
			wantErr: `unknown command "stat" for "a2a-serve"; did you mean start, status, stop?`, wantHint: clierr.UsageHint,
		},
		{
			name: "root typo", args: []string{"statsu"},
			wantErr: `unknown command "statsu" for "a2a-serve"; did you mean status?`, wantHint: clierr.UsageHint,
		},
		{
			name: "group", args: []string{"config", "sett"},
			wantErr: `unknown command "sett" for "a2a-serve config"; did you mean set?`, wantHint: "run `a2a-serve config --help` for its flags",
		},
		{
			name: "no close command", args: []string{"zzzzzzzz"},
			wantErr: `unknown command "zzzzzzzz" for "a2a-serve"`, wantHint: clierr.UsageHint,
		},
		{
			name: "other group", args: []string{"pair", "lst"},
			wantErr: `unknown command "lst" for "a2a-serve pair"; did you mean get, list, set?`, wantHint: "run `a2a-serve pair --help` for its flags",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := &globalConfig{}
			cfg.Printer = output.NewPrinter(io.Discard, output.ModeText)
			root := newRootCmd(cfg)
			root.SetOut(io.Discard)
			root.SetArgs(tc.args)

			cmd, err := root.ExecuteC()

			if err == nil {
				t.Fatalf("a2a-serve %s error = nil, want a usage error", strings.Join(tc.args, " "))
			}
			got := classify(err, cmd)
			if diff := cmp.Diff([]any{tc.wantErr, tc.wantHint, 2}, []any{got.Message, got.Hint, got.Exit}); diff != "" {
				t.Fatalf("a2a-serve %s wrong message, hint and exit (-want +got) diff = %s", strings.Join(tc.args, " "), diff)
			}
		})
	}
}
