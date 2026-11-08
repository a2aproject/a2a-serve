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

package harness

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
)

// preparedDocker returns the mounts, the env file lines and the command of
// a prepared docker harness.
func preparedDocker(t *testing.T, p *Prepared) (mounts, env, command []string) {
	t.Helper()
	var envFile string
	for i := 0; i+1 < len(p.Args); i++ {
		switch p.Args[i] {
		case "-v":
			mounts = append(mounts, p.Args[i+1])
		case "--env-file":
			envFile = p.Args[i+1]
		}
	}
	data, err := os.ReadFile(envFile)
	if err != nil {
		t.Fatalf("os.ReadFile() error = %v", err)
	}
	env = strings.Split(strings.TrimSpace(string(data)), "\n")
	command = p.Args[slices.Index(p.Args, "a2a-image")+1:]
	return mounts, env, command
}

func TestPrepareDockerRoutesModelKeys(t *testing.T) {
	fakeDocker(t, runningDocker)
	const forwarded = "http://127.0.0.1:47413/_model/"
	tests := []struct {
		name         string
		profile      Profile
		env          []string
		socket       bool
		route        func(sock string) *ModelRoute
		wantMount    bool
		wantEnv      []string
		wantKeys     []string
		wantWarnings []string
		wantFwd      bool
	}{
		{
			name:   "socket of the agent",
			env:    []string{"GEMINI_API_KEY=real", "GITHUB_TOKEN=gh"},
			socket: true,
			route: func(sock string) *ModelRoute {
				return &ModelRoute{Keys: []string{"GEMINI_API_KEY"}, Socket: sock, Key: "placeholder"}
			},
			wantEnv: []string{
				"GEMINI_API_KEY=placeholder",
				"GITHUB_TOKEN=gh",
				"GOOGLE_GEMINI_BASE_URL=" + forwarded + "google",
				`OPENCODE_CONFIG_CONTENT={"experimental":{"continue_loop_on_deny":true},"provider":{"google":{"options":{"apiKey":"placeholder","baseURL":"` + forwarded + `google/v1beta"}}}}`,
			},
			wantKeys: []string{"GEMINI_API_KEY"},
			wantFwd:  true,
		},
		{
			name: "socket only for the model routes",
			env:  []string{"OPENAI_API_KEY=real"},
			route: func(sock string) *ModelRoute {
				return &ModelRoute{Keys: []string{"OPENAI_API_KEY"}, Socket: sock, Key: "placeholder"}
			},
			wantMount: true,
			wantEnv: []string{
				"OPENAI_API_KEY=placeholder",
				"OPENAI_BASE_URL=" + forwarded + "openai/v1",
				`OPENCODE_CONFIG_CONTENT={"experimental":{"continue_loop_on_deny":true},"provider":{"openai":{"options":{"apiKey":"placeholder","baseURL":"` + forwarded + `openai/v1"}}}}`,
			},
			wantKeys: []string{"OPENAI_API_KEY"},
			wantFwd:  true,
		},
		{
			name: "TCP",
			env:  []string{"ANTHROPIC_API_KEY=real", "OPENROUTER_API_KEY=real2"},
			route: func(string) *ModelRoute {
				return &ModelRoute{Keys: []string{"ANTHROPIC_API_KEY", "OPENROUTER_API_KEY"}, URL: "http://host.docker.internal:1234", Key: "agent-key"}
			},
			wantEnv: []string{
				"ANTHROPIC_API_KEY=agent-key",
				"OPENROUTER_API_KEY=agent-key",
				"ANTHROPIC_BASE_URL=http://host.docker.internal:1234/_model/anthropic",
				`OPENCODE_CONFIG_CONTENT={"experimental":{"continue_loop_on_deny":true},"provider":{"anthropic":{"options":{"apiKey":"agent-key","baseURL":"http://host.docker.internal:1234/_model/anthropic/v1"}},"openrouter":{"options":{"apiKey":"agent-key","baseURL":"http://host.docker.internal:1234/_model/openrouter/api/v1"}}}}`,
			},
			wantKeys: []string{"ANTHROPIC_API_KEY", "OPENROUTER_API_KEY"},
		},
		{
			name: "own base URL and keys that aren't secrets",
			env:  []string{"OPENAI_API_KEY=real", "OPENAI_BASE_URL=http://llm.local/v1", "GEMINI_API_KEY=plain"},
			route: func(string) *ModelRoute {
				return &ModelRoute{Keys: []string{"OPENAI_API_KEY"}, URL: "http://host.docker.internal:1234", Key: "agent-key"}
			},
			wantEnv: []string{
				"OPENAI_API_KEY=real",
				"OPENAI_BASE_URL=http://llm.local/v1",
				"GEMINI_API_KEY=plain",
				`OPENCODE_CONFIG_CONTENT={"experimental":{"continue_loop_on_deny":true}}`,
			},
			wantWarnings: []string{"OPENAI_API_KEY is readable in the sandbox, since OPENAI_BASE_URL sends its requests to another server"},
		},
		{
			name:    "claude takes only the base URL of anthropic",
			profile: claude{},
			env:     []string{"ANTHROPIC_API_KEY=real", "OPENAI_API_KEY=real2"},
			route: func(string) *ModelRoute {
				return &ModelRoute{Keys: []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY"}, URL: "http://host.docker.internal:1234", Key: "agent-key"}
			},
			wantEnv: []string{
				"ANTHROPIC_API_KEY=agent-key",
				"OPENAI_API_KEY=real2",
				"ANTHROPIC_BASE_URL=http://host.docker.internal:1234/_model/anthropic",
			},
			wantKeys: []string{"ANTHROPIC_API_KEY"},
		},
		{
			name:    "agent without a profile",
			profile: custom{name: "aider", argv: []string{"aider"}},
			env:     []string{"OPENROUTER_API_KEY=real"},
			route: func(string) *ModelRoute {
				return &ModelRoute{Keys: []string{"OPENROUTER_API_KEY"}, URL: "http://host.docker.internal:1234", Key: "agent-key"}
			},
			wantEnv: []string{"OPENROUTER_API_KEY=real"},
		},
		{
			name:    "no route",
			env:     []string{"OPENAI_API_KEY=real"},
			route:   func(string) *ModelRoute { return nil },
			wantEnv: []string{"OPENAI_API_KEY=real", `OPENCODE_CONFIG_CONTENT={"experimental":{"continue_loop_on_deny":true}}`},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ws, home, sockDir := t.TempDir(), t.TempDir(), t.TempDir()
			sock := filepath.Join(sockDir, "a2a.sock")
			profile := tc.profile
			if profile == nil {
				profile = opencode{}
			}
			spec := Spec{
				Profile: profile,
				Mounts:  []agentdir.Mount{{Src: ws, Dst: ws}},
				Home:    home,
				Name:    "a2a-test",
				Image:   "a2a-image",
				Env:     tc.env,
				Models:  tc.route(sock),
			}
			if tc.socket {
				spec.Socket = sock
			}
			p, err := Prepare(context.Background(), spec)
			if err != nil {
				t.Fatalf("Prepare() error = %v", err)
			}
			t.Cleanup(func() {
				if err := p.Stop(context.Background()); err != nil {
					t.Errorf("Stop() error = %v", err)
				}
			})
			mounts, env, command := preparedDocker(t, p)
			if diff := cmp.Diff(tc.wantKeys, p.ModelKeys); diff != "" {
				t.Fatalf("Prepare() model keys wrong result (-want +got) diff = %s", diff)
			}
			if diff := cmp.Diff(tc.wantWarnings, p.Warnings); diff != "" {
				t.Fatalf("Prepare() warnings wrong result (-want +got) diff = %s", diff)
			}
			var gotEnv []string
			for _, kv := range env {
				if !strings.HasPrefix(kv, EnvTimeout+"=") && !strings.HasPrefix(kv, EnvUnixSocket+"=") && !strings.Contains(kv, "PERMISSION=") && !strings.HasPrefix(kv, "CLOUDCODE_CONFIG_CONTENT=") {
					gotEnv = append(gotEnv, kv)
				}
			}
			if diff := cmp.Diff(tc.wantEnv, gotEnv); diff != "" {
				t.Fatalf("Prepare() env wrong result (-want +got) diff = %s", diff)
			}
			if got := slices.Contains(mounts, sockDir+":"+SandboxDir+":ro"); got != (tc.socket || tc.wantMount) {
				t.Fatalf("Prepare() mounts = %v, socket mounted = %v, want %v", mounts, got, tc.socket || tc.wantMount)
			}
			if got := slices.ContainsFunc(env, func(kv string) bool { return strings.HasPrefix(kv, EnvUnixSocket+"=") }); got != tc.socket {
				t.Fatalf("Prepare() env = %v, has %s = %v, want %v", env, EnvUnixSocket, got, tc.socket)
			}
			want := profile.Command(Workspace{Dir: ws}).Args
			want = append([]string{profile.Command(Workspace{Dir: ws}).Path}, want...)
			if tc.wantFwd {
				want = append([]string{"sh", "-c", forwardScript, "sh", modelForwarder, SandboxDir + "/a2a.sock", "47413"}, want...)
			}
			if diff := cmp.Diff(want, command); diff != "" {
				t.Fatalf("Prepare() command wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestPrepareLocalPassesModelKeys(t *testing.T) {
	t.Parallel()
	ws := t.TempDir()
	p, err := Prepare(context.Background(), Spec{
		Profile: opencode{},
		Sandbox: SandboxLocal,
		Mounts:  []agentdir.Mount{{Src: ws, Dst: ws}},
		Home:    t.TempDir(),
		Env:     []string{"OPENAI_API_KEY=real"},
		Models:  &ModelRoute{Keys: []string{"OPENAI_API_KEY"}, URL: "http://127.0.0.1:1", Key: "k"},
	})
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if !slices.Contains(p.Env, "OPENAI_API_KEY=real") || len(p.ModelKeys) > 0 {
		t.Fatalf("Prepare() env = %v, model keys = %v, want the key itself", p.Env, p.ModelKeys)
	}
}

func TestModelForwarder(t *testing.T) {
	t.Parallel()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("no node")
	}
	sock := filepath.Join(t.TempDir(), "a2a.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("net.Listen() error = %v", err)
	}
	t.Cleanup(func() {
		if err := ln.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				line, err := bufio.NewReader(c).ReadString('\n')
				if err == nil {
					_, err = io.WriteString(c, "echo: "+line)
				}
				if err := errors.Join(err, c.Close()); err != nil {
					t.Errorf("echo server: %v", err)
				}
			}()
		}
	}()
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen() error = %v", err)
	}
	port := probe.Addr().(*net.TCPAddr).Port
	if err := probe.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, node, "-e", modelForwarder, sock, strconv.Itoa(port))
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting the forwarder: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		if err := cmd.Wait(); err != nil && ctx.Err() == nil {
			t.Errorf("forwarder: %v", err)
		}
	})
	var c net.Conn
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		if c, err = net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port))); err == nil || time.Now().After(deadline) {
			break
		}
	}
	if err != nil {
		t.Fatalf("net.Dial() error = %v", err)
	}
	defer func() {
		if err := c.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	}()
	if _, err := io.WriteString(c, "ping\n"); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	got, err := io.ReadAll(c)
	if err != nil {
		t.Fatalf("ReadAll() error = %v", err)
	}
	if string(got) != "echo: ping\n" {
		t.Fatalf("forwarded reply = %q, want %q", got, "echo: ping\n")
	}
}
