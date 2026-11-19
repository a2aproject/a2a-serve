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

package a2aserve_test

import (
	"context"
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/core/a2aserve"
	"github.com/a2aproject/a2a-serve/core/agent"
	"github.com/a2aproject/a2a-serve/core/sandbox"
	"github.com/a2aproject/a2a-serve/core/tools"
	"github.com/a2aproject/a2a-serve/core/workspace"
	"github.com/a2aproject/a2a-serve/internal/testutil"
)

// fakeACP is the fake ACP agent of the server's tests.
var fakeACP string

func TestMain(m *testing.M) {
	dir, err := testutil.MkdirShortTemp()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fakeACP = filepath.Join(dir, "fakeacp")
	build := exec.Command("go", "build", "-o", fakeACP, "../../internal/agentsrv/testdata/fakeacp")
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "building fakeacp:", err)
		os.Exit(1)
	}
	code := m.Run()
	if err := os.RemoveAll(dir); err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
	os.Exit(code)
}

// environment returns an Environment in its own directories.
func environment(t *testing.T, pool string) a2aserve.Environment {
	t.Helper()
	return a2aserve.Environment{Workspace: workspace.Dir(t.TempDir()), Home: filepath.Join(t.TempDir(), "home"), Pool: pool}
}

// newAgent starts an agent of fakeacp without a sandbox, with cfg.
func newAgent(t *testing.T, cfg a2aserve.Config) *a2aserve.Agent {
	t.Helper()
	cfg.Profile = agent.Command(fakeACP)
	cfg.Runtime = sandbox.Local()
	cfg.StateDir = testutil.ShortTempDir(t)
	if cfg.Main.Workspace == nil {
		cfg.Main = environment(t, "")
	}
	ctx, cancel := context.WithCancel(context.Background())
	a, err := a2aserve.New(ctx, cfg)
	if err != nil {
		cancel()
		t.Fatalf("a2aserve.New() error = %v", err)
	}
	t.Cleanup(func() {
		cancel()
		if err := a.Close(context.Background()); err != nil {
			t.Errorf("Agent.Close() error = %v", err)
		}
	})
	return a
}

// client returns an A2A client of the handler of a over JSON-RPC.
func client(t *testing.T, a *a2aserve.Agent) *a2aclient.Client {
	t.Helper()
	srv := httptest.NewServer(a2asrv.NewJSONRPCHandler(a.Handler()))
	t.Cleanup(srv.Close)
	c, err := a2aclient.NewFromEndpoints(context.Background(), []*a2a.AgentInterface{a2a.NewAgentInterface(srv.URL, a2a.TransportProtocolJSONRPC)}, a2aclient.WithJSONRPCTransport(srv.Client()))
	if err != nil {
		t.Fatalf("a2aclient.NewFromEndpoints() error = %v", err)
	}
	return c
}

// send sends text in the conversation contextID and returns the text of
// the task's result.
func send(t *testing.T, c *a2aclient.Client, contextID, text string) string {
	t.Helper()
	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(text))
	msg.ContextID = contextID
	res, err := c.SendMessage(context.Background(), &a2a.SendMessageRequest{Message: msg})
	if err != nil {
		t.Fatalf("Client.SendMessage(%q) error = %v", text, err)
	}
	task, ok := res.(*a2a.Task)
	if !ok {
		t.Fatalf("Client.SendMessage(%q) = %T, want a task", text, res)
	}
	if task.Status.State != a2a.TaskStateCompleted {
		t.Fatalf("Client.SendMessage(%q) state = %v, want %v", text, task.Status.State, a2a.TaskStateCompleted)
	}
	var out strings.Builder
	for _, art := range task.Artifacts {
		for _, p := range art.Parts {
			out.WriteString(p.Text())
		}
	}
	return out.String()
}

func TestAgentAnswersAndDecidesTools(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		tools tools.Decider
		text  string
		want  string
	}{
		{name: "reply", text: "env HOME", want: "env HOME="},
		{name: "allowed command", tools: tools.Rules{Allow: []string{"command:whoami"}}, text: "whoami", want: "Ran whoami (allow)."},
		{name: "denied command", tools: tools.Rules{Deny: []string{"execute"}}, text: "whoami", want: "whoami denied."},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a := newAgent(t, a2aserve.Config{Tools: tc.tools})

			got := send(t, client(t, a), "", tc.text)

			if !strings.HasPrefix(got, tc.want) {
				t.Fatalf("Client.SendMessage(%q) = %q, want it to start with %q", tc.text, got, tc.want)
			}
		})
	}
}

// keyedCallers runs the conversations whose IDs start with a key and a dash
// in the process of that key.
type keyedCallers struct {
	envs map[string]a2aserve.Environment
}

func (c keyedCallers) Key(_ context.Context, contextID string) (string, error) {
	key, _, _ := strings.Cut(contextID, "-")
	return key, nil
}

func (c keyedCallers) Environment(_ context.Context, key string) (a2aserve.Environment, error) {
	env, ok := c.envs[key]
	if !ok {
		return a2aserve.Environment{}, fmt.Errorf("unknown key %q", key)
	}
	return env, nil
}

func (c keyedCallers) Admit(context.Context, string, *a2asrv.ExecutorContext) (func(), error) {
	return func() {}, nil
}

// keyedAgent starts an agent whose callers a and b run in processes of the
// pool users.
func keyedAgent(t *testing.T) *a2aserve.Agent {
	t.Helper()
	callers := keyedCallers{envs: map[string]a2aserve.Environment{"a": environment(t, "users"), "b": environment(t, "users")}}
	return newAgent(t, a2aserve.Config{Callers: callers, Pools: map[string]a2aserve.Pool{"users": {MaxProcesses: 2}}})
}

// running returns the keys of the processes of a that run.
func running(a *a2aserve.Agent) []string {
	var out []string
	for _, p := range a.Processes() {
		out = append(out, p.Key)
	}
	slices.Sort(out)
	return out
}

func TestCallersRunInProcessesOfTheirKeys(t *testing.T) {
	t.Parallel()
	a := keyedAgent(t)
	c := client(t, a)

	pids := map[string]string{}
	for _, key := range []string{"a", "b"} {
		pids[key] = send(t, c, key+"-1", "pid")
		if again := send(t, c, key+"-2", "pid"); again != pids[key] {
			t.Fatalf("Client.SendMessage(pid) of %s = %q, want the process of its first conversation, %q", key, again, pids[key])
		}
	}

	if pids["a"] == pids["b"] {
		t.Fatalf("Client.SendMessage(pid) of a and b = %q, want two processes", pids["a"])
	}
	if diff := cmp.Diff([]string{"", "a", "b"}, running(a)); diff != "" {
		t.Fatalf("Agent.Processes() wrong result (-want +got) diff = %s", diff)
	}
}

func TestStopStopsTheProcessOfAKey(t *testing.T) {
	t.Parallel()
	a := keyedAgent(t)
	c := client(t, a)
	first := send(t, c, "a-1", "pid")
	send(t, c, "b-1", "pid")

	release, err := a.Stop(context.Background(), "a")
	if err != nil {
		t.Fatalf("Agent.Stop() error = %v", err)
	}
	got := running(a)
	release()

	if diff := cmp.Diff([]string{"", "b"}, got); diff != "" {
		t.Fatalf("Agent.Processes() after Agent.Stop() wrong result (-want +got) diff = %s", diff)
	}
	if again := send(t, c, "a-1", "pid"); again == first {
		t.Fatalf("Client.SendMessage(pid) after Agent.Stop() = %q, want a new process", again)
	}
}

func TestStopIdleStopsAnIdleProcess(t *testing.T) {
	t.Parallel()
	a := keyedAgent(t)
	send(t, client(t, a), "a-1", "pid")

	stopped, err := a.StopIdle("a")

	if err != nil || !stopped {
		t.Fatalf("Agent.StopIdle() = %v, %v, want true, nil", stopped, err)
	}
	if diff := cmp.Diff([]string{""}, running(a)); diff != "" {
		t.Fatalf("Agent.Processes() after Agent.StopIdle() wrong result (-want +got) diff = %s", diff)
	}
}

func TestMainGetsConfigMainWithoutAskingCallers(t *testing.T) {
	t.Parallel()
	a := keyedAgent(t)

	got := send(t, client(t, a), "-1", "pid")

	if got == "" {
		t.Fatalf("Client.SendMessage(pid) in the main process = %q, want its pid", got)
	}
}
