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

package sidecar_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"

	"github.com/a2aproject/a2a-serve/core/fsys"
	"github.com/a2aproject/a2a-serve/core/sandbox"
	"github.com/a2aproject/a2a-serve/internal/sandboxrt"
	"github.com/a2aproject/a2a-serve/internal/sidecar/hosttools"
	"github.com/a2aproject/a2a-serve/internal/sidecar/refhost"
	"github.com/a2aproject/a2a-serve/internal/sidecar/serve"
	"github.com/a2aproject/a2a-serve/internal/testutil"
)

func TestToolsReachTheHostThroughTheBoundary(t *testing.T) {
	t.Parallel()
	bin, image := dockerTestEnv(t)
	tests := []struct {
		name        string
		egressProxy bool
		wantEgress  string
	}{
		{name: "egress policy", wantEgress: serve.EgressPolicy},
		{name: "egress proxy", egressProxy: true, wantEgress: serve.EgressProxy},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tools := refhost.NewTools(refhost.ToolsConfig{Hooks: hosttools.HookSupport{BeforeTool: true, AfterTool: true}})
			host := refhost.New(refhost.Config{Tools: tools})
			dir := testutil.ShortTempDir(t)
			url := serveHost(t, host, dir)
			sock := filepath.Join(dir, "a2a.sock")
			args := []string{"--sandbox", "docker", "--image", image, "--command", "/usr/local/bin/fakeacp",
				"--workspace", "dir:" + t.TempDir(), "--tools", url, "--listen", "unix://" + sock, "--max-processes", "1"}
			if tc.egressProxy {
				args = append(args, "--egress-proxy", url)
			} else {
				args = append(args, "--egress", "allowlist")
			}
			if ready := startBinary(t, bin, args...); ready.Egress != tc.wantEgress {
				t.Fatalf("ready line egress = %q, want %q", ready.Egress, tc.wantEgress)
			}
			c, err := a2aclient.NewFromEndpoints(context.Background(), []*a2a.AgentInterface{a2a.NewAgentInterface("http://a2a/", a2a.TransportProtocolJSONRPC)}, a2aclient.WithJSONRPCTransport(unixClient(sock)))
			if err != nil {
				t.Fatalf("a2aclient.NewFromEndpoints() error = %v", err)
			}

			task := sendTask(t, c, "context-a", "mcpcall host echo through the boundary")
			refused := sendTask(t, c, "context-a", "dial "+serve.ToolsHost+":81")
			otherPath := sendTask(t, c, "context-a", "fetch http://"+serve.ToolsHost+"/admin")

			if got, want := testutil.AllArtifactText(task), "mcp result: echo: through the boundary"; got != want {
				t.Fatalf("Client.SendMessage(mcpcall) = %q, want %q", got, want)
			}
			if got := len(tools.Records("tools/call")); got != 1 {
				t.Fatalf("host got %d tools/call, want 1", got)
			}
			if got := len(tools.Records(hosttools.MethodBeforeTool)); got != 1 {
				t.Fatalf("host got %d hooks/beforeTool, want 1", got)
			}
			waitRecords(t, tools, hosttools.MethodAfterTool, 1)
			if text := testutil.AllArtifactText(refused); !strings.Contains(text, "dial error") {
				t.Fatalf("Client.SendMessage(dial %s:81) = %q, want a dial error", serve.ToolsHost, text)
			}
			if text := testutil.AllArtifactText(otherPath); !strings.HasPrefix(text, "fetch 403") {
				t.Fatalf("Client.SendMessage(fetch http://%s/admin) = %q, want 403", serve.ToolsHost, text)
			}
			for _, c := range host.Connects() {
				if strings.HasPrefix(c.Target, serve.ToolsHost) {
					t.Fatalf("the egress proxy got CONNECT %s, want the tools endpoint's traffic kept from it", c.Target)
				}
			}
			for _, r := range host.Requests() {
				if strings.Contains(r.URL, serve.ToolsHost) {
					t.Fatalf("the egress proxy got the request %s, want the tools endpoint's traffic kept from it", r.URL)
				}
			}
		})
	}
}

func TestLiveWorkspaceInDockerShowsTheAgentsWritesBeforeTheTurnEnds(t *testing.T) {
	t.Parallel()
	bin, image := dockerTestEnv(t)
	ctx := context.Background()
	wantProfile := os.Getenv(envLiveAppArmor)
	profile, profileErr := sandboxrt.LiveDockerAppArmor(ctx, sandboxrt.AppArmorDir)
	if profileErr != nil && !errors.Is(profileErr, sandboxrt.ErrLiveAppArmorNotLoaded) {
		t.Fatalf("sandboxrt.LiveDockerAppArmor() error = %v", profileErr)
	}
	if wantProfile != "" && profile != wantProfile {
		t.Fatalf("sandboxrt.LiveDockerAppArmor() = %q, %v, want %q", profile, profileErr, wantProfile)
	}
	caps, err := sandbox.Docker(sandbox.DockerOptions{Launcher: bin, ServedAppArmor: profile}).Probe(ctx)
	if err != nil {
		t.Fatalf("Docker().Probe() error = %v", err)
	}
	served := slices.Contains(caps.Mounts, sandbox.MountServed)
	if wantProfile != "" && !served {
		t.Fatalf("Docker(ServedAppArmor: %q).Probe() doesn't support served mounts: %s", profile, caps.MountProblems[sandbox.MountServed])
	}
	tests := []struct {
		name string
		// workspace returns the --workspace value, and a function that
		// reads hello.txt as the host sees it.
		workspace func(t *testing.T) (string, func(*testing.T, *a2aclient.Client) string)
	}{
		{
			name: "url",
			workspace: func(t *testing.T) (string, func(*testing.T, *a2aclient.Client) string) {
				tree := fsys.Memory()
				return serveTree(t, tree), func(t *testing.T, _ *a2aclient.Client) string {
					got, err := readTree(t, tree, "hello.txt")
					if err != nil {
						return err.Error()
					}
					return got
				}
			},
		},
		{
			name: "memory",
			workspace: func(*testing.T) (string, func(*testing.T, *a2aclient.Client) string) {
				return serve.WorkspaceMemory, func(t *testing.T, c *a2aclient.Client) string {
					return testutil.AllArtifactText(sendWant(t, c, textIn("context-b", "read hello.txt"), a2a.TaskStateCompleted))
				}
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			workspace, seen := tc.workspace(t)
			sock := filepath.Join(testutil.ShortTempDir(t), "a2a.sock")
			args := []string{"--sandbox", "docker", "--image", image, "--command", "/usr/local/bin/fakeacp", "--egress", "allowlist",
				"--workspace", workspace, "--workspace-sync", "live", "--listen", "unix://" + sock}
			if !served {
				t.Logf("Docker here can't mount a live workspace, so the test checks the refusal: %s", caps.MountProblems[sandbox.MountServed])
				ready := failedReady(t, bin, args...)
				if !strings.Contains(ready.Error, "--workspace-sync live") || !strings.Contains(ready.Error, "use --workspace-sync turn") {
					t.Fatalf("ready line where Docker can't mount a live workspace = %+v, want an error with the hint to use turn sync", ready)
				}
				if errors.Is(profileErr, sandboxrt.ErrLiveAppArmorNotLoaded) && !strings.Contains(ready.Error, "a2a-serve-sidecar apparmor-profile | sudo apparmor_parser -r") {
					t.Fatalf("ready line where the AppArmor profile isn't loaded = %+v, want an error with the command that loads it", ready)
				}
				return
			}
			if ready := startBinary(t, bin, args...); ready.Workspace != tc.name || ready.WorkspaceSync != serve.SyncLive {
				t.Fatalf("ready line = %+v, want workspace %s with live sync", ready, tc.name)
			}
			c, err := a2aclient.NewFromEndpoints(context.Background(), []*a2a.AgentInterface{a2a.NewAgentInterface("http://a2a/", a2a.TransportProtocolJSONRPC)}, a2aclient.WithJSONRPCTransport(unixClient(sock)))
			if err != nil {
				t.Fatalf("a2aclient.NewFromEndpoints() error = %v", err)
			}

			waiting := sendWant(t, c, textIn("context-a", "writeask hello.txt"), a2a.TaskStateInputRequired)
			during := seen(t, c)
			done := sendWant(t, c, allow(t, waiting), a2a.TaskStateCompleted)

			if during != "written by fakeacp\n" {
				t.Fatalf("hello.txt as the host sees it while the turn waits = %q, want the agent's file", during)
			}
			if changes := changesOf(t, done); changes != nil {
				t.Fatalf("changes of a live turn = %+v, want no changes artifact", changes)
			}
		})
	}
}

// envLiveAppArmor names the AppArmor profile that a2a-serve-sidecar must
// find loaded and give the containers with a live workspace, so that the
// test fails where they can't mount it.
const envLiveAppArmor = "A2A_SERVE_TEST_LIVE_APPARMOR"

// failedReady runs the a2a-serve-sidecar binary bin with args, which must fail to
// start, and returns its ready line.
func failedReady(t *testing.T, bin string, args ...string) serve.Ready {
	t.Helper()
	var stdout, stderr bytes.Buffer
	cmd := exec.Command(bin, args...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err == nil {
		t.Fatalf("a2a-serve-sidecar %v exited with 0, want a failure; stdout:\n%s", args, stdout.String())
	}
	var ready serve.Ready
	if err := json.Unmarshal(stdout.Bytes(), &ready); err != nil || ready.Ready {
		t.Fatalf("ready line = %q, %v, want ready false; stderr:\n%s", stdout.String(), err, stderr.String())
	}
	return ready
}
