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
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/a2aproject/a2a-go/v2/a2aclient/agentcard"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/a2aproject/a2a-serve/internal/a2atext"
	"github.com/a2aproject/a2a-serve/internal/agentsrv/agentkey"
	"github.com/a2aproject/a2a-serve/internal/agentsrv/agentproxy"
	"github.com/a2aproject/a2a-serve/internal/agentsrv/approvals"
	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/channel/channeltest"
	"github.com/a2aproject/a2a-serve/internal/clierr"
	"github.com/a2aproject/a2a-serve/internal/contacts"
	"github.com/a2aproject/a2a-serve/internal/controlapi"
	"github.com/a2aproject/a2a-serve/internal/pairings"
	"github.com/a2aproject/a2a-serve/internal/store"
)

// addContact pairs caller with callee as a contact named name.
func addContact(t *testing.T, caller, callee *testServer, name, role string) {
	t.Helper()
	ctx := context.Background()
	code, err := controlapi.NewClient(callee.dir).Pair(ctx, controlapi.PairCodeRequest{Name: caller.srv.rt.Config().Name, Owner: role == auth.RoleOwner})
	if err != nil {
		t.Fatal(err)
	}
	c, err := controlapi.NewClient(caller.dir).AddContact(ctx, controlapi.ContactRequest{Name: name, URL: callee.localURL(), Code: code.Code})
	if err != nil {
		t.Fatalf("AddContact() error = %v", err)
	}
	if !c.HasToken || c.Via != contacts.ViaDirect {
		t.Fatalf("AddContact() = %+v, want an https contact with a token", c)
	}
}

func TestAgentCallsContact(t *testing.T) {
	t.Parallel()
	coder := startServer(t, serverOpts{name: "coder"})
	assistant := startServer(t, serverOpts{name: "assistant"})
	addContact(t, assistant, coder, "coder", auth.RoleAuthenticated)

	c := assistant.client(assistant.pair("me", auth.RoleOwner))
	task := send(t, c, userText("call coder hello", "", ""))
	wantResult(t, task, a2a.TaskStateCompleted, "Hello from fakeacp.")
	if !strings.Contains(a2atext.Result(task), "tool result") {
		t.Fatalf("task result = %q, want the called agent's result", a2atext.Result(task))
	}
	if strings.Contains(a2atext.Message(task.History[0]), agentsLinePrefix) {
		t.Fatalf("task history = %q, want the caller's message without the agents line", a2atext.Message(task.History[0]))
	}

	unknown := send(t, c, userText("call nobody hi", "", ""))
	wantResult(t, unknown, a2a.TaskStateCompleted, "404")
}

func TestCancelingTaskCancelsItsCalls(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	coder := startServer(t, serverOpts{name: "coder"})
	assistant := startServer(t, serverOpts{name: "assistant"})
	addContact(t, assistant, coder, "coder", auth.RoleAuthenticated)
	c := assistant.client(assistant.pair("me", auth.RoleOwner))

	task := sendAsync(t, c, userText("call coder slow", "", ""))
	var calls []store.CallTask
	deadline := time.Now().Add(10 * time.Second)
	for len(calls) == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
		var err error
		if calls, err = assistant.srv.store.CallTasksOf(ctx, task.ContextID, string(task.ID)); err != nil {
			t.Fatalf("store.CallTasksOf() error = %v", err)
		}
	}
	if len(calls) != 1 {
		t.Fatalf("store.CallTasksOf() = %+v, want the call to coder", calls)
	}
	coderClient := coder.client(coder.pair("me", auth.RoleOwner))
	waitState(t, coderClient, a2a.TaskID(calls[0].TaskID), a2a.TaskStateWorking)

	if _, err := c.CancelTask(ctx, &a2a.CancelTaskRequest{ID: task.ID}); err != nil {
		t.Fatalf("CancelTask() error = %v", err)
	}

	got := waitState(t, coderClient, a2a.TaskID(calls[0].TaskID), a2a.TaskStateCanceled)
	if got.Status.State != a2a.TaskStateCanceled {
		t.Fatalf("called task state after canceling the caller's task = %s, want canceled", got.Status.State)
	}
}

func TestAgentCallsContactWhileWaitingForApproval(t *testing.T) {
	t.Parallel()
	coder := startServer(t, serverOpts{name: "coder"})
	assistant := startServer(t, serverOpts{name: "assistant"})
	addContact(t, assistant, coder, "coder", auth.RoleAuthenticated)
	c := assistant.client(assistant.pair("me", auth.RoleOwner))
	signals := t.TempDir()

	task := send(t, c, userText("parallel "+signals+" coder hello", "", ""))
	if task.Status.State != a2a.TaskStateInputRequired {
		t.Fatalf("task state = %s, want input-required", task.Status.State)
	}
	if err := os.WriteFile(filepath.Join(signals, "go"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	result := waitFile(t, filepath.Join(signals, "result"))
	if !strings.HasPrefix(result, "tool result:") || !strings.Contains(result, "Hello from fakeacp.") {
		t.Fatalf("call while waiting for approval = %q, want the called agent's result", result)
	}

	wantResult(t, send(t, c, userText("approve", string(task.ID), task.ContextID)), a2a.TaskStateCompleted, "Pushed (allow).")
}

// waitFile waits until path exists and returns its content.
func waitFile(t *testing.T, path string) string {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		data, err := os.ReadFile(path)
		if err == nil {
			return string(data)
		}
		if time.Now().After(deadline) {
			t.Fatalf("os.ReadFile(%s) error = %v", path, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestRemoteApprovalIsRelayedToCallerChat(t *testing.T) {
	t.Parallel()
	coder := startServer(t, serverOpts{name: "coder"})
	assistant := startServer(t, serverOpts{name: "assistant", channel: channeltest.NewChannel("fake")})
	addContact(t, assistant, coder, "coder", auth.RoleOwner)
	linkOwner(t, assistant, "telegram:1", "dm1")

	assistant.ch.Write("telegram:1", "dm1", "call coder ask")
	ref, q := assistant.ch.WaitFor(t, "dm1", channeltest.HasQuestion)
	if !strings.Contains(q.Text, "coder wants to run git push") {
		t.Fatalf("question text = %q, want it to name coder and the tool", q.Text)
	}
	assistant.ch.Click("telegram:1", "dm1", ref, q.Question, 0)
	assistant.ch.WaitFor(t, "dm1", channeltest.HasText("✓ Allow once by owner"))

	ctx := context.Background()
	contextID, err := assistant.srv.store.ContextOf(ctx, "fake", "dm1")
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		last, err := assistant.srv.tasks.LatestInContext(ctx, contextID)
		if err != nil {
			t.Fatal(err)
		}
		if len(last.History) > 0 && strings.Contains(a2atext.Message(last.History[0]), "Pushed (allow).") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("latest turn history = %v, want coder's result posted into the conversation", last.History)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func TestRelayedApprovalSurvivesRestart(t *testing.T) {
	t.Parallel()
	coder := startServer(t, serverOpts{name: "coder"})
	ch := channeltest.NewChannel("fake")
	ch.SendDelay = 200 * time.Millisecond
	assistant := startServer(t, serverOpts{name: "assistant", channel: ch})
	addContact(t, assistant, coder, "coder", auth.RoleOwner)
	linkOwner(t, assistant, "telegram:1", "dm1")

	assistant.ch.Write("telegram:1", "dm1", "call coder ask")
	ref, q := assistant.ch.WaitFor(t, "dm1", channeltest.HasQuestion)
	waitQuestionMessage(t, assistant, q.Question.ID)
	root := assistant.dir.Root
	assistant.stop()

	restarted := startServer(t, serverOpts{name: "assistant", dir: root, channel: channeltest.NewChannel("fake")})
	restarted.ch.WaitReady(t)
	time.Sleep(3 * testTimings.WatchMaxInterval)
	if n := restarted.ch.Questions(); n != 0 {
		t.Fatalf("questions asked again after the restart = %d, want 0", n)
	}
	restarted.ch.Click("telegram:1", "dm1", ref, q.Question, 0)
	restarted.ch.WaitEdited(t, ref, "✓ Allow once by owner")
}

func TestRemoteApprovalIsAskedOnTheCallersTask(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		prompt        string
		wantQuestions int
		wantResult    string
	}{
		{
			name:          "one question",
			prompt:        "call coder ask",
			wantQuestions: 1,
			wantResult:    "Pushed (allow).",
		},
		{
			name:          "a question after the answer",
			prompt:        "call coder ask twice",
			wantQuestions: 2,
			wantResult:    "Pushed twice (allow, allow).",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			coder := startServer(t, serverOpts{name: "coder"})
			assistant := startServer(t, serverOpts{name: "assistant"})
			addContact(t, assistant, coder, "coder", auth.RoleOwner)
			c := assistant.client(assistant.pair("me", auth.RoleOwner))

			task := send(t, c, userText(tc.prompt, "", ""))
			for range tc.wantQuestions {
				q := approvals.FromStatus(task.Status)
				if q == nil || q.Type != approvals.TypePermission || !strings.Contains(q.Text, "coder wants to run git push") {
					t.Fatalf("task status = %s (%s), want coder's permission question", task.Status.State, a2atext.Message(task.Status.Message))
				}
				task = send(t, c, userText("approve", string(task.ID), task.ContextID))
			}

			if task.Status.State != a2a.TaskStateCompleted {
				t.Fatalf("task state = %s (%s), want completed", task.Status.State, a2atext.Message(task.Status.Message))
			}
			if !slices.ContainsFunc(task.History, func(m *a2a.Message) bool { return strings.Contains(a2atext.Message(m), tc.wantResult) }) {
				t.Fatalf("task history = %v, want coder's result %q", historyTexts(task), tc.wantResult)
			}
		})
	}
}

func TestRemoteApprovalOnTheCallersTaskKeepsUnclearReply(t *testing.T) {
	t.Parallel()
	coder := startServer(t, serverOpts{name: "coder"})
	assistant := startServer(t, serverOpts{name: "assistant"})
	addContact(t, assistant, coder, "coder", auth.RoleOwner)
	c := assistant.client(assistant.pair("me", auth.RoleOwner))

	task := send(t, c, userText("call coder ask", "", ""))
	again := send(t, c, userText("maybe later", string(task.ID), task.ContextID))
	if q := approvals.FromStatus(again.Status); q == nil || !strings.Contains(q.Text, "coder wants to run git push") {
		t.Fatalf("task status = %s (%s), want coder's question asked again", again.Status.State, a2atext.Message(again.Status.Message))
	}

	done := send(t, c, userText("deny", string(task.ID), task.ContextID))
	if done.Status.State != a2a.TaskStateCompleted {
		t.Fatalf("task state = %s (%s), want completed", done.Status.State, a2atext.Message(done.Status.Message))
	}
}

func TestRemoteApprovalOnTheCallersTaskSurvivesRestart(t *testing.T) {
	t.Parallel()
	coder := startServer(t, serverOpts{name: "coder"})
	assistant := startServer(t, serverOpts{name: "assistant"})
	addContact(t, assistant, coder, "coder", auth.RoleOwner)
	token := assistant.pair("me", auth.RoleOwner)
	task := send(t, assistant.client(token), userText("call coder ask", "", ""))
	if q := approvals.FromStatus(task.Status); q == nil {
		t.Fatalf("task status = %s (%s), want coder's question", task.Status.State, a2atext.Message(task.Status.Message))
	}
	root := assistant.dir.Root
	assistant.stop()

	restarted := startServer(t, serverOpts{name: "assistant", dir: root})
	done := send(t, restarted.client(token), userText("approve", string(task.ID), task.ContextID))
	if done.Status.State != a2a.TaskStateCompleted {
		t.Fatalf("task state = %s (%s), want completed", done.Status.State, a2atext.Message(done.Status.Message))
	}
	if !slices.ContainsFunc(done.History, func(m *a2a.Message) bool { return strings.Contains(a2atext.Message(m), "Pushed (allow).") }) {
		t.Fatalf("task history = %v, want coder's result", historyTexts(done))
	}
}

func historyTexts(task *a2a.Task) []string {
	var out []string
	for _, m := range task.History {
		out = append(out, a2atext.Message(m))
	}
	return out
}

// waitQuestionMessage waits until the server recorded the message it sent
// for a question, which it does after sending it.
func waitQuestionMessage(t *testing.T, ts *testServer, questionID string) {
	t.Helper()
	db, err := sql.Open("sqlite", ts.dir.DBPath())
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var n int
		err := db.QueryRow(`SELECT COUNT(*) FROM question_messages WHERE question_id = ?`, questionID).Scan(&n)
		if err == nil && n > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("question messages of %s = %d, %v, want one", questionID, n, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestCallerAtHopLimitIsRefused(t *testing.T) {
	t.Parallel()
	coder := startServer(t, serverOpts{name: "coder"})
	c := coder.client(coder.pair("assistant", auth.RoleOwner))
	tests := []struct {
		name    string
		hops    int
		wantErr bool
	}{
		{name: "first hop", hops: 1},
		{name: "past the limit", hops: auth.MaxHops + 1, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := a2aclient.AttachServiceParams(context.Background(), a2aclient.ServiceParams{auth.HeaderHops: {strconv.Itoa(tc.hops)}})
			_, err := c.SendMessage(ctx, &a2a.SendMessageRequest{Message: userText("hello", "", "")})
			if (err != nil) != tc.wantErr {
				t.Fatalf("SendMessage() at %d hops error = %v, want error %v", tc.hops, err, tc.wantErr)
			}
			if err != nil && !strings.Contains(err.Error(), "too many agents") {
				t.Fatalf("SendMessage() error = %v, want the hop limit", err)
			}
		})
	}
}

func TestAgentsURLHandles(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{})
	p := ts.srv.claims
	ctx := context.Background()
	h, err := p.handle(ctx, "ctx1")
	if err != nil {
		t.Fatalf("handle() error = %v", err)
	}
	if again, err := p.handle(ctx, "ctx1"); err != nil || again != h {
		t.Fatalf("handle() again = %q, %v, want %q", again, err, h)
	}
	if other, err := p.handle(ctx, "ctx2"); err != nil || other == h {
		t.Fatalf("handle() of another conversation = %q, %v, want a different handle", other, err)
	}
	if len(h) != handleLen {
		t.Fatalf("handle() = %q, want %d characters", h, handleLen)
	}
	if _, err := p.ClaimsOf(ctx, h, ""); !errors.Is(err, agentproxy.ErrNoTurn) {
		t.Fatalf("claimsOf() before a turn error = %v, want %v", err, agentproxy.ErrNoTurn)
	}

	turn := agentproxy.Claims{ContextID: "ctx1", User: auth.Principal{ID: "pair:anna", Role: auth.RoleAuthenticated, Identity: auth.Identity{Hops: 1}}}
	end := p.beginTurn(turn)
	got, err := p.ClaimsOf(ctx, h, "")
	if err != nil {
		t.Fatalf("claimsOf() during a turn error = %v", err)
	}
	if diff := cmp.Diff(turn, got); diff != "" {
		t.Fatalf("claimsOf() wrong result (-want +got) diff = %s", diff)
	}
	end()

	if _, err := p.ClaimsOf(ctx, h, ""); !errors.Is(err, agentproxy.ErrNoTurn) {
		t.Fatalf("claimsOf() after the turn error = %v, want %v", err, agentproxy.ErrNoTurn)
	}
	if _, err := p.ClaimsOf(ctx, "unknown", ""); !errors.Is(err, agentproxy.ErrUnknownHandle) {
		t.Fatalf("claimsOf(unknown) error = %v, want %v", err, agentproxy.ErrUnknownHandle)
	}
}

// turnURL returns the agents URL of a conversation with a running turn of
// the owner, which ends with the test.
func turnURL(t *testing.T, ts *testServer, contextID string) string {
	t.Helper()
	return turnURLOf(t, ts, contextID, auth.Owner())
}

// turnURLOf returns the agents URL of a conversation with a running turn of
// user, which ends with the test.
func turnURLOf(t *testing.T, ts *testServer, contextID string, user auth.Principal) string {
	t.Helper()
	u, err := ts.srv.claims.agentsURL(context.Background(), contextID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ts.srv.claims.beginTurn(agentproxy.Claims{ContextID: contextID, User: user}))
	return u
}

// harnessClient returns a client which reaches ts the way the owner's
// agent process does.
func harnessClient(ts *testServer) *http.Client {
	return socketClient(ts.srv.bridge.socket)
}

// socketClient returns a client which sends every request through the Unix
// socket at path.
func socketClient(path string) *http.Client {
	var d net.Dialer
	return &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return d.DialContext(ctx, "unix", path)
	}}}
}

// agentClient returns a client of an agent behind an agents URL.
func agentClient(t *testing.T, hc *http.Client, agentURL string) (*a2aclient.Client, error) {
	t.Helper()
	ctx := context.Background()
	card, err := agentcard.NewResolver(hc).Resolve(ctx, agentURL)
	if err != nil {
		return nil, err
	}
	client, err := a2aclient.NewFromCard(ctx, card, a2aclient.WithJSONRPCTransport(hc), a2aclient.WithRESTTransport(hc))
	if err != nil {
		t.Fatalf("NewFromCard() error = %v", err)
	}
	t.Cleanup(func() {
		if err := client.Destroy(); err != nil {
			t.Errorf("Destroy() error = %v", err)
		}
	})
	return client, nil
}

// callAgent sends text through an agents URL and waits for a settled task.
func callAgent(t *testing.T, hc *http.Client, agentURL, text string) (*a2a.Task, error) {
	t.Helper()
	ctx := context.Background()
	client, err := agentClient(t, hc, agentURL)
	if err != nil {
		return nil, err
	}
	res, err := client.SendMessage(ctx, &a2a.SendMessageRequest{Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(text))})
	if err != nil {
		return nil, err
	}
	task, ok := res.(*a2a.Task)
	if !ok {
		t.Fatalf("SendMessage() = %T, want a task", res)
	}
	deadline := time.Now().Add(10 * time.Second)
	for !task.Status.State.Terminal() && task.Status.State != a2a.TaskStateInputRequired && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
		if task, err = client.GetTask(ctx, &a2a.GetTaskRequest{ID: task.ID}); err != nil {
			t.Fatalf("GetTask() error = %v", err)
		}
	}
	return task, nil
}

func TestAgentsProxy(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{name: "assistant"})
	addNode(t, ts, "grok", 0)
	u := turnURL(t, ts, "c1")
	idle, err := ts.srv.claims.agentsURL(context.Background(), "c2")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		url     string
		want    string
		wantErr bool
	}{
		{name: "node", url: u + "/grok", want: "grok says: hi"},
		{name: "unknown agent", url: u + "/nobody", wantErr: true},
		{name: "unknown handle", url: socketHost + "/unknown/grok", wantErr: true},
		{name: "conversation without a running turn", url: idle + "/grok", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			task, err := callAgent(t, harnessClient(ts), tc.url, "hi")
			if (err != nil) != tc.wantErr {
				t.Fatalf("callAgent() error = %v, want error %v", err, tc.wantErr)
			}
			if err == nil && a2atext.Result(task) != tc.want {
				t.Fatalf("task result = %q, want %q", a2atext.Result(task), tc.want)
			}
		})
	}
}

func TestAgentsProxyServesOnlyTheConversationsOfItsProcess(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{name: "assistant"})
	addNode(t, ts, "grok", 0)
	anna := ts.client(ts.pair("anna", auth.RoleAuthenticated))
	annaContext := send(t, anna, userText("hello", "", "")).ContextID
	annaURL := turnURLOf(t, ts, annaContext, auth.Principal{ID: "pair:anna", Role: auth.RoleAuthenticated})
	ownerURL := turnURL(t, ts, "c1")
	annaSocket, err := ts.srv.harness.open(agentkey.User("pair:anna"))
	if err != nil {
		t.Fatalf("harnessListeners.open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := annaSocket.close(); err != nil {
			t.Errorf("harnessEndpoint.close() error = %v", err)
		}
	})
	tests := []struct {
		name    string
		socket  string
		url     string
		wantErr bool
	}{
		{name: "owner's socket and conversation", socket: ts.srv.bridge.socket, url: ownerURL},
		{name: "anna's socket and conversation", socket: annaSocket.socket, url: annaURL},
		{name: "owner's socket and anna's conversation", socket: ts.srv.bridge.socket, url: annaURL, wantErr: true},
		{name: "anna's socket and owner's conversation", socket: annaSocket.socket, url: ownerURL, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			task, err := callAgent(t, socketClient(tc.socket), tc.url+"/grok", "hi")
			if (err != nil) != tc.wantErr {
				t.Fatalf("callAgent() error = %v, want error %v", err, tc.wantErr)
			}
			if err == nil && a2atext.Result(task) != "grok says: hi" {
				t.Fatalf("task result = %q, want %q", a2atext.Result(task), "grok says: hi")
			}
		})
	}
}

func TestHarnessEndpointsOverTCP(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{name: "assistant"})
	addNode(t, ts, "grok", 0)
	anna := ts.client(ts.pair("anna", auth.RoleAuthenticated))
	annaContext := send(t, anna, userText("hello", "", "")).ContextID
	annaHandle := strings.TrimPrefix(turnURLOf(t, ts, annaContext, auth.Principal{ID: "pair:anna", Role: auth.RoleAuthenticated}), socketHost+"/")
	ownerHandle := strings.TrimPrefix(turnURL(t, ts, "c1"), socketHost+"/")
	hl, err := listenHarnessTCP("127.0.0.1", "127.0.0.1")
	if err != nil {
		t.Fatalf("listenHarnessTCP() error = %v", err)
	}
	t.Cleanup(func() {
		if err := hl.close(context.Background()); err != nil {
			t.Errorf("harnessListeners.close() error = %v", err)
		}
	})
	shutdown := hl.serve(context.Background(), ts.srv.proxy.Routes(hl.processOf), make(chan error, 1))
	t.Cleanup(func() {
		if err := shutdown(context.Background()); err != nil {
			t.Errorf("shutting down the harness listener: %v", err)
		}
	})
	owner, annaKey := hl.openKey(""), hl.openKey(agentkey.User("pair:anna"))
	closed := hl.openKey("")
	if err := closed.close(); err != nil {
		t.Fatalf("harnessEndpoint.close() error = %v", err)
	}
	tests := []struct {
		name   string
		key    string
		handle string
		want   int
	}{
		{name: "owner's key and conversation", key: owner.key, handle: ownerHandle, want: http.StatusOK},
		{name: "anna's key and conversation", key: annaKey.key, handle: annaHandle, want: http.StatusOK},
		{name: "owner's key and anna's conversation", key: owner.key, handle: annaHandle, want: http.StatusNotFound},
		{name: "anna's key and owner's conversation", key: annaKey.key, handle: ownerHandle, want: http.StatusNotFound},
		{name: "closed key", key: closed.key, handle: ownerHandle, want: http.StatusUnauthorized},
		{name: "no key", handle: ownerHandle, want: http.StatusUnauthorized},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			header := ""
			if tc.key != "" {
				header = "Bearer " + tc.key
			}
			got := getURL(t, hl.url+"/"+tc.handle+"/grok/.well-known/agent-card.json", header)
			if got.Status != tc.want {
				t.Fatalf("GET agent card status = %d (%s), want %d", got.Status, got.Error, tc.want)
			}
		})
	}
}

func TestAgentsProxyChecksTheTurnsRole(t *testing.T) {
	t.Parallel()
	coder := startServer(t, serverOpts{name: "coder"})
	ts := startServer(t, serverOpts{name: "assistant"})
	addNode(t, ts, "grok", 0)
	addContact(t, ts, coder, "coder", auth.RoleAuthenticated)
	tests := []struct {
		name    string
		user    auth.Principal
		agent   string
		wantErr bool
	}{
		{name: "owner calls a node", user: auth.Owner(), agent: "grok"},
		{name: "authenticated calls a node", user: auth.Principal{ID: "pair:anna", Role: auth.RoleAuthenticated}, agent: "grok"},
		{name: "anonymous calls a node", user: auth.Principal{ID: "telegram:9", Role: auth.RoleAnonymous}, agent: "grok", wantErr: true},
		{name: "anonymous calls a contact", user: auth.Principal{ID: "telegram:9", Role: auth.RoleAnonymous}, agent: "coder", wantErr: true},
		{name: "anonymous calls the schedules agent", user: auth.Principal{ID: "telegram:9", Role: auth.RoleAnonymous}, agent: pairings.SchedulesAgent, wantErr: true},
		{name: "user without a role calls a node", user: auth.Principal{ID: "pair:gone"}, agent: "grok", wantErr: true},
	}
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			u := turnURLOf(t, ts, fmt.Sprintf("c%d", i), tc.user)
			_, err := callAgent(t, harnessClient(ts), u+"/"+tc.agent, "hi")
			if (err != nil) != tc.wantErr {
				t.Fatalf("callAgent() error = %v, want error %v", err, tc.wantErr)
			}
		})
	}
}

func TestAgentsProxyOverTCPNeedsKey(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{name: "assistant"})
	addNode(t, ts, "grok", 0)
	hs := httptest.NewServer(ts.srv.proxy.Routes(harnessKeyCaller("harness-key")))
	t.Cleanup(hs.Close)
	h := strings.TrimPrefix(turnURL(t, ts, "c1"), socketHost+"/")
	tests := []struct {
		name   string
		header string
		want   int
	}{
		{name: "no key", want: http.StatusUnauthorized},
		{name: "wrong key", header: "Bearer nope", want: http.StatusUnauthorized},
		{name: "key", header: "Bearer harness-key", want: http.StatusOK},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodGet, hs.URL+"/"+h+"/grok/.well-known/agent-card.json", nil)
			if err != nil {
				t.Fatal(err)
			}
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			resp, err := testHTTP.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			if err := resp.Body.Close(); err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != tc.want {
				t.Fatalf("GET card status = %d, want %d", resp.StatusCode, tc.want)
			}
		})
	}
}

func TestWriteHarnessHelper(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("the helper is a POSIX shell script")
	}
	home := t.TempDir()
	env, err := writeHarnessHelper(home, "host.docker.internal", "k3y")
	if err != nil {
		t.Fatalf("writeHarnessHelper() error = %v", err)
	}
	if want := "A2ACLI_CREDENTIAL_HELPER=host.docker.internal=/home/agent/" + harnessHelperPath; env != want {
		t.Fatalf("writeHarnessHelper() = %q, want %q", env, want)
	}
	out, err := runHelper(filepath.Join(home, filepath.FromSlash(harnessHelperPath)), `{"uri":"http://host.docker.internal:4000/k3/coder"}`)
	if err != nil {
		t.Fatalf("credential helper error = %v", err)
	}
	var got struct {
		Headers map[string][]string `json:"headers"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("json.Unmarshal(%q) error = %v", out, err)
	}
	if diff := cmp.Diff(map[string][]string{"Authorization": {"Bearer k3y"}}, got.Headers); diff != "" {
		t.Fatalf("credential helper wrong result (-want +got) diff = %s", diff)
	}
}

// runHelper runs a just-written executable. A child forked meanwhile by a
// parallel test holds its write descriptor until exec, failing the run with
// ETXTBSY (golang.org/issue/22315), so the run is retried.
func runHelper(path, stdin string) ([]byte, error) {
	deadline := time.Now().Add(5 * time.Second)
	for {
		cmd := exec.Command(path, "get")
		cmd.Stdin = strings.NewReader(stdin)
		out, err := cmd.Output()
		if !errors.Is(err, syscall.ETXTBSY) || time.Now().After(deadline) {
			return out, err
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestHarnessCannotAnswerRemoteApproval(t *testing.T) {
	t.Parallel()
	coder := startServer(t, serverOpts{name: "coder"})
	assistant := startServer(t, serverOpts{name: "assistant"})
	addContact(t, assistant, coder, "coder", auth.RoleOwner)
	u := turnURL(t, assistant, "c1")
	hc := harnessClient(assistant)
	task, err := callAgent(t, hc, u+"/coder", "ask")
	if err != nil {
		t.Fatalf("callAgent() error = %v", err)
	}
	if task.Status.State != a2a.TaskStateInputRequired {
		t.Fatalf("task state = %v, want a task waiting for an approval", task.Status.State)
	}
	ctx := context.Background()
	card, err := agentcard.NewResolver(hc).Resolve(ctx, u+"/coder")
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	client, err := a2aclient.NewFromCard(ctx, card, a2aclient.WithJSONRPCTransport(hc), a2aclient.WithRESTTransport(hc))
	if err != nil {
		t.Fatalf("NewFromCard() error = %v", err)
	}
	answer := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("allow"))
	answer.TaskID, answer.ContextID = task.ID, task.ContextID
	if _, err := client.SendMessage(ctx, &a2a.SendMessageRequest{Message: answer}); err == nil {
		t.Fatalf("SendMessage() answering an approval error = nil, want error")
	}
}

func TestRemoteApprovalOfMemberGoesToMember(t *testing.T) {
	t.Parallel()
	coder := startServer(t, serverOpts{name: "coder"})
	assistant := startServer(t, serverOpts{name: "assistant", channel: channeltest.NewChannel("fake")})
	addContact(t, assistant, coder, "coder", auth.RoleOwner)
	linkOwner(t, assistant, "telegram:1", "dm1")
	linkAs(t, assistant, "anna", auth.RoleAuthenticated, "telegram:2", "dm2")

	assistant.ch.Write("telegram:2", "dm2", "call coder ask")
	ref, q := assistant.ch.WaitFor(t, "dm2", channeltest.HasQuestion)
	if !strings.Contains(q.Text, "coder wants to run git push") {
		t.Fatalf("question text = %q, want it to name coder and the tool", q.Text)
	}
	assistant.ch.Click("telegram:2", "dm2", ref, q.Question, 0)
	assistant.ch.WaitEdited(t, ref, "✓ Allow once by")
}

func TestContactNameTakenByNode(t *testing.T) {
	t.Parallel()
	coder := startServer(t, serverOpts{name: "coder"})
	assistant := startServer(t, serverOpts{name: "assistant"})
	addNode(t, assistant, "grok", 0)

	_, err := controlapi.NewClient(assistant.dir).AddContact(context.Background(), controlapi.ContactRequest{Name: "grok", URL: coder.localURL()})
	if err == nil || !strings.Contains(err.Error(), "webhook node") {
		t.Fatalf("AddContact() error = %v, want the name taken by a node", err)
	}
}

func TestContactUnreachableURL(t *testing.T) {
	t.Parallel()
	assistant := startServer(t, serverOpts{name: "assistant"})

	_, err := controlapi.NewClient(assistant.dir).AddContact(context.Background(), controlapi.ContactRequest{Name: "grok", URL: "http://127.0.0.1:1"})

	got := clierr.Classify(err)
	if got == nil || got.Code != clierr.CodeIO || got.Exit != 3 || got.Hint != clierr.IOHint {
		t.Fatalf("AddContact() error = %+v, want %s (exit 3) with a hint, as while the agent is stopped", got, clierr.CodeIO)
	}
}

func TestAgentsURLReachesOnlyItsTasks(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{name: "assistant"})
	addNode(t, ts, "grok", 0)
	hc := harnessClient(ts)
	task, err := callAgent(t, hc, turnURL(t, ts, "c1")+"/grok", "hi")
	if err != nil {
		t.Fatalf("callAgent() error = %v", err)
	}
	own, err := agentClient(t, hc, turnURL(t, ts, "c1")+"/grok")
	if err != nil {
		t.Fatal(err)
	}
	other, err := agentClient(t, hc, turnURL(t, ts, "c2")+"/grok")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	tests := []struct {
		name    string
		call    func() error
		wantErr bool
	}{
		{name: "get from its conversation", call: func() error {
			_, err := own.GetTask(ctx, &a2a.GetTaskRequest{ID: task.ID})
			return err
		}},
		{name: "get from another conversation", wantErr: true, call: func() error {
			_, err := other.GetTask(ctx, &a2a.GetTaskRequest{ID: task.ID})
			return err
		}},
		{name: "continue from another conversation", wantErr: true, call: func() error {
			_, err := other.SendMessage(ctx, &a2a.SendMessageRequest{Message: userText("more", string(task.ID), task.ContextID)})
			return err
		}},
		{name: "join its context from another conversation", wantErr: true, call: func() error {
			_, err := other.SendMessage(ctx, &a2a.SendMessageRequest{Message: userText("more", "", task.ContextID)})
			return err
		}},
		{name: "cancel from another conversation", wantErr: true, call: func() error {
			_, err := other.CancelTask(ctx, &a2a.CancelTaskRequest{ID: task.ID})
			return err
		}},
		{name: "list from another conversation", call: func() error {
			res, err := other.ListTasks(ctx, &a2a.ListTasksRequest{})
			if err == nil && len(res.Tasks) > 0 {
				return fmt.Errorf("listed %d tasks of another conversation", len(res.Tasks))
			}
			return err
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.call(); (err != nil) != tc.wantErr {
				t.Fatalf("call error = %v, want error %v", err, tc.wantErr)
			}
		})
	}
}

func TestTurnOpensAgentsURLForItsCaller(t *testing.T) {
	t.Parallel()
	anna := auth.Principal{ID: "pair:anna", Role: auth.RoleAuthenticated}
	tests := []struct {
		name    string
		ctx     context.Context
		execCtx *a2asrv.ExecutorContext
		want    auth.Principal
	}{
		{
			name:    "caller",
			ctx:     auth.InProcess(context.Background(), anna),
			execCtx: &a2asrv.ExecutorContext{ContextID: "c1", User: &a2asrv.User{Name: "someone else"}},
			want:    anna,
		},
		{
			name:    "user of a stored execution",
			ctx:     context.Background(),
			execCtx: &a2asrv.ExecutorContext{ContextID: "c1", User: &a2asrv.User{Name: anna.ID}},
			want:    auth.Principal{ID: anna.ID},
		},
	}
	ts := startServer(t, serverOpts{})
	p := ts.srv.claims
	h, err := p.handle(context.Background(), "c1")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			end := p.turn(tc.ctx, tc.execCtx)
			got, err := p.ClaimsOf(context.Background(), h, "")
			end()
			if err != nil {
				t.Fatalf("claimsOf() during the turn error = %v", err)
			}
			if diff := cmp.Diff(agentproxy.Claims{ContextID: "c1", User: tc.want}, got); diff != "" {
				t.Fatalf("claimsOf() wrong result (-want +got) diff = %s", diff)
			}
			if _, err := p.ClaimsOf(context.Background(), h, ""); !errors.Is(err, agentproxy.ErrNoTurn) {
				t.Fatalf("claimsOf() after the turn error = %v, want %v", err, agentproxy.ErrNoTurn)
			}
		})
	}
}

func TestAnswerAfterRestartOfUnfinishedTask(t *testing.T) {
	t.Parallel()
	const interrupted = "The server restarted before this task finished. Send the message again."
	tests := []struct {
		name       string
		prompt     string
		relay      bool
		killed     bool
		wantState  a2a.TaskState
		wantStatus string
		answer     string
		wantErr    string
		wantResult string
	}{
		{
			name:       "working when the server stops",
			prompt:     "slow",
			wantState:  a2a.TaskStateFailed,
			wantStatus: "The server stopped before this task finished. Send the message again.",
			answer:     "approve",
			wantErr:    "the server stopped or restarted",
		},
		{
			name:       "working when the server is killed",
			killed:     true,
			wantState:  a2a.TaskStateFailed,
			wantStatus: interrupted,
			answer:     "approve",
			wantErr:    "the server stopped or restarted",
		},
		{
			name:       "input required by the agent itself",
			prompt:     "ask",
			wantState:  a2a.TaskStateFailed,
			wantStatus: interrupted,
			answer:     "approve",
			wantErr:    "the server stopped or restarted",
		},
		{
			name:       "input required relaying another agent's question",
			prompt:     "call coder ask",
			relay:      true,
			wantState:  a2a.TaskStateInputRequired,
			wantStatus: "coder wants to run git push",
			answer:     "approve",
			wantResult: "Pushed (allow).",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			assistant := startServer(t, serverOpts{name: "assistant"})
			if tc.relay {
				coder := startServer(t, serverOpts{name: "coder"})
				addContact(t, assistant, coder, "coder", auth.RoleOwner)
			}
			token := assistant.pair("me", auth.RoleOwner)
			task := &a2a.Task{ID: "killed", ContextID: "c1", Status: a2a.TaskStatus{State: a2a.TaskStateWorking}}
			if !tc.killed {
				c := assistant.client(token)
				task = sendAsync(t, c, userText(tc.prompt, "", ""))
				if tc.prompt == "slow" {
					waitState(t, c, task.ID, a2a.TaskStateWorking)
				} else {
					task = waitState(t, c, task.ID, a2a.TaskStateInputRequired)
				}
			}
			root := assistant.dir.Root
			assistant.stop()
			if tc.killed {
				seedTask(t, assistant.dir.DBPath(), task)
			}

			restarted := startServer(t, serverOpts{name: "assistant", dir: root})
			c := restarted.client(token)
			got, err := c.GetTask(ctx, &a2a.GetTaskRequest{ID: task.ID})
			if err != nil {
				t.Fatalf("GetTask() error = %v", err)
			}
			if got.Status.State != tc.wantState || !strings.Contains(a2atext.Message(got.Status.Message), tc.wantStatus) {
				t.Fatalf("GetTask() status = %s (%s), want %s (%s)", got.Status.State, a2atext.Message(got.Status.Message), tc.wantState, tc.wantStatus)
			}

			res, err := c.SendMessage(ctx, &a2a.SendMessageRequest{Message: userText(tc.answer, string(task.ID), task.ContextID)})
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("SendMessage() = %v, %v, want error containing %q", res, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("SendMessage() error = %v", err)
			}
			answered, ok := res.(*a2a.Task)
			if !ok || answered.Status.State != a2a.TaskStateCompleted {
				t.Fatalf("SendMessage() = %v, want a completed task", res)
			}
			if !slices.ContainsFunc(answered.History, func(m *a2a.Message) bool { return strings.Contains(a2atext.Message(m), tc.wantResult) }) {
				t.Fatalf("task history = %v, want %q", historyTexts(answered), tc.wantResult)
			}
		})
	}
}

func seedTask(t *testing.T, dbPath string, task *a2a.Task) {
	t.Helper()
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	viewer := func(context.Context) (string, bool, error) { return "pair:me", true, nil }
	if _, err := st.Tasks("", viewer).Create(context.Background(), task); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("store.Close() error = %v", err)
	}
}
