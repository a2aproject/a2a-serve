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
	"context"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/core/sandbox"
	"github.com/a2aproject/a2a-serve/internal/sidecar/hosttools"
	"github.com/a2aproject/a2a-serve/internal/sidecar/refhost"
	"github.com/a2aproject/a2a-serve/internal/sidecar/serve"
	"github.com/a2aproject/a2a-serve/internal/testutil"
)

func TestBwrapSandboxRunsTheAgentBehindTheEgressProxy(t *testing.T) {
	t.Parallel()
	bin := bwrapTestEnv(t)
	dir := testutil.ShortTempDir(t)
	host := refhost.New(refhost.Config{
		Decide: func(c refhost.Connect) refhost.Decision {
			if c.Target == "blocked.test:443" {
				return refhost.Refuse("blocked.test is off limits")
			}
			return refhost.Intercept()
		},
		Answer: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if _, err := io.WriteString(w, "host answer for "+r.Header.Get("X-Api-Key")); err != nil {
				t.Errorf("answering error = %v", err)
			}
		}),
	})
	workspace := t.TempDir()
	sock := filepath.Join(dir, "a2a.sock")
	ready := startBinary(t, bin, "--sandbox", "bwrap", "--command", fakeACP, "--workspace", "dir:"+workspace, "--workspace-sync", "live",
		"--listen", "unix://"+sock, "--egress-proxy", serveHost(t, host, dir))
	if ready.Sandbox != serve.SandboxBwrap || ready.Egress != serve.EgressProxy {
		t.Fatalf("ready line = %+v, want the bwrap sandbox behind the egress proxy", ready)
	}
	c, err := a2aclient.NewFromEndpoints(context.Background(), []*a2a.AgentInterface{a2a.NewAgentInterface("http://a2a/", a2a.TransportProtocolJSONRPC)}, a2aclient.WithJSONRPCTransport(unixClient(sock)))
	if err != nil {
		t.Fatalf("a2aclient.NewFromEndpoints() error = %v", err)
	}

	task := sendTask(t, c, "ctx-e2e", "fetch https://api.anthropic.com/v1/models")
	refused := sendTask(t, c, "ctx-e2e", "fetch https://blocked.test/")
	waiting := sendWant(t, c, textIn("ctx-e2e", "writeask hello.txt"), a2a.TaskStateInputRequired)
	sendWant(t, c, allow(t, waiting), a2a.TaskStateCompleted)

	if got, want := testutil.AllArtifactText(task), "fetch 200 host answer for "; got != want {
		t.Fatalf("fetch through the egress proxy = %q, want %q", got, want)
	}
	if got := testutil.AllArtifactText(refused); !strings.HasPrefix(got, "fetch error:") {
		t.Fatalf("fetch of a refused host = %q, want an error", got)
	}
	wantConnects := []refhost.Connect{
		{Context: "ctx-e2e", Target: "api.anthropic.com:443", Offered: true},
		{Context: "ctx-e2e", Target: "blocked.test:443", Offered: true},
	}
	if diff := cmp.Diff(wantConnects, slices.Compact(host.Connects())); diff != "" {
		t.Fatalf("host connects wrong result (-want +got) diff = %s", diff)
	}
	if got, err := os.ReadFile(filepath.Join(workspace, "hello.txt")); err != nil || string(got) != "written by fakeacp\n" {
		t.Fatalf("os.ReadFile(hello.txt) = %q, %v, want the agent's file", got, err)
	}
}

func TestBwrapSandboxTellsTheToolsEndpointTheContextOfEachCall(t *testing.T) {
	t.Parallel()
	bin := bwrapTestEnv(t)
	tools := refhost.NewTools(refhost.ToolsConfig{Hooks: hosttools.HookSupport{BeforeTool: true, AfterTool: true}})
	dir := testutil.ShortTempDir(t)
	url := serveHost(t, refhost.New(refhost.Config{Tools: tools}), dir)
	sock := filepath.Join(dir, "a2a.sock")
	startBinary(t, bin, "--sandbox", "bwrap", "--command", fakeACP, "--workspace", "memory", "--tools", url, "--listen", "unix://"+sock)
	c, err := a2aclient.NewFromEndpoints(context.Background(), []*a2a.AgentInterface{a2a.NewAgentInterface("http://a2a/", a2a.TransportProtocolJSONRPC)}, a2aclient.WithJSONRPCTransport(unixClient(sock)))
	if err != nil {
		t.Fatalf("a2aclient.NewFromEndpoints() error = %v", err)
	}

	var tasks []string
	for _, contextID := range []string{"context-a", "context-b"} {
		task := sendTask(t, c, contextID, "mcpcall host echo hi")
		if got, want := testutil.AllArtifactText(task), "mcp result: echo: hi"; got != want {
			t.Fatalf("Client.SendMessage(mcpcall) in %s = %q, want %q", contextID, got, want)
		}
		tasks = append(tasks, contextID+" "+string(task.ID))
	}

	var got []string
	for _, r := range tools.Records("tools/call") {
		got = append(got, r.Context+" "+r.Task)
	}
	if diff := cmp.Diff(tasks, got); diff != "" {
		t.Fatalf("context and task of the tool calls wrong result (-want +got) diff = %s", diff)
	}
}

func TestBwrapSandboxNamesAMissingAgent(t *testing.T) {
	t.Parallel()
	bin := bwrapTestEnv(t)
	sock := filepath.Join(testutil.ShortTempDir(t), "a2a.sock")
	cmd := exec.Command(bin, "--sandbox", "bwrap", "--command", "a2a-no-such-agent", "--workspace", "memory", "--listen", "unix://"+sock)
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "a2a-no-such-agent") {
		t.Fatalf("a2a-serve-sidecar with a missing agent = %v, output %q, want an error that names it", err, out)
	}
}

// bwrapTestEnv returns a2a-serve-sidecar built as releases are, or skips the test
// where its bubblewrap sandbox can't route through the boundary.
func bwrapTestEnv(t *testing.T) string {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("bubblewrap runs only on Linux")
	}
	if _, err := exec.LookPath("bwrap"); err != nil {
		t.Skip("needs bwrap")
	}
	bin := buildSidecar(t, "0")
	caps, err := sandbox.Bubblewrap(sandbox.BubblewrapOptions{Launcher: bin}).Probe(context.Background())
	if err != nil {
		t.Skipf("bubblewrap isn't available: %v", err)
	}
	if !caps.Boundary {
		t.Skipf("the boundary can't work here: %s", caps.Problem)
	}
	return bin
}

func TestBwrapSandboxServesALiveMemoryWorkspace(t *testing.T) {
	t.Parallel()
	bin := bwrapTestEnv(t)
	caps, err := sandbox.Bubblewrap(sandbox.BubblewrapOptions{Launcher: bin}).Probe(context.Background())
	if err != nil {
		t.Fatalf("Bubblewrap().Probe() error = %v", err)
	}
	sock := filepath.Join(testutil.ShortTempDir(t), "a2a.sock")
	args := []string{"--sandbox", "bwrap", "--command", fakeACP, "--egress", "allowlist", "--workspace", serve.WorkspaceMemory, "--workspace-sync", "live", "--listen", "unix://" + sock}
	if !slices.Contains(caps.Mounts, sandbox.MountServed) {
		ready := failedReady(t, bin, args...)
		if !strings.Contains(ready.Error, "use --workspace-sync turn") {
			t.Fatalf("ready line where bubblewrap can't mount a live workspace = %+v, want the hint to use turn sync", ready)
		}
		return
	}
	if ready := startBinary(t, bin, args...); ready.Workspace != serve.WorkspaceMemory || ready.WorkspaceSync != serve.SyncLive {
		t.Fatalf("ready line = %+v, want a memory workspace with live sync", ready)
	}
	c, err := a2aclient.NewFromEndpoints(context.Background(), []*a2a.AgentInterface{a2a.NewAgentInterface("http://a2a/", a2a.TransportProtocolJSONRPC)}, a2aclient.WithJSONRPCTransport(unixClient(sock)))
	if err != nil {
		t.Fatalf("a2aclient.NewFromEndpoints() error = %v", err)
	}

	waiting := sendWant(t, c, textIn("context-a", "writeask hello.txt"), a2a.TaskStateInputRequired)
	during := testutil.AllArtifactText(sendWant(t, c, textIn("context-b", "read hello.txt"), a2a.TaskStateCompleted))
	sendWant(t, c, allow(t, waiting), a2a.TaskStateCompleted)

	if !strings.Contains(during, "written by fakeacp") {
		t.Fatalf("hello.txt as another context sees it while the turn waits = %q, want the agent's file", during)
	}
}
