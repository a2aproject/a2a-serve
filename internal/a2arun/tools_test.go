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

package a2arun_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/a2arun"
	"github.com/a2aproject/a2a-serve/internal/a2arun/hosttools"
	"github.com/a2aproject/a2a-serve/internal/a2arun/refhost"
	"github.com/a2aproject/a2a-serve/internal/a2arun/serve"
	"github.com/a2aproject/a2a-serve/internal/testutil"
)

// startWithTools runs a2a-run locally with fakeacp and --tools of host,
// and returns its client.
func startWithTools(t *testing.T, host *refhost.Tools) *a2aclient.Client {
	t.Helper()
	srv := httptest.NewServer(host)
	t.Cleanup(srv.Close)
	sock := filepath.Join(testutil.ShortTempDir(t), "a2a.sock")
	start(t, "--sandbox", "local", "--command", fakeACP, "--workspace", "dir:"+t.TempDir(), "--tools", srv.URL+"/mcp", "--listen", "unix://"+sock)
	c, err := a2aclient.NewFromEndpoints(context.Background(), []*a2a.AgentInterface{a2a.NewAgentInterface("http://a2a/", a2a.TransportProtocolJSONRPC)}, a2aclient.WithJSONRPCTransport(unixClient(sock)))
	if err != nil {
		t.Fatalf("a2aclient.NewFromEndpoints() error = %v", err)
	}
	return c
}

// sendFor sends text in a new context and returns the task.
func sendFor(t *testing.T, c *a2aclient.Client, contextID, text string) *a2a.Task {
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
	return task
}

// params decodes the params of the records of method that host got.
func params[T any](t *testing.T, host *refhost.Tools, method string) []T {
	t.Helper()
	var out []T
	for _, r := range host.Records(method) {
		var p T
		if err := json.Unmarshal(r.Params, &p); err != nil {
			t.Fatalf("json.Unmarshal(%s) error = %v", r.Params, err)
		}
		out = append(out, p)
	}
	return out
}

// waitRecords waits until host got n messages of method.
func waitRecords(t *testing.T, host *refhost.Tools, method string, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for len(host.Records(method)) < n {
		if time.Now().After(deadline) {
			t.Fatalf("host got %d %s, want %d", len(host.Records(method)), method, n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestToolsGivesTheAgentTheHostAndItsHooks(t *testing.T) {
	t.Parallel()
	host := refhost.NewTools(refhost.ToolsConfig{Hooks: hosttools.HookSupport{BeforeTool: true, AfterTool: true}})
	c := startWithTools(t, host)
	task := sendFor(t, c, "context-a", "mcpcall host echo hi")
	if got := testutil.AllArtifactText(task); task.Status.State != a2a.TaskStateCompleted || got != "mcp result: echo: hi" {
		t.Fatalf("Client.SendMessage(mcpcall) = %v with %q, want completed with %q", task.Status.State, got, "mcp result: echo: hi")
	}
	mcp := "host.echo"
	wantBefore := []hosttools.BeforeToolParams{{Context: "context-a", Task: string(task.ID), Call: hosttools.BeforeCall{ID: "m1", Kind: "other", Title: "host_echo", MCP: &mcp}}}
	if diff := cmp.Diff(wantBefore, params[hosttools.BeforeToolParams](t, host, hosttools.MethodBeforeTool)); diff != "" {
		t.Fatalf("hooks/beforeTool wrong result (-want +got) diff = %s", diff)
	}
	waitRecords(t, host, hosttools.MethodAfterTool, 1)
	wantAfter := []hosttools.AfterToolParams{{Context: "context-a", Task: string(task.ID), Call: hosttools.AfterCall{ID: "m1", Kind: "other", Title: "host_echo", Status: "completed", Output: "echo: hi"}}}
	if diff := cmp.Diff(wantAfter, params[hosttools.AfterToolParams](t, host, hosttools.MethodAfterTool)); diff != "" {
		t.Fatalf("hooks/afterTool wrong result (-want +got) diff = %s", diff)
	}
}

func TestToolsBeforeToolDecidesShellCommands(t *testing.T) {
	t.Parallel()
	tests := []struct {
		decision  string
		wantState a2a.TaskState
		wantText  string
	}{
		{decision: hosttools.DecisionAllow, wantState: a2a.TaskStateCompleted, wantText: "Pushed (allow)."},
		{decision: hosttools.DecisionDeny, wantState: a2a.TaskStateCompleted, wantText: "Push denied."},
		{decision: hosttools.DecisionAsk, wantState: a2a.TaskStateInputRequired},
	}
	for _, tc := range tests {
		t.Run(tc.decision, func(t *testing.T) {
			t.Parallel()
			host := refhost.NewTools(refhost.ToolsConfig{
				Hooks: hosttools.HookSupport{BeforeTool: true},
				Decide: func(hosttools.BeforeToolParams) hosttools.BeforeToolResult {
					return hosttools.BeforeToolResult{Decision: tc.decision, Message: "push from the host"}
				},
			})
			task := sendFor(t, startWithTools(t, host), "context-a", "ask")
			if text := testutil.AllArtifactText(task); task.Status.State != tc.wantState || text != tc.wantText {
				t.Fatalf("Client.SendMessage(ask) = %v with %q, want %v with %q", task.Status.State, text, tc.wantState, tc.wantText)
			}
			calls := params[hosttools.BeforeToolParams](t, host, hosttools.MethodBeforeTool)
			if len(calls) != 1 || calls[0].Call.Command == nil || *calls[0].Call.Command != "git push origin main" {
				t.Fatalf("hooks/beforeTool calls = %+v, want one with the command git push origin main", calls)
			}
		})
	}
}

func TestToolsWithoutHooksAsksTheCaller(t *testing.T) {
	t.Parallel()
	host := refhost.NewTools(refhost.ToolsConfig{})
	task := sendFor(t, startWithTools(t, host), "context-a", "mcpcall host echo hi")
	if task.Status.State != a2a.TaskStateInputRequired {
		t.Fatalf("Client.SendMessage(mcpcall) state = %v, want %v", task.Status.State, a2a.TaskStateInputRequired)
	}
	if got := host.Records(hosttools.MethodBeforeTool); len(got) != 0 {
		t.Fatalf("host got %d hooks/beforeTool, want none", len(got))
	}
}

func TestToolsFailsToStartWithoutTheEndpoint(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(refhost.NewTools(refhost.ToolsConfig{}))
	url := srv.URL + "/mcp?token=secret-token"
	srv.Close()
	var stdout, stderr bytes.Buffer
	args := []string{"--sandbox", "local", "--command", fakeACP, "--workspace", "dir:" + t.TempDir(), "--tools", url}
	code := a2arun.Main(context.Background(), args, a2arun.IO{Stdout: &stdout, Stderr: &stderr})
	var ready serve.Ready
	if err := json.Unmarshal(stdout.Bytes(), &ready); err != nil || code != 1 || ready.Ready || !strings.Contains(ready.Error, "connecting to --tools") {
		t.Fatalf("a2arun.Main() = %d with stdout %q, want 1 and an error about --tools", code, stdout.String())
	}
	if strings.Contains(stdout.String()+stderr.String(), "secret-token") {
		t.Fatalf("a2arun.Main() output %q, want it without the query of --tools", stdout.String()+stderr.String())
	}
}
