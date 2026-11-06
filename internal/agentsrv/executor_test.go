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

package agentsrv

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"iter"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/integrations/executor/a2aacp"
	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/a2atext"
	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/agentkey"
	"github.com/a2aproject/a2a-serve/internal/agentsrv/approvals"
	"github.com/a2aproject/a2a-serve/internal/agentsrv/messenger"
	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/channel/channeltest"
	"github.com/a2aproject/a2a-serve/internal/harness"
	"github.com/a2aproject/a2a-serve/internal/store"
)

// turnExecutor cancels like a2aacp: it stops the turn, which ends the
// running Execute, and only then yields the canceled event.
type turnExecutor struct {
	mu       sync.Mutex
	canceled bool
}

func (e *turnExecutor) Execute(context.Context, *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(func(a2a.Event, error) bool) {}
}

func (e *turnExecutor) Cancel(ctx context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		if err := ctx.Err(); err != nil {
			yield(nil, err)
			return
		}
		e.mu.Lock()
		e.canceled = true
		e.mu.Unlock()
		yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateCanceled, nil), nil)
	}
}

func (e *turnExecutor) turnCanceled() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.canceled
}

func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "server.db")
	seedDB(t, path)
	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})
	return st
}

func TestCancelYieldsCanceledBeforeTurnEnds(t *testing.T) {
	t.Parallel()
	inner := &turnExecutor{}
	exec := &observedExecutor{inner: inner, bus: messenger.NewBus(), notes: newToolNotes(), approvals: approvals.New(approvals.Deps{Records: approvals.NewRecords(approvals.RecordsDeps{Store: openTestStore(t)})})}
	execCtx := &a2asrv.ExecutorContext{TaskID: "t1", ContextID: "c1"}

	var states []a2a.TaskState
	for ev, err := range exec.Cancel(context.Background(), execCtx) {
		if err != nil {
			t.Fatalf("Cancel() error = %v", err)
		}
		if len(states) == 0 && inner.turnCanceled() {
			t.Fatalf("Cancel() first event after the turn was canceled, want it before, while the execution's queue is open")
		}
		states = append(states, ev.(*a2a.TaskStatusUpdateEvent).Status.State)
	}
	if diff := cmp.Diff([]a2a.TaskState{a2a.TaskStateCanceled}, states); diff != "" {
		t.Fatalf("Cancel() wrong states (-want +got) diff = %s", diff)
	}
	if !inner.turnCanceled() {
		t.Fatalf("Cancel() didn't cancel the turn")
	}
}

func TestCancelStopsTurnAfterConsumerStops(t *testing.T) {
	t.Parallel()
	inner := &turnExecutor{}
	exec := &observedExecutor{inner: inner, bus: messenger.NewBus(), notes: newToolNotes(), approvals: approvals.New(approvals.Deps{Records: approvals.NewRecords(approvals.RecordsDeps{Store: openTestStore(t)})})}
	execCtx := &a2asrv.ExecutorContext{TaskID: "t1", ContextID: "c1"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// a2asrv stops consuming at the terminal event and cancels the context.
	var states []a2a.TaskState
	for ev, err := range exec.Cancel(ctx, execCtx) {
		if err != nil {
			t.Fatalf("Cancel() error = %v", err)
		}
		states = append(states, ev.(*a2a.TaskStatusUpdateEvent).Status.State)
		cancel()
		break
	}

	if diff := cmp.Diff([]a2a.TaskState{a2a.TaskStateCanceled}, states); diff != "" {
		t.Fatalf("Cancel() wrong states (-want +got) diff = %s", diff)
	}
	if !inner.turnCanceled() {
		t.Fatalf("Cancel() didn't cancel the turn")
	}
}

// statusExecutor yields one status update.
type statusExecutor struct {
	state a2a.TaskState
	msg   *a2a.Message
}

func (e statusExecutor) Execute(_ context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		yield(a2a.NewStatusUpdateEvent(execCtx, e.state, e.msg), nil)
	}
}

func (statusExecutor) Cancel(context.Context, *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(func(a2a.Event, error) bool) {}
}

func TestUnattendedQuestionIsAskedAfterExecutionEnds(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{channel: channeltest.NewChannel("telegram")})
	linkOwner(t, ts, "telegram:1", "dm1")
	ctx := auth.WithPrincipal(context.Background(), auth.Owner())
	asked := send(t, ts.client(ts.pair("me", auth.RoleOwner)), userText("ask", "", ""))
	contextID := a2a.NewContextID()
	ts.srv.unattended.Add(contextID)
	exec := &observedExecutor{
		inner:     statusExecutor{state: a2a.TaskStateInputRequired, msg: asked.Status.Message},
		bus:       messenger.NewBus(),
		claims:    ts.srv.claims,
		approvals: ts.srv.approvals,
		notes:     newToolNotes(),
	}
	execCtx := &a2asrv.ExecutorContext{TaskID: a2a.NewTaskID(), ContextID: contextID}

	// a2a-go processes an event after the executor yields it. An answer sent
	// before the execution is done with the event fails as concurrent.
	var result *a2a.Task
	for ev, err := range exec.Execute(ctx, execCtx) {
		if err != nil {
			t.Fatalf("Execute() error = %v", err)
		}
		time.Sleep(200 * time.Millisecond)
		if got := ts.ch.Questions(); got != 0 {
			t.Fatalf("questions sent while the execution handles the event = %d, want 0; messages:\n%s", got, ts.ch.Dump())
		}
		u := ev.(*a2a.TaskStatusUpdateEvent)
		result = &a2a.Task{ID: u.TaskID, ContextID: u.ContextID, Status: u.Status}
	}
	exec.Cleanup(ctx, execCtx, result, nil)
	ts.ch.WaitFor(t, "dm1", channeltest.HasQuestion)
}

func TestExecutionEndWakesFollowersOfItsContext(t *testing.T) {
	t.Parallel()
	bus := messenger.NewBus()
	exec := &observedExecutor{bus: bus, unattended: &messenger.ContextSet{}}
	changed, unsubscribe := bus.SubscribeContext("c1")
	defer unsubscribe()
	other, unsubscribeOther := bus.SubscribeContext("c2")
	defer unsubscribeOther()

	exec.Cleanup(context.Background(), &a2asrv.ExecutorContext{TaskID: "t1", ContextID: "c1"}, nil, errors.New("the agent crashed"))

	select {
	case <-changed:
	default:
		t.Fatal("followers of the context were not woken when its execution ended")
	}
	select {
	case <-other:
		t.Fatal("followers of another context were woken")
	default:
	}
}

func TestToolDeciderReadsCurrentConfig(t *testing.T) {
	t.Parallel()
	cfg := &agentdir.Config{}
	rules := &toolDecider{config: func() *agentdir.Config { return cfg }, notes: newToolNotes()}
	q := a2aacp.Question{Options: []string{a2aacp.OptionOnce, a2aacp.OptionReject}, Tool: &a2aacp.ToolCall{Kind: "read"}}

	if _, ok := rules.AutoAnswer(context.Background(), nil, q); ok {
		t.Fatalf("AutoAnswer() without rules ok = true, want false")
	}
	cfg = &agentdir.Config{Approvals: agentdir.ApprovalsConfig{Allow: []string{"read"}}}
	got, ok := rules.AutoAnswer(context.Background(), nil, q)
	if !ok || got.Option != a2aacp.OptionOnce {
		t.Fatalf("AutoAnswer() after allowing read = %+v, %v, want %q", got, ok, a2aacp.OptionOnce)
	}
}

func TestToolDeciderAnswers(t *testing.T) {
	t.Parallel()
	rules := agentdir.ApprovalsConfig{
		Allow: []string{"tool:grafana.*", "command:kubectl get *"},
		Deny:  []string{"execute", "other"},
	}
	decider := &toolDecider{
		config:  func() *agentdir.Config { return &agentdir.Config{Approvals: rules} },
		servers: func() []string { return []string{"grafana"} },
		notes:   newToolNotes(),
	}
	options := []string{a2aacp.OptionAlways, a2aacp.OptionOnce, a2aacp.OptionReject}
	tests := []struct {
		name    string
		options []string
		tool    a2aacp.ToolCall
		want    string
		wantOK  bool
	}{
		{name: "allowed MCP tool of claude", tool: a2aacp.ToolCall{Kind: "other", Title: "mcp__grafana__query"}, want: a2aacp.OptionOnce, wantOK: true},
		{name: "allowed MCP tool of opencode", tool: a2aacp.ToolCall{Kind: "other", Title: "grafana_query"}, want: a2aacp.OptionOnce, wantOK: true},
		{name: "denied MCP tool", tool: a2aacp.ToolCall{Kind: "other", Title: "mcp__gmail__send"}, want: a2aacp.OptionReject, wantOK: true},
		{name: "allowed command", tool: a2aacp.ToolCall{Kind: "execute", Title: "kubectl get pods", Input: map[string]any{"command": "kubectl get pods"}}, want: a2aacp.OptionOnce, wantOK: true},
		{name: "denied command", tool: a2aacp.ToolCall{Kind: "execute", Title: "kubectl get pods; rm -rf /", Input: map[string]any{"command": "kubectl get pods; rm -rf /"}}, want: a2aacp.OptionReject, wantOK: true},
		{name: "allowed without once", options: []string{a2aacp.OptionAlways, a2aacp.OptionReject}, tool: a2aacp.ToolCall{Kind: "other", Title: "grafana_query"}, want: a2aacp.OptionAlways, wantOK: true},
		{name: "denied without reject", options: []string{a2aacp.OptionOnce}, tool: a2aacp.ToolCall{Kind: "execute"}, want: a2aacp.OptionCancel, wantOK: true},
		{name: "no rule", tool: a2aacp.ToolCall{Kind: "edit"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			opts := tc.options
			if opts == nil {
				opts = options
			}
			got, ok := decider.AutoAnswer(context.Background(), nil, a2aacp.Question{Options: opts, Tool: &tc.tool})
			if ok != tc.wantOK || got.Option != tc.want {
				t.Fatalf("AutoAnswer() = %q, %v, want %q, %v", got.Option, ok, tc.want, tc.wantOK)
			}
		})
	}
}

// lockedBuffer is a log destination safe for concurrent writes.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// Not parallel: it replaces the default logger, which the server logs to.
func TestTaskLifecycleIsLogged(t *testing.T) {
	var logs lockedBuffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	ts := startServer(t, serverOpts{})
	c := ts.client(ts.pair("me", auth.RoleOwner))

	task := send(t, c, userText("ask", "", ""))
	send(t, c, userText("approve", string(task.ID), task.ContextID))

	var got []string
	for line := range strings.Lines(logs.String()) {
		if strings.Contains(line, "task="+string(task.ID)) && strings.Contains(line, `msg="task `) {
			msg := line[strings.Index(line, `msg="`)+5:]
			got = append(got, msg[:strings.Index(msg, `"`)])
		}
	}
	want := []string{"task received", "task input-required", "task received", "task completed"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("task log wrong result (-want +got) diff = %s", diff)
	}
}

// Not parallel: it changes PATH.
func TestStartReportsOutdatedA2AOnce(t *testing.T) {
	bin := t.TempDir()
	script := "#!/bin/sh\necho '{\"version\":\"v0.1.0\"}'\n"
	if err := os.WriteFile(filepath.Join(bin, "a2a"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	var out lockedBuffer
	startServer(t, serverOpts{out: &out, setupProblems: func(context.Context) []string {
		return []string{"a2a v0.1.0 at " + filepath.Join(bin, "a2a") + " is too old"}
	}})

	got := 0
	for line := range strings.Lines(out.String()) {
		if strings.Contains(line, "a2a v0.1.0") {
			got++
		}
	}
	if got != 1 {
		t.Fatalf("lines about the outdated a2a at start = %d, want 1; output:\n%s", got, out.String())
	}
}

func TestMessageJoinsOnlyItsSendersConversation(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{})
	anna := ts.client(ts.pair("anna", auth.RoleAuthenticated))
	ben := ts.client(ts.pair("ben", auth.RoleAuthenticated))
	owner := ts.client(ts.pair("me", auth.RoleOwner))
	contextID := send(t, anna, userText("hello", "", "")).ContextID
	tests := []struct {
		name    string
		client  *a2aclient.Client
		wantErr bool
	}{
		{name: "the conversation's user", client: anna},
		{name: "another user", client: ben, wantErr: true},
		{name: "an owner", client: owner},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.client.SendMessage(context.Background(), &a2a.SendMessageRequest{Message: userText("hello", "", contextID)})
			if (err != nil) != tc.wantErr {
				t.Fatalf("SendMessage() into anna's conversation error = %v, want error %v", err, tc.wantErr)
			}
		})
	}
}

func TestOwnerMessageKeepsConversationWithItsUser(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{})
	anna := ts.client(ts.pair("anna", auth.RoleAuthenticated))
	ben := ts.client(ts.pair("ben", auth.RoleAuthenticated))
	owner := ts.client(ts.pair("me", auth.RoleOwner))
	contextID := send(t, anna, userText("hello", "", "")).ContextID
	send(t, owner, userText("hello", "", contextID))

	if _, err := anna.SendMessage(context.Background(), &a2a.SendMessageRequest{Message: userText("hello", "", contextID)}); err != nil {
		t.Fatalf("SendMessage() by anna after the owner's message error = %v, want nil", err)
	}
	_, err := ben.SendMessage(context.Background(), &a2a.SendMessageRequest{Message: userText("hello", "", contextID)})
	if !errors.Is(err, a2a.ErrUnauthorized) {
		t.Fatalf("SendMessage() by ben error = %v, want %v", err, a2a.ErrUnauthorized)
	}
}

func TestUserApprovalTimesOut(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{timings: func(tm *Timings) { tm.UserQuestionTimeout = time.Second }})
	anna := ts.client(ts.pair("anna", auth.RoleAuthenticated))
	task := send(t, anna, userText("ask", "", ""))
	if task.Status.State != a2a.TaskStateInputRequired {
		t.Fatalf("task state = %s, want %s", task.Status.State, a2a.TaskStateInputRequired)
	}
	wantResult(t, waitState(t, anna, task.ID, a2a.TaskStateCompleted), a2a.TaskStateCompleted, "Push denied.")
}

func TestApprovalOfExitedAgentFailsTask(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{})
	c := ts.client(ts.pair("me", auth.RoleOwner))
	task := send(t, c, userText("ask", "", ""))
	if task.Status.State != a2a.TaskStateInputRequired {
		t.Fatalf("task state = %s, want %s", task.Status.State, a2a.TaskStateInputRequired)
	}

	if got := send(t, c, userText("crash", "", "")); got.Status.State != a2a.TaskStateFailed {
		t.Fatalf("crash task state = %s, want %s", got.Status.State, a2a.TaskStateFailed)
	}

	failed := waitState(t, c, task.ID, a2a.TaskStateFailed)
	if text := a2atext.Message(failed.Status.Message); !strings.Contains(text, "exited") {
		t.Fatalf("task status message = %q, want it to say the agent exited", text)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		qs, err := ts.srv.store.UnansweredQuestions(context.Background())
		if err != nil {
			t.Fatalf("UnansweredQuestions() error = %v", err)
		}
		if len(qs) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("UnansweredQuestions() = %d questions, want none", len(qs))
		}
		time.Sleep(50 * time.Millisecond)
	}
	_, err := c.SendMessage(context.Background(), &a2a.SendMessageRequest{Message: userText("approve", string(task.ID), task.ContextID)})
	if err == nil || !strings.Contains(err.Error(), "terminal state") {
		t.Fatalf("SendMessage() answering the failed task error = %v, want a terminal state error", err)
	}
}

func TestApprovalOfStoppedServerFailsTaskAsRestarted(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{})
	token := ts.pair("me", auth.RoleOwner)
	task := send(t, ts.client(token), userText("ask", "", ""))
	if task.Status.State != a2a.TaskStateInputRequired {
		t.Fatalf("task state = %s, want %s", task.Status.State, a2a.TaskStateInputRequired)
	}
	root := ts.dir.Root
	ts.stop()

	restarted := startServer(t, serverOpts{dir: root})
	got, err := restarted.client(token).GetTask(context.Background(), &a2a.GetTaskRequest{ID: task.ID})
	if err != nil {
		t.Fatalf("GetTask() error = %v", err)
	}
	if got.Status.State != a2a.TaskStateFailed || !strings.Contains(a2atext.Message(got.Status.Message), "restarted") {
		t.Fatalf("GetTask() = %s %q, want failed by the restart", got.Status.State, a2atext.Message(got.Status.Message))
	}
}

func TestCallerLimitCountsTasksWaitingForApproval(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{})
	anna := ts.client(ts.pair("anna", auth.RoleAuthenticated))
	owner := ts.client(ts.pair("me", auth.RoleOwner))
	for range maxCallerExecutions {
		if task := send(t, anna, userText("ask", "", "")); task.Status.State != a2a.TaskStateInputRequired {
			t.Fatalf("task state = %s, want %s", task.Status.State, a2a.TaskStateInputRequired)
		}
	}

	_, err := anna.SendMessage(context.Background(), &a2a.SendMessageRequest{Message: userText("hello", "", "")})
	if !errors.Is(err, a2a.ErrUnauthorized) {
		t.Fatalf("SendMessage() over the limit error = %v, want %v", err, a2a.ErrUnauthorized)
	}
	send(t, owner, userText("hello", "", ""))
}

func TestMessageToConversationWaitingForApprovalIsRefused(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{})
	c := ts.client(ts.pair("me", auth.RoleOwner))
	asked := send(t, c, userText("ask", "", ""))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := c.SendMessage(ctx, &a2a.SendMessageRequest{Message: userText("hello", "", asked.ContextID)})
	if err == nil || !strings.Contains(err.Error(), string(asked.ID)) || !strings.Contains(err.Error(), "git push") {
		t.Fatalf("SendMessage() into a conversation waiting for approval error = %v, want a refusal that names task %s and its approval", err, asked.ID)
	}

	wantResult(t, send(t, c, userText("approve", string(asked.ID), asked.ContextID)), a2a.TaskStateCompleted, "Pushed (allow).")
	wantResult(t, send(t, c, userText("hello", "", asked.ContextID)), a2a.TaskStateCompleted, "Hello from fakeacp.")
}

func TestObservedExecutorCleanupForgetsUnattendedContextOfEndedTask(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		result  a2a.SendMessageResult
		err     error
		wantHas bool
	}{
		{name: "completed task", result: &a2a.Task{Status: a2a.TaskStatus{State: a2a.TaskStateCompleted}}},
		{name: "failed task", result: &a2a.Task{Status: a2a.TaskStatus{State: a2a.TaskStateFailed}}},
		{name: "execution error", err: context.Canceled},
		{name: "task waiting for input", result: &a2a.Task{Status: a2a.TaskStatus{State: a2a.TaskStateInputRequired}}, wantHas: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			unattended := &messenger.ContextSet{}
			unattended.Add("c1")
			unattended.Add("c2")
			e := &observedExecutor{bus: messenger.NewBus(), unattended: unattended, notes: newToolNotes()}

			e.Cleanup(context.Background(), &a2asrv.ExecutorContext{ContextID: "c1", TaskID: "t1"}, tc.result, tc.err)

			if got := unattended.Has("c1"); got != tc.wantHas {
				t.Fatalf("unattended.Has(c1) = %v, want %v", got, tc.wantHas)
			}
			if !unattended.Has("c2") {
				t.Fatalf("unattended.Has(c2) = false, want true for another context")
			}
		})
	}
}

func TestPrepareHarnessRefusesReplacedAgentDirectoryBeforeWritingHome(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		key  string
	}{
		{name: "owner", key: ""},
		{name: "anonymous", key: agentkey.Anonymous},
		{name: "user", key: agentkey.User("pair:anna")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir, err := agentdir.Open(filepath.Join(t.TempDir(), "proj"))
			if err != nil {
				t.Fatal(err)
			}
			if err := dir.Init(); err != nil {
				t.Fatal(err)
			}
			pins, err := harness.NewPins(dir.Root, dir.Path(), dir.StateDir(), dir.HomeDir())
			if err != nil {
				t.Fatalf("harness.NewPins() error = %v", err)
			}
			cfg := &agentdir.Config{Name: "coder", Harness: "fake", Command: []string{"fake-acp"}, Workspace: "..", Sandbox: harness.SandboxDocker}
			s := &Server{dir: dir, pins: pins, rt: agentdir.NewRuntime(dir, cfg, agentdir.Secrets{})}
			if err := os.Rename(dir.Root, dir.Root+".old"); err != nil {
				t.Fatal(err)
			}
			if err := dir.Init(); err != nil {
				t.Fatal(err)
			}

			_, err = s.prepareHarness(context.Background(), io.Discard, tc.key)

			if err == nil || !strings.Contains(err.Error(), "was replaced") {
				t.Fatalf("Server.prepareHarness() error = %v, want the agent directory replaced", err)
			}
			entries, err := os.ReadDir(dir.StateDir())
			if err != nil {
				t.Fatal(err)
			}
			var names []string
			for _, e := range entries {
				names = append(names, e.Name())
			}
			if diff := cmp.Diff([]string{"home"}, names); diff != "" {
				t.Fatalf("Server.prepareHarness() wrote into the state directory (-want +got) diff = %s", diff)
			}
			home, err := os.ReadDir(dir.HomeDir())
			if err != nil || len(home) > 0 {
				t.Fatalf("Server.prepareHarness() wrote %v into the home, error = %v", home, err)
			}
		})
	}
}

func TestRejectedToolCallIsNotedInTheResult(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		approvals agentdir.ApprovalsConfig
		reply     string
		want      string
	}{
		{name: "rejected by the caller", reply: "deny", want: "Tool call rejected: git push."},
		{name: "denied by deny-tools", approvals: agentdir.ApprovalsConfig{Deny: []string{"execute"}}, want: "Tool call denied by --deny-tools: git push."},
		{name: "approved", reply: "approve"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ts := startServer(t, serverOpts{configure: func(c *agentdir.Config) { c.Approvals = tc.approvals }})
			c := ts.client(ts.pair("me", auth.RoleOwner))
			task := send(t, c, userText("ask", "", ""))
			if tc.reply != "" {
				task = send(t, c, userText(tc.reply, string(task.ID), task.ContextID))
			}

			if task.Status.State != a2a.TaskStateCompleted {
				t.Fatalf("task state = %s, want %s", task.Status.State, a2a.TaskStateCompleted)
			}
			if got := a2atext.Message(task.Status.Message); got != tc.want {
				t.Fatalf("task status message = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestAlwaysAllowsTheRuleInTheConversation(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{})
	c := ts.client(ts.pair("me", auth.RoleOwner))
	asked := send(t, c, userText("ask", "", ""))
	q := approvals.FromStatus(asked.Status)
	want := []approvals.Option{{ID: "once", Label: "Allow once"}, {ID: "always", Label: "Always allow git push *"}, {ID: "reject", Label: "Deny"}}
	if q == nil || !cmp.Equal(want, q.Options) {
		t.Fatalf("question = %+v, want the options %v", q, want)
	}

	wantResult(t, send(t, c, userText("always", string(asked.ID), asked.ContextID)), a2a.TaskStateCompleted, "Pushed (allow).")

	wantResult(t, send(t, c, userText("ask", "", asked.ContextID)), a2a.TaskStateCompleted, "Pushed (allow).")
	if other := send(t, c, userText("ask", "", "")); other.Status.State != a2a.TaskStateInputRequired {
		t.Fatalf("task in another conversation state = %s, want %s", other.Status.State, a2a.TaskStateInputRequired)
	}
}

// Not parallel: it replaces the default logger, which the server logs to.
func TestApprovalAnswersAreLogged(t *testing.T) {
	var logs lockedBuffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	ts := startServer(t, serverOpts{})
	c := ts.client(ts.pair("me", auth.RoleOwner))

	answered := send(t, c, userText("ask", "", ""))
	send(t, c, userText("always", string(answered.ID), answered.ContextID))
	granted := send(t, c, userText("ask", "", answered.ContextID))
	if err := ts.srv.rt.Update(func(c *agentdir.Config) error { c.Approvals.Deny = []string{"execute"}; return nil }); err != nil {
		t.Fatalf("Runtime.Update() error = %v", err)
	}
	denied := send(t, c, userText("ask", "", ""))

	for _, want := range []string{
		fmt.Sprintf(`msg="approval answered" task=%s by=pair:me kind=execute option=always`, answered.ID),
		fmt.Sprintf(`msg="tool calls allowed for the conversation" task=%s context=%s rule="command:git push *" user=pair:me by=pair:me`, answered.ID, answered.ContextID),
		fmt.Sprintf(`msg="approval answered by a grant of the conversation" task=%s rule="command:git push *" kind=execute user=pair:me option=once`, granted.ID),
		fmt.Sprintf(`msg="approval answered by a rule" task=%s rule=execute kind=execute option=reject`, denied.ID),
	} {
		if !strings.Contains(logs.String(), want) {
			t.Fatalf("logs have no line with %q; logs:\n%s", want, logs.String())
		}
	}
}
