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
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/a2aproject/a2a-serve/internal/a2atext"
	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/agentsrv/approvals"
	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/channel/channeltest"
)

// The conformance suite runs every scenario a harness profile must pass
// through the whole server: auth, the ACP executor, the task store and the
// harness process.

func TestConformanceTextTurn(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{})
	c := ts.client(ts.pair("me", auth.RoleOwner))
	wantResult(t, send(t, c, userText("hello", "", "")), a2a.TaskStateCompleted, "Hello from fakeacp.")
}

func TestConformanceToolProgress(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{})
	c := ts.client(ts.pair("me", auth.RoleOwner))
	var sawTool bool
	var final a2a.TaskState
	for ev, err := range c.SendStreamingMessage(context.Background(), &a2a.SendMessageRequest{Message: userText("tool", "", "")}) {
		if err != nil {
			t.Fatalf("SendStreamingMessage() error = %v", err)
		}
		if su, ok := ev.(*a2a.TaskStatusUpdateEvent); ok {
			final = su.Status.State
			if su.Status.Message != nil {
				for _, p := range su.Status.Message.Parts {
					if m, ok := p.Data().(map[string]any); ok && m["toolCall"] != nil {
						sawTool = true
					}
				}
			}
		}
	}
	if !sawTool {
		t.Fatalf("SendStreamingMessage() had no toolCall progress update")
	}
	if final != a2a.TaskStateCompleted {
		t.Fatalf("final state = %s, want completed", final)
	}
}

func TestConformanceApprovals(t *testing.T) {
	t.Parallel()
	typed := func(reply string) func(*testing.T, *a2a.Task, *approvals.Question) *a2a.Message {
		return func(_ *testing.T, task *a2a.Task, _ *approvals.Question) *a2a.Message {
			return userText(reply, string(task.ID), task.ContextID)
		}
	}
	tests := []struct {
		name   string
		answer func(*testing.T, *a2a.Task, *approvals.Question) *a2a.Message
		want   string
	}{
		{name: "approve", answer: typed("approve"), want: "Pushed (allow)."},
		{name: "always", answer: typed("always"), want: "Pushed (always)."},
		{name: "deny", answer: typed("deny"), want: "Push denied."},
		{
			name: "structured input",
			answer: func(t *testing.T, task *a2a.Task, q *approvals.Question) *a2a.Message {
				msg, err := approvals.AnswerMessage(task.ID, task.ContextID, q.ID, "once")
				if err != nil {
					t.Fatalf("approvals.AnswerMessage() error = %v", err)
				}
				return msg
			},
			want: "Pushed (allow).",
		},
	}
	ts := startServer(t, serverOpts{})
	c := ts.client(ts.pair("me", auth.RoleOwner))
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			task := send(t, c, userText("ask", "", ""))
			if task.Status.State != a2a.TaskStateInputRequired {
				t.Fatalf("task state = %s, want input-required", task.Status.State)
			}
			q := approvals.FromStatus(task.Status)
			if q == nil || q.Type != approvals.TypePermission || len(q.Options) != 3 {
				t.Fatalf("question = %+v, want a permission question with 3 options", q)
			}
			wantResult(t, send(t, c, tc.answer(t, task, q)), a2a.TaskStateCompleted, tc.want)
		})
	}
}

func TestConformanceCancelMidTool(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{})
	c := ts.client(ts.pair("me", auth.RoleOwner))
	task := sendAsync(t, c, userText("slow", "", ""))
	waitState(t, c, task.ID, a2a.TaskStateWorking)
	if _, err := c.CancelTask(context.Background(), &a2a.CancelTaskRequest{ID: task.ID}); err != nil {
		t.Fatalf("CancelTask() error = %v", err)
	}
	waitState(t, c, task.ID, a2a.TaskStateCanceled)
	wantResult(t, send(t, c, userText("hello", "", task.ContextID)), a2a.TaskStateCompleted, "Hello from fakeacp.")
}

func TestConformanceFollowUpInContext(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{})
	c := ts.client(ts.pair("me", auth.RoleOwner))
	first := send(t, c, userText("remember blue", "", ""))
	wantResult(t, send(t, c, userText("recall", "", first.ContextID)), a2a.TaskStateCompleted, "You said: blue")
	wantResult(t, send(t, c, userText("recall", "", "")), a2a.TaskStateCompleted, "You said: ")
}

func TestConformanceRestartAndResume(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{})
	token := ts.pair("me", auth.RoleOwner)
	first := send(t, ts.client(token), userText("remember green", "", ""))
	root := ts.dir.Root
	ts.stop()

	ts2 := startServer(t, serverOpts{dir: root})
	wantResult(t, send(t, ts2.client(token), userText("recall", "", first.ContextID)), a2a.TaskStateCompleted, "You said: green")
}

func TestSecondServerInSameDirFailsWithoutSideEffects(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{})
	c := ts.client(ts.pair("me", auth.RoleOwner))
	task := send(t, c, userText("ask", "", ""))
	if task.Status.State != a2a.TaskStateInputRequired {
		t.Fatalf("task state = %s, want input-required", task.Status.State)
	}

	cfg, err := ts.dir.LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	err = Run(context.Background(), Options{Dir: ts.dir, Config: cfg, Secrets: agentdir.Secrets{}, Timings: testTimings})
	if err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("Run() error = %v, want an already running error", err)
	}

	got := waitState(t, c, task.ID, a2a.TaskStateInputRequired)
	wantResult(t, send(t, c, userText("approve", string(got.ID), got.ContextID)), a2a.TaskStateCompleted, "Pushed (allow).")
}

func TestConformanceCrashRestartsAgent(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{})
	c := ts.client(ts.pair("me", auth.RoleOwner))
	task := send(t, c, userText("crash", "", ""))
	if task.Status.State != a2a.TaskStateFailed {
		t.Fatalf("task state = %s, want failed", task.Status.State)
	}
	wantResult(t, send(t, c, userText("hello", "", "")), a2a.TaskStateCompleted, "Hello from fakeacp.")
}

func TestConformanceSessionModel(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		model     string
		wantState a2a.TaskState
		wantText  string
	}{
		{name: "known model", model: "fake/other", wantState: a2a.TaskStateCompleted, wantText: "model: fake/other"},
		{name: "unknown model", model: "fake/missing", wantState: a2a.TaskStateFailed, wantText: "model not found: fake/missing"},
		{name: "no model", wantState: a2a.TaskStateCompleted, wantText: "model: fake/default"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ts := startServer(t, serverOpts{configure: func(cfg *agentdir.Config) {
				cfg.Harness, cfg.Model = "opencode", tc.model
			}})
			c := ts.client(ts.pair("me", auth.RoleOwner))
			task := send(t, c, userText("model", "", ""))
			if task.Status.State != tc.wantState {
				t.Fatalf("task state = %s (%s), want %s", task.Status.State, a2atext.Message(task.Status.Message), tc.wantState)
			}
			if got := a2atext.Result(task) + a2atext.Message(task.Status.Message); !strings.Contains(got, tc.wantText) {
				t.Fatalf("task result = %q, want it to contain %q", got, tc.wantText)
			}
		})
	}
}

func TestConformanceQueuedConcurrentTask(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{})
	c := ts.client(ts.pair("me", auth.RoleOwner))
	slow := sendAsync(t, c, userText("slow", "", ""))
	waitState(t, c, slow.ID, a2a.TaskStateWorking)
	queued := sendAsync(t, c, userText("hello", "", slow.ContextID))
	if got := waitState(t, c, queued.ID, a2a.TaskStateSubmitted); got.Status.State != a2a.TaskStateSubmitted {
		t.Fatalf("queued task state = %s, want submitted", got.Status.State)
	}
	if _, err := c.CancelTask(context.Background(), &a2a.CancelTaskRequest{ID: slow.ID}); err != nil {
		t.Fatalf("CancelTask() error = %v", err)
	}
	wantResult(t, waitState(t, c, queued.ID, a2a.TaskStateCompleted), a2a.TaskStateCompleted, "Hello from fakeacp.")
}

func TestUnauthenticatedCallerIsRejected(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{})
	resp, err := testHTTP.Post(ts.localURL()+"/", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("POST / status = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}
	card, err := testHTTP.Get(ts.localURL() + "/.well-known/agent-card.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := card.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if card.StatusCode != http.StatusOK {
		t.Fatalf("GET card status = %d, want %d", card.StatusCode, http.StatusOK)
	}
}

func TestCallerAnswersOwnApprovalInline(t *testing.T) {
	t.Parallel()
	ch := channeltest.NewChannel("telegram")
	ts := startServer(t, serverOpts{channel: ch})
	ctx := context.Background()
	ch.Write("telegram:1", "dm1", "/start "+ownerCode(t, ts))
	ch.WaitFor(t, "dm1", channeltest.HasText("Linked as owner"))

	member := ts.client(ts.pair("grok", auth.RoleAuthenticated))
	other := ts.client(ts.pair("bob", auth.RoleAuthenticated))
	task := send(t, member, userText("ask", "", ""))
	if task.Status.State != a2a.TaskStateInputRequired {
		t.Fatalf("task state = %s, want input-required", task.Status.State)
	}
	if _, err := other.SendMessage(ctx, &a2a.SendMessageRequest{Message: userText("approve", string(task.ID), task.ContextID)}); err == nil {
		t.Fatalf("SendMessage() answering another member's approval error = nil, want error")
	}
	answer := send(t, member, userText("approve", string(task.ID), task.ContextID))

	wantResult(t, answer, a2a.TaskStateCompleted, "Pushed (allow).")
	if got := ch.Questions(); got != 0 {
		t.Fatalf("questions sent to messengers = %d, want none: the caller answers inline", got)
	}
}

func TestConformanceAnswerFollowsQuestionInHistory(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{})
	c := ts.client(ts.pair("me", auth.RoleOwner))
	task := send(t, c, userText("ask", "", ""))
	answer := userText("approve", string(task.ID), task.ContextID)

	done := send(t, c, answer)

	var got []string
	for _, m := range done.History {
		got = append(got, string(m.Role)+": "+a2atext.Message(m))
	}
	question := slices.IndexFunc(done.History, func(m *a2a.Message) bool { return m.ID == task.Status.Message.ID })
	answered := slices.IndexFunc(done.History, func(m *a2a.Message) bool { return m.ID == answer.ID })
	if question < 0 || answered < 0 || question > answered {
		t.Fatalf("task history = %q, want the question before the answer", got)
	}
	if n := len(slices.Compact(slices.Clone(got))); n != len(got) {
		t.Fatalf("task history = %q, want no repeated messages", got)
	}
}
