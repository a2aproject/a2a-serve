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

package serve_test

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/a2aproject/a2a-serve/internal/sidecar/serve"
	"github.com/a2aproject/a2a-serve/internal/testutil"
)

func TestStartRefusesInvalidFlags(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"KEY": "secret-value"}
	valid := serve.Config{Agent: "opencode", Workspace: "dir:" + dir, Sandbox: serve.SandboxLocal}
	tests := []struct {
		name string
		edit func(*serve.Config)
		want string
	}{
		{name: "no agent", edit: func(c *serve.Config) { c.Agent = "" }, want: "no agent: pass --agent claude or --agent opencode"},
		{name: "unknown agent", edit: func(c *serve.Config) { c.Agent = "gemini" }, want: `unknown --agent "gemini"`},
		{name: "unparsable command", edit: func(c *serve.Config) { c.Command = `"unterminated` }, want: "invalid --command"},
		{name: "custom agent in docker without an image", edit: func(c *serve.Config) { c.Agent, c.Command, c.Sandbox = "", "fakeacp", "" }, want: "--command without --agent needs --image with --sandbox docker"},
		{name: "custom agent in gvisor without an image", edit: func(c *serve.Config) { c.Agent, c.Command, c.Sandbox = "", "fakeacp", "gvisor" }, want: "--command without --agent needs --image with --sandbox gvisor"},
		{name: "unknown sandbox", edit: func(c *serve.Config) { c.Sandbox = "podman" }, want: `invalid --sandbox "podman"`},
		{name: "egress with local", edit: func(c *serve.Config) { c.Egress = "allowlist" }, want: "--egress needs --sandbox docker"},
		{
			name: "egress rules and model keys with local",
			edit: func(c *serve.Config) { c.EgressAllow, c.ModelKeys = []string{"nas.lan"}, []string{"anthropic=KEY"} },
			want: "--egress-allow, --inject-model-key need --sandbox docker, gvisor or bwrap",
		},
		{name: "image with local", edit: func(c *serve.Config) { c.Image = "img" }, want: "--image needs --sandbox docker"},
		{name: "image with bwrap", edit: func(c *serve.Config) { c.Sandbox, c.Image = "bwrap", "img" }, want: "--image needs --sandbox docker or gvisor"},
		{name: "unknown egress mode", edit: func(c *serve.Config) { c.Sandbox, c.Egress = "", "unrestricted" }, want: `invalid --egress "unrestricted"`},
		{name: "invalid egress rule", edit: func(c *serve.Config) { c.Sandbox, c.EgressAllow = "", []string{"nas lan"} }, want: `invalid egress rule "nas lan"`},
		{name: "env without a value", edit: func(c *serve.Config) { c.Env = []string{"DEBUG"} }, want: `invalid --env "DEBUG": want NAME=value`},
		{name: "invalid secret name", edit: func(c *serve.Config) { c.Secrets = []string{"A-B"} }, want: `invalid --secret "A-B"`},
		{name: "unset secret", edit: func(c *serve.Config) { c.Secrets = []string{"MISSING"} }, want: "--secret MISSING: the variable MISSING is unset or empty"},
		{name: "model key without a variable", edit: func(c *serve.Config) { c.Sandbox, c.ModelKeys = "", []string{"anthropic"} }, want: `invalid --inject-model-key "anthropic": want provider=NAME`},
		{name: "unknown provider", edit: func(c *serve.Config) { c.Sandbox, c.ModelKeys = "", []string{"mistral=KEY"} }, want: `unknown model provider "mistral"`},
		{name: "provider twice", edit: func(c *serve.Config) { c.Sandbox, c.ModelKeys = "", []string{"openai=KEY", "openai=KEY"} }, want: "--inject-model-key names openai twice"},
		{name: "unset model key", edit: func(c *serve.Config) { c.Sandbox, c.ModelKeys = "", []string{"openai=MISSING"} }, want: "the variable MISSING is unset or empty"},
		{name: "egress middleware with local", edit: func(c *serve.Config) { c.EgressMiddleware = []string{"api.anthropic.com=http://127.0.0.1:9000"} }, want: "--egress-middleware needs --sandbox docker"},
		{name: "egress middleware without equals", edit: func(c *serve.Config) { c.Sandbox, c.EgressMiddleware = "", []string{"api.anthropic.com"} }, want: "invalid --egress-middleware: want host[:port]=URL"},
		{name: "egress middleware invalid host", edit: func(c *serve.Config) { c.Sandbox, c.EgressMiddleware = "", []string{"127.0.0.1=http://127.0.0.1:9000"} }, want: "invalid --egress-middleware: invalid egress middleware host"},
		{name: "egress middleware URL with credentials", edit: func(c *serve.Config) { c.Sandbox, c.EgressMiddleware = "", []string{"api.anthropic.com=http://u:secret-value@127.0.0.1:9000"} }, want: "the URL can't carry credentials"},
		{name: "egress middleware duplicate host", edit: func(c *serve.Config) { c.Sandbox, c.EgressMiddleware = "", []string{"api.anthropic.com=http://127.0.0.1:9000", "api.anthropic.com:443=http://127.0.0.1:9001"} }, want: "two egress middlewares for api.anthropic.com:443"},
		{name: "egress middleware overlaps model key", edit: func(c *serve.Config) { c.Sandbox, c.ModelKeys, c.EgressMiddleware = "", []string{"anthropic=KEY"}, []string{"api.anthropic.com=http://127.0.0.1:9000"} }, want: "--egress-middleware and --inject-model-key both name api.anthropic.com"},
		{name: "no workspace", edit: func(c *serve.Config) { c.Workspace = "" }, want: "no workspace: pass --workspace dir:<path>, --workspace memory, or the URL of your file server"},
		{name: "unknown workspace kind", edit: func(c *serve.Config) { c.Workspace = "nfs:/x" }, want: `invalid --workspace "nfs:/x": want dir:<path>, memory, or an http://, https:// or unix:///path URL`},
		{name: "workspace URL with user info", edit: func(c *serve.Config) { c.Workspace = "http://user:secret-value@127.0.0.1:1" }, want: "invalid --workspace: the file server URL can't have user info"},
		{name: "workspace URL on a relative socket", edit: func(c *serve.Config) { c.Workspace = "unix://fs.sock" }, want: "invalid --workspace: want the file server socket as unix:///path/to/socket"},
		{name: "workspace URL with a query", edit: func(c *serve.Config) { c.Workspace = "http://127.0.0.1:1/?token=secret-value" }, want: "invalid --workspace: the file server URL can't have user info, a query or a fragment"},
		{name: "unknown workspace sync", edit: func(c *serve.Config) { c.WorkspaceSync = "eager" }, want: `invalid --workspace-sync "eager": want turn or live`},
		{name: "live memory workspace with local", edit: func(c *serve.Config) { c.Workspace, c.WorkspaceSync = "memory", "live" }, want: "--workspace-sync live with --workspace memory needs --sandbox docker or bwrap, which mount the workspace in the sandbox; use --workspace-sync turn"},
		{name: "live URL workspace with local", edit: func(c *serve.Config) { c.Workspace, c.WorkspaceSync = "http://127.0.0.1:1/secret-value", "live" }, want: "--workspace-sync live with --workspace http://127.0.0.1:1 needs --sandbox docker"},
		{name: "missing workspace", edit: func(c *serve.Config) { c.Workspace = "dir:" + filepath.Join(dir, "missing") }, want: "no such file or directory"},
		{name: "workspace that is a file", edit: func(c *serve.Config) { c.Workspace = "dir:" + file }, want: "isn't a directory"},
		{name: "home directory as the workspace in docker", edit: func(c *serve.Config) { c.Sandbox, c.Workspace = "", "dir:"+home }, want: "holds your home directory"},
		{name: "home directory as a live workspace in docker", edit: func(c *serve.Config) { c.Sandbox, c.Workspace, c.WorkspaceSync = "", "dir:"+home, "live" }, want: "holds your home directory"},
		{name: "relative socket", edit: func(c *serve.Config) { c.Listen = "unix://a.sock" }, want: "want unix:///absolute/path"},
		{name: "address without a port", edit: func(c *serve.Config) { c.Listen = "127.0.0.1" }, want: "want unix:///path or host:port"},
		{name: "invalid port", edit: func(c *serve.Config) { c.Listen = "127.0.0.1:99999" }, want: "the port isn't a number from 0 to 65535"},
		{name: "all interfaces", edit: func(c *serve.Config) { c.Listen = ":8080" }, want: "listens only on a loopback address"},
		{name: "public address", edit: func(c *serve.Config) { c.Listen = "192.0.2.1:8080" }, want: "listens only on a loopback address"},
		{name: "tools that aren't a URL", edit: func(c *serve.Config) { c.Tools = "localhost:9000" }, want: "invalid --tools: want an http:// or https:// URL"},
		{name: "tools on a relative socket", edit: func(c *serve.Config) { c.Tools = "unix://tools.sock" }, want: "invalid --tools: want unix:///absolute/path"},
		{name: "tools on a socket with local", edit: func(c *serve.Config) { c.Tools = "unix:///run/tools.sock" }, want: "--tools unix:// needs --sandbox docker"},
		{name: "too many processes", edit: func(c *serve.Config) { c.MaxProcesses = 65 }, want: "invalid --max-processes 65: want 1 to 64"},
		{name: "negative processes", edit: func(c *serve.Config) { c.MaxProcesses = -1 }, want: "invalid --max-processes -1"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := valid
			cfg.Getenv = func(name string) string { return env[name] }
			tc.edit(&cfg)

			srv, err := serve.Start(context.Background(), cfg)

			if srv != nil {
				t.Errorf("serve.Start() = %v, want nil", srv)
				if err := srv.Close(context.Background()); err != nil {
					t.Errorf("Server.Close() error = %v", err)
				}
			}
			if _, ok := errors.AsType[*serve.UsageError](err); !ok || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("serve.Start() error = %v, want a UsageError with %q", err, tc.want)
			}
			if strings.Contains(err.Error(), env["KEY"]) {
				t.Fatalf("serve.Start() error = %v, want it without the value of KEY", err)
			}
		})
	}
}

func TestStartFailsWhenTheWorkspaceServerDoesntAnswer(t *testing.T) {
	t.Parallel()
	sock := filepath.Join(testutil.ShortTempDir(t), "missing.sock")
	tests := []struct {
		name, workspace, want string
	}{
		{name: "socket", workspace: "unix://" + sock, want: "the file server of --workspace unix://" + sock + " doesn't answer"},
		{name: "secret in the path", workspace: "http://127.0.0.1:1/secret-value", want: "the file server of --workspace http://127.0.0.1:1 doesn't answer"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := serve.Start(context.Background(), serve.Config{Agent: "opencode", Workspace: tc.workspace, Sandbox: serve.SandboxLocal})

			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("serve.Start() error = %v, want %q", err, tc.want)
			}
			if _, ok := errors.AsType[*serve.UsageError](err); ok {
				t.Fatalf("serve.Start() error = %v, want an error other than a UsageError", err)
			}
			if strings.Contains(err.Error(), "secret-value") {
				t.Fatalf("serve.Start() error = %v, want it without the path of the URL", err)
			}
		})
	}
}

func TestStartRefusesASocketInUse(t *testing.T) {
	t.Parallel()
	sock := filepath.Join(testutil.ShortTempDir(t), "a.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := ln.Close(); err != nil {
			t.Error(err)
		}
	})

	_, err = serve.Start(context.Background(), serve.Config{Agent: "opencode", Workspace: "dir:" + t.TempDir(), Sandbox: serve.SandboxLocal, Listen: "unix://" + sock})

	if _, ok := errors.AsType[*serve.UsageError](err); !ok || !strings.Contains(err.Error(), "another process listens on it") {
		t.Fatalf("serve.Start() error = %v, want a UsageError for a socket in use", err)
	}
}
