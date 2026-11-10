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
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/a2aproject/a2a-go/v2/a2aclient/agentcard"
	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/a2atext"
	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/agentsrv/approvals"
	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/messenger/messengertest"
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
		{name: "always", answer: typed("always"), want: "Pushed (allow)."},
		{name: "deny", answer: typed("deny"), want: "Push denied."},
		{
			name: "structured input",
			answer: func(t *testing.T, task *a2a.Task, q *approvals.Question) *a2a.Message {
				msg, err := approvals.AnswerMessage(task.ID, task.ContextID, q, "once")
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

func TestUnknownApprovalReplyKeepsQuestionPending(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{})
	c := ts.client(ts.pair("me", auth.RoleOwner))
	task := send(t, c, userText("ask", "", ""))
	asked := approvals.FromStatus(task.Status)
	if asked == nil {
		t.Fatalf("task status = %+v, want a question", task.Status)
	}

	for range 2 {
		got := send(t, c, userText("aprove", string(task.ID), task.ContextID))
		q := approvals.FromStatus(got.Status)
		if q == nil || q.ID != asked.ID {
			t.Fatalf("send(aprove) status = %+v, want question %s asked again", got.Status, asked.ID)
		}
		if text := a2atext.Message(got.Status.Message); strings.Count(text, `"aprove"`) != 1 || !strings.Contains(text, "once, always, reject") {
			t.Fatalf("send(aprove) status text = %q, want one refusal of \"aprove\" that lists the options", text)
		}
	}
	wantResult(t, send(t, c, userText("approve", string(task.ID), task.ContextID)), a2a.TaskStateCompleted, "Pushed (allow).")
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
	want := "the agent stopped unexpectedly; send the message again"
	if got := a2atext.Message(task.Status.Message); task.Status.State != a2a.TaskStateFailed || got != want {
		t.Fatalf("task = %s %q, want %s %q", task.Status.State, got, a2a.TaskStateFailed, want)
	}
	wantResult(t, send(t, c, userText("hello", "", "")), a2a.TaskStateCompleted, "Hello from fakeacp.")
}

func TestConformanceSessionModel(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		model     string
		secret    string
		wantState a2a.TaskState
		wantText  string
		notText   string
	}{
		{name: "known model", model: "fake/other", wantState: a2a.TaskStateCompleted, wantText: "model: fake/other"},
		{name: "unknown model", model: "fake/missing", wantState: a2a.TaskStateFailed, wantText: "the agent failed: setting \"model\": model not found: fake/missing", notText: "code -32603"},
		{name: "no model", wantState: a2a.TaskStateCompleted, wantText: "model: fake/default"},
		{
			name: "model of a provider without a key", model: "google/gemini-3-flash-preview", wantState: a2a.TaskStateFailed,
			wantText: "no key for provider google: set one with `a2a-serve config set --secret GOOGLE_GENERATIVE_AI_API_KEY`",
		},
		{
			name: "unknown model of a provider with a key", model: "google/gemini-missing", secret: "GEMINI_API_KEY", wantState: a2a.TaskStateFailed,
			wantText: "model not found: google/gemini-missing; choose another model with `a2a-serve config set --model <provider/model> --restart`", notText: "no key",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			secrets := agentdir.Secrets{}
			if tc.secret != "" {
				secrets[tc.secret] = "key"
			}
			ts := startServer(t, serverOpts{secrets: secrets, configure: func(cfg *agentdir.Config) {
				cfg.Harness, cfg.Model = "opencode", tc.model
				if tc.secret != "" {
					cfg.Secrets = []string{tc.secret}
				}
			}})
			c := ts.client(ts.pair("me", auth.RoleOwner))
			task := send(t, c, userText("model", "", ""))
			if task.Status.State != tc.wantState {
				t.Fatalf("task state = %s (%s), want %s", task.Status.State, a2atext.Message(task.Status.Message), tc.wantState)
			}
			got := a2atext.Result(task) + a2atext.Message(task.Status.Message)
			if !strings.Contains(got, tc.wantText) {
				t.Fatalf("task result = %q, want it to contain %q", got, tc.wantText)
			}
			if tc.notText != "" && strings.Contains(got, tc.notText) {
				t.Fatalf("task result = %q, want it without %q", got, tc.notText)
			}
		})
	}
}

func TestConformanceAgentErrorWithoutProtocolDetails(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{configure: func(cfg *agentdir.Config) { cfg.Harness = "opencode" }})
	c := ts.client(ts.pair("me", auth.RoleOwner))

	task := send(t, c, userText("apierror", "", ""))

	want := "the agent failed: models/gemini-2.5-flash is not found for API version v1beta; choose another model with `a2a-serve config set --model <provider/model> --restart`"
	if got := a2atext.Message(task.Status.Message); task.Status.State != a2a.TaskStateFailed || got != want {
		t.Fatalf("task = %s %q, want %s %q", task.Status.State, got, a2a.TaskStateFailed, want)
	}
}

func TestConformanceStalledTurn(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		prompt      string
		toolTimeout time.Duration
		wantState   a2a.TaskState
		wantText    string
	}{
		{name: "agent sends nothing", prompt: "stall", wantState: a2a.TaskStateFailed, wantText: "the agent sent nothing for 3s"},
		{name: "silent tool call", prompt: "build 6s", wantState: a2a.TaskStateCompleted, wantText: "Built."},
		{name: "silent pending tool call", prompt: "pending 6s", wantState: a2a.TaskStateCompleted, wantText: "Built."},
		{name: "tool call that never ends", prompt: "pending 1h", toolTimeout: 6 * time.Second, wantState: a2a.TaskStateFailed, wantText: "a tool call of the agent sent nothing for 6s"},
		{name: "streaming agent", prompt: "slow 6s", wantState: a2a.TaskStateCompleted, wantText: "Done."},
	}
	// A 3s stall timeout leaves room for a busy machine to delay the agent's
	// updates; the turns last twice as long, so they stall without them.
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ts := startServer(t, serverOpts{timings: func(tm *Timings) {
				tm.TurnStallTimeout = 3 * time.Second
				tm.ToolStallTimeout = tc.toolTimeout
			}})
			c := ts.client(ts.pair("me", auth.RoleOwner))
			// A first turn starts the agent process, which the guard would count as silence.
			warm := send(t, c, userText("hello", "", ""))
			wantResult(t, warm, a2a.TaskStateCompleted, "Hello from fakeacp.")
			task := send(t, c, userText(tc.prompt, "", warm.ContextID))
			if task.Status.State != tc.wantState {
				t.Fatalf("task state = %s (%s), want %s", task.Status.State, a2atext.Message(task.Status.Message), tc.wantState)
			}
			if got := a2atext.Result(task) + a2atext.Message(task.Status.Message); !strings.Contains(got, tc.wantText) {
				t.Fatalf("task result = %q, want it to contain %q", got, tc.wantText)
			}
			wantResult(t, send(t, c, userText("hello", "", task.ContextID)), a2a.TaskStateCompleted, "Hello from fakeacp.")
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
	ch := messengertest.NewMessenger("telegram")
	ts := startServer(t, serverOpts{messenger: ch})
	ctx := context.Background()
	ch.Write("telegram:1", "dm1", "/start "+ownerCode(t, ts))
	ch.WaitFor(t, "dm1", messengertest.HasText("Linked as owner"))

	member := ts.client(ts.pair("grok", auth.RoleMember))
	other := ts.client(ts.pair("bob", auth.RoleMember))
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

func TestConformanceCallerErrorsWithoutWrapping(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{})
	c := ts.client(ts.pair("me", auth.RoleOwner))
	done := send(t, c, userText("hello", "", ""))
	ctx := context.Background()
	tests := []struct {
		name string
		call func() error
		want string
	}{
		{
			name: "message to a completed task",
			call: func() error {
				_, err := c.SendMessage(ctx, &a2a.SendMessageRequest{Message: userText("again", string(done.ID), done.ContextID)})
				return err
			},
			want: `task in a terminal state "TASK_STATE_COMPLETED": this operation is not supported`,
		},
		{
			name: "cancel of an unknown task",
			call: func() error {
				_, err := c.CancelTask(ctx, &a2a.CancelTaskRequest{ID: "nope"})
				return err
			},
			want: "task not found",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.call()
			var a2aErr *a2a.Error
			if !errors.As(err, &a2aErr) || a2aErr.Message != tc.want {
				t.Fatalf("call error = %v, want %q", err, tc.want)
			}
		})
	}
}

var conformanceTransports = []a2a.TransportProtocol{a2a.TransportProtocolJSONRPC, a2a.TransportProtocolHTTPJSON}

func messageIDs(msgs []*a2a.Message) []string {
	ids := []string{}
	for _, m := range msgs {
		ids = append(ids, m.ID)
	}
	return ids
}

func TestConformanceHistoryLength(t *testing.T) {
	t.Parallel()
	const answered = 4
	// call returns the history the method answers with, and the whole history
	// of the task at that point.
	tests := []struct {
		name     string
		wantFull int
		// noneUnset is set for the methods that return no history unless
		// the request asks for it.
		noneUnset bool
		call      func(t *testing.T, c *a2aclient.Client, length *int) (got, full []string)
	}{
		{
			name:      "SendMessage",
			wantFull:  answered,
			noneUnset: true,
			call: func(t *testing.T, c *a2aclient.Client, length *int) ([]string, []string) {
				task := send(t, c, userText("ask", "", ""))
				res, err := c.SendMessage(context.Background(), &a2a.SendMessageRequest{
					Message: userText("approve", string(task.ID), task.ContextID),
					Config:  &a2a.SendMessageConfig{HistoryLength: length},
				})
				if err != nil {
					t.Fatalf("SendMessage() error = %v%s", err, taskStatus(c, task.ID))
				}
				return messageIDs(res.(*a2a.Task).History), storedHistory(t, c, task.ID)
			},
		},
		{
			name:      "SendMessage returning immediately",
			wantFull:  1,
			noneUnset: true,
			call: func(t *testing.T, c *a2aclient.Client, length *int) ([]string, []string) {
				res, err := c.SendMessage(context.Background(), &a2a.SendMessageRequest{
					Message: userText("ask", "", ""),
					Config:  &a2a.SendMessageConfig{HistoryLength: length, ReturnImmediately: true},
				})
				if err != nil {
					t.Fatalf("SendMessage() error = %v", err)
				}
				task := res.(*a2a.Task)
				return messageIDs(task.History), storedHistory(t, c, task.ID)[:1]
			},
		},
		{
			name:     "SendStreamingMessage",
			wantFull: 1,
			call: func(t *testing.T, c *a2aclient.Client, length *int) ([]string, []string) {
				msg := userText("ask", "", "")
				var got []string
				for ev, err := range c.SendStreamingMessage(context.Background(), &a2a.SendMessageRequest{
					Message: msg,
					Config:  &a2a.SendMessageConfig{HistoryLength: length},
				}) {
					if err != nil {
						t.Fatalf("SendStreamingMessage() error = %v", err)
					}
					if task, ok := ev.(*a2a.Task); ok {
						got = messageIDs(task.History)
					}
				}
				return got, []string{msg.ID}
			},
		},
		{
			name:     "GetTask",
			wantFull: answered,
			call: func(t *testing.T, c *a2aclient.Client, length *int) ([]string, []string) {
				asked := send(t, c, userText("ask", "", ""))
				done := send(t, c, userText("approve", string(asked.ID), asked.ContextID))
				task, err := c.GetTask(context.Background(), &a2a.GetTaskRequest{ID: done.ID, HistoryLength: length})
				if err != nil {
					t.Fatalf("GetTask() error = %v", err)
				}
				return messageIDs(task.History), storedHistory(t, c, done.ID)
			},
		},
	}
	lengths := []struct {
		name   string
		length *int
		keep   func(full int) int
	}{
		{name: "unset", keep: func(full int) int { return full }},
		{name: "zero", length: new(0), keep: func(int) int { return 0 }},
		{name: "one", length: new(1), keep: func(int) int { return 1 }},
		{name: "two", length: new(2), keep: func(full int) int { return min(2, full) }},
		{name: "more than there are", length: new(100), keep: func(full int) int { return full }},
	}
	ts := startServer(t, serverOpts{})
	token := ts.pair("me", auth.RoleOwner)
	for _, transport := range conformanceTransports {
		c := ts.clientOver(token, transport)
		for _, tc := range tests {
			for _, l := range lengths {
				t.Run(string(transport)+"/"+tc.name+"/"+l.name, func(t *testing.T) {
					t.Parallel()
					got, full := tc.call(t, c, l.length)
					if len(full) != tc.wantFull {
						t.Fatalf("%s whole history = %d messages, want %d", tc.name, len(full), tc.wantFull)
					}
					keep := l.keep(len(full))
					if tc.noneUnset && l.length == nil {
						keep = 0
					}
					want := full[len(full)-keep:]
					if diff := cmp.Diff(want, got); diff != "" {
						t.Fatalf("%s history wrong result (-want +got) diff = %s", tc.name, diff)
					}
				})
			}
		}
	}
}

func storedHistory(t *testing.T, c *a2aclient.Client, id a2a.TaskID) []string {
	t.Helper()
	task, err := c.GetTask(context.Background(), &a2a.GetTaskRequest{ID: id})
	if err != nil {
		t.Fatalf("GetTask() error = %v", err)
	}
	return messageIDs(task.History)
}

func TestConformanceReturnImmediately(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		prompt    string
		immediate bool
		want      func(a2a.TaskState) bool
		wantDesc  string
	}{
		{name: "waits for the turn by default", prompt: "hello", want: a2a.TaskState.Terminal, wantDesc: "terminal"},
		{name: "waits for input by default", prompt: "ask", want: func(s a2a.TaskState) bool { return s == a2a.TaskStateInputRequired }, wantDesc: "input-required"},
		{name: "returns before the turn ends", prompt: "slow", immediate: true, want: func(s a2a.TaskState) bool { return !s.Terminal() }, wantDesc: "not terminal"},
	}
	ts := startServer(t, serverOpts{})
	token := ts.pair("me", auth.RoleOwner)
	for _, transport := range conformanceTransports {
		c := ts.clientOver(token, transport)
		for _, tc := range tests {
			t.Run(string(transport)+"/"+tc.name, func(t *testing.T) {
				t.Parallel()
				res, err := c.SendMessage(context.Background(), &a2a.SendMessageRequest{
					Message: userText(tc.prompt, "", ""),
					Config:  &a2a.SendMessageConfig{ReturnImmediately: tc.immediate},
				})
				if err != nil {
					t.Fatalf("SendMessage() error = %v", err)
				}
				task := res.(*a2a.Task)
				if !tc.want(task.Status.State) {
					t.Fatalf("SendMessage() task state = %s, want %s", task.Status.State, tc.wantDesc)
				}
				if tc.prompt == "slow" {
					if _, err := c.CancelTask(context.Background(), &a2a.CancelTaskRequest{ID: task.ID}); err != nil {
						t.Fatalf("CancelTask() error = %v", err)
					}
				}
			})
		}
	}
}

func TestConformanceSubscribeToTask(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{})
	token := ts.pair("me", auth.RoleOwner)
	for _, transport := range conformanceTransports {
		c := ts.clientOver(token, transport)
		t.Run(string(transport)+"/first event is the task", func(t *testing.T) {
			t.Parallel()
			task := sendAsync(t, c, userText("slow", "", ""))
			waitState(t, c, task.ID, a2a.TaskStateWorking)
			t.Cleanup(func() {
				if _, err := c.CancelTask(context.Background(), &a2a.CancelTaskRequest{ID: task.ID}); err != nil {
					t.Errorf("CancelTask() error = %v", err)
				}
			})

			for ev, err := range c.SubscribeToTask(context.Background(), &a2a.SubscribeToTaskRequest{ID: task.ID}) {
				if err != nil {
					t.Fatalf("SubscribeToTask() error = %v", err)
				}
				got, ok := ev.(*a2a.Task)
				if !ok || got.ID != task.ID {
					t.Fatalf("SubscribeToTask() first event = %#v, want task %s", ev, task.ID)
				}
				return
			}
			t.Fatalf("SubscribeToTask() ended without an event")
		})
		t.Run(string(transport)+"/finished task", func(t *testing.T) {
			t.Parallel()
			done := send(t, c, userText("hello", "", ""))

			for _, err := range c.SubscribeToTask(context.Background(), &a2a.SubscribeToTaskRequest{ID: done.ID}) {
				if !errors.Is(err, a2a.ErrUnsupportedOperation) {
					t.Fatalf("SubscribeToTask() error = %v, want %v", err, a2a.ErrUnsupportedOperation)
				}
				return
			}
			t.Fatalf("SubscribeToTask() ended without an error, want %v", a2a.ErrUnsupportedOperation)
		})
	}
}

func TestConformancePushNotificationsAreRefused(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{})
	token := ts.pair("me", auth.RoleOwner)
	card, err := agentcard.NewResolver(testHTTP).Resolve(context.Background(), ts.localURL())
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if card.Capabilities.PushNotifications {
		t.Fatalf("agent card pushNotifications = true, want false")
	}
	hook := func() *a2a.PushConfig { return &a2a.PushConfig{URL: "https://example.com/hook"} }
	ctx := context.Background()
	tests := []struct {
		name string
		call func(c *a2aclient.Client, task a2a.TaskID) error
	}{
		{
			name: "SendMessage with a push config",
			call: func(c *a2aclient.Client, _ a2a.TaskID) error {
				_, err := c.SendMessage(ctx, &a2a.SendMessageRequest{Message: userText("hello", "", ""), Config: &a2a.SendMessageConfig{PushConfig: hook()}})
				return err
			},
		},
		{
			name: "SendStreamingMessage with a push config",
			call: func(c *a2aclient.Client, _ a2a.TaskID) error {
				for _, err := range c.SendStreamingMessage(ctx, &a2a.SendMessageRequest{Message: userText("hello", "", ""), Config: &a2a.SendMessageConfig{PushConfig: hook()}}) {
					return err
				}
				return nil
			},
		},
		{
			name: "CreateTaskPushConfig",
			call: func(c *a2aclient.Client, task a2a.TaskID) error {
				cfg := hook()
				cfg.TaskID = task
				_, err := c.CreateTaskPushConfig(ctx, cfg)
				return err
			},
		},
		{
			name: "GetTaskPushConfig",
			call: func(c *a2aclient.Client, task a2a.TaskID) error {
				_, err := c.GetTaskPushConfig(ctx, &a2a.GetTaskPushConfigRequest{TaskID: task, ID: "x"})
				return err
			},
		},
		{
			name: "ListTaskPushConfigs",
			call: func(c *a2aclient.Client, task a2a.TaskID) error {
				_, err := c.ListTaskPushConfigs(ctx, &a2a.ListTaskPushConfigRequest{TaskID: task})
				return err
			},
		},
		{
			name: "DeleteTaskPushConfig",
			call: func(c *a2aclient.Client, task a2a.TaskID) error {
				return c.DeleteTaskPushConfig(ctx, &a2a.DeleteTaskPushConfigRequest{TaskID: task, ID: "x"})
			},
		},
	}
	for _, transport := range conformanceTransports {
		c := ts.clientOver(token, transport)
		task := send(t, c, userText("hello", "", ""))
		for _, tc := range tests {
			t.Run(string(transport)+"/"+tc.name, func(t *testing.T) {
				t.Parallel()
				if err := tc.call(c, task.ID); !errors.Is(err, a2a.ErrPushNotificationNotSupported) {
					t.Fatalf("%s error = %v, want %v", tc.name, err, a2a.ErrPushNotificationNotSupported)
				}
			})
		}
	}
}
