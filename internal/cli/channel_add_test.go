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
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/output"
)

func TestChannelAddTokenFromStdin(t *testing.T) {
	t.Parallel()
	d := newTestAgent(t, &agentdir.Config{Name: "coder", Harness: "opencode"})

	_, err := runCMDWithStdin(t, pipeWith(t, ""), "channel", "add", "telegram", "--token", "-", "--dir", d.Root)

	if !isUsage(err) || !strings.Contains(err.Error(), "--token: empty secret") {
		t.Fatalf("channel add telegram --token - with empty stdin error = %v, want a usage error %q", err, "--token: empty secret")
	}
}

func TestChannelAddEmail(t *testing.T) {
	t.Parallel()
	d := newTestAgent(t, &agentdir.Config{Name: "coder", Harness: "opencode"})

	_, err := runCMD(t, "channel", "add", "email", "--dir", d.Root,
		"--email-address", "coder@example.com", "--email-imap", "127.0.0.1:1", "--email-smtp", "127.0.0.1:2",
		"--email-username", "coder", "--email-password", "pw", "--email-trusted-authserv", "mx.example.com")

	if err == nil || !strings.Contains(err.Error(), "IMAP 127.0.0.1:1") {
		t.Fatalf("channel add email error = %v, want the IMAP login of the given server to fail", err)
	}
}

func TestSecretFlagsReadStdin(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{name: "channel add", args: []string{"channel", "add", "telegram", "--token", "-"}, wantErr: "--token: empty secret"},
		{name: "contact add", args: []string{"contact", "add", "grok", "--url", "https://grok.example.com", "--token", "-"}, wantErr: "--token: empty secret"},
		{name: "mailbox list", args: []string{"mailbox", "list", "--token", "-"}, wantErr: "--token: empty secret"},
		{name: "backup restore", args: []string{"backup", "restore", "/nonexistent-a2a-backup", "--backup-key", "-"}, wantErr: "--backup-key: empty secret"},
		{name: "pair invite webhook header", args: []string{"pair", "invite", "grok", "--webhook", "https://example.com/hook", "--webhook-header", "Authorization: Bearer -"}, wantErr: "--webhook-header Authorization: empty secret"},
		{name: "channel add email", args: []string{"channel", "add", "email", "--email-password", "-"}, wantErr: "--email-password: empty secret"},
		{name: "config set", args: []string{"config", "set", "--relay-token", "-"}, wantErr: "--relay-token: empty secret"},
		{name: "proxy auth", args: []string{"start", "proxy", "https://agent.example.com", "--auth", "Bearer -"}, wantErr: "--auth Authorization: empty secret"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := newTestAgent(t, &agentdir.Config{Name: "coder", Harness: "opencode"})

			args := tc.args
			if tc.args[0] != "mailbox" && tc.args[1] != "proxy" {
				args = append(args, "--dir", d.Root)
			}
			_, err := runCMDWithStdin(t, pipeWith(t, ""), args...)

			if !isUsage(err) || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("a2a-serve %s with empty stdin error = %v, want a usage error %q", strings.Join(tc.args, " "), err, tc.wantErr)
			}
		})
	}
}

func TestPrintChannelSaved(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		mode output.Mode
		want string
	}{
		{name: "text", mode: output.ModeText, want: "✓ saved telegram; it starts with the agent (a2a-serve start)\n"},
		{name: "json", mode: output.ModeJson, want: "{\n  \"name\": \"telegram\"\n}\n"},
		{name: "jsonl", mode: output.ModeJSONL, want: "{\"name\":\"telegram\"}\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			cfg := &globalConfig{Printer: output.NewPrinter(&buf, tc.mode)}

			err := printChannelSaved(cfg, "telegram")

			if err != nil {
				t.Fatalf("printChannelSaved() error = %v", err)
			}
			if diff := cmp.Diff(tc.want, buf.String()); diff != "" {
				t.Fatalf("printChannelSaved() wrong output (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestGlobalConfigProgress(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		mode       output.Mode
		wantStdout string
		wantStderr string
	}{
		{name: "text", mode: output.ModeText, wantStdout: "working\n"},
		{name: "json", mode: output.ModeJson, wantStderr: "working\n"},
		{name: "jsonl", mode: output.ModeJSONL, wantStderr: "working\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var stdout, stderr bytes.Buffer
			cfg := &globalConfig{Printer: output.NewPrinter(&stdout, tc.mode), errOut: &stderr}

			if _, err := fmt.Fprintln(cfg.progress(), "working"); err != nil {
				t.Fatalf("Fprintln() error = %v", err)
			}

			if diff := cmp.Diff([]string{tc.wantStdout, tc.wantStderr}, []string{stdout.String(), stderr.String()}); diff != "" {
				t.Fatalf("cfg.progress() wrong stdout and stderr (-want +got) diff = %s", diff)
			}
		})
	}
}
