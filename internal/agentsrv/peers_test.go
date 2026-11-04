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
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"net"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/a2aproject/a2a-go/v2/a2aclient/agentcard"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/controlapi"
	"github.com/a2aproject/a2a-serve/internal/store"
)

// addContact pairs caller with callee as a contact named name.
func addContact(t *testing.T, caller, callee *testServer, name, role string) {
	t.Helper()
	ctx := context.Background()
	code, err := controlapi.NewClient(callee.dir).Pair(ctx, controlapi.PairCodeRequest{Name: caller.srv.config().Name, Owner: role == auth.RoleOwner})
	if err != nil {
		t.Fatal(err)
	}
	c, err := controlapi.NewClient(caller.dir).AddContact(ctx, controlapi.ContactRequest{Name: name, URL: callee.localURL(), Code: code.Code})
	if err != nil {
		t.Fatalf("AddContact() error = %v", err)
	}
	if !c.HasToken || c.Via != ViaHTTPS {
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
	if !strings.Contains(resultText(task), "tool result") {
		t.Fatalf("task result = %q, want the called agent's result", resultText(task))
	}
	if strings.Contains(messageText(task.History[0]), agentsLinePrefix) {
		t.Fatalf("task history = %q, want the caller's message without the agents line", messageText(task.History[0]))
	}

	unknown := send(t, c, userText("call nobody hi", "", ""))
	wantResult(t, unknown, a2a.TaskStateCompleted, "404")
}

func TestRemoteApprovalIsRelayedToCallerChat(t *testing.T) {
	t.Parallel()
	coder := startServer(t, serverOpts{name: "coder"})
	assistant := startServer(t, serverOpts{name: "assistant", channel: newFakeChannel("fake")})
	addContact(t, assistant, coder, "coder", auth.RoleOwner)
	linkOwner(t, assistant, "telegram:1", "dm1")

	assistant.ch.write("telegram:1", "dm1", "call coder ask")
	ref, q := assistant.ch.waitFor(t, "dm1", hasQuestion)
	if !strings.Contains(q.Text, "coder wants to run git push") {
		t.Fatalf("question text = %q, want it to name coder and the tool", q.Text)
	}
	assistant.ch.click("telegram:1", "dm1", ref, q.Question, 0)
	assistant.ch.waitFor(t, "dm1", hasText("✓ Allow by owner"))

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
		if len(last.History) > 0 && strings.Contains(messageText(last.History[0]), "Pushed (allow).") {
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
	assistant := startServer(t, serverOpts{name: "assistant", channel: newFakeChannel("fake")})
	addContact(t, assistant, coder, "coder", auth.RoleOwner)
	linkOwner(t, assistant, "telegram:1", "dm1")

	assistant.ch.write("telegram:1", "dm1", "call coder ask")
	ref, q := assistant.ch.waitFor(t, "dm1", hasQuestion)
	waitPendingQuestion(t, assistant, "fake", "dm1", q.Question.ID)
	root := assistant.dir.Root
	assistant.stop()

	restarted := startServer(t, serverOpts{name: "assistant", dir: root, channel: newFakeChannel("fake")})
	restarted.ch.waitReady(t)
	time.Sleep(3 * watchMaxInterval)
	if n := restarted.ch.questions(); n != 0 {
		t.Fatalf("questions asked again after the restart = %d, want 0", n)
	}
	restarted.ch.click("telegram:1", "dm1", ref, q.Question, 0)
	restarted.ch.waitEdited(t, ref, "✓ Allow by owner")
}

// waitPendingQuestion waits until the server recorded the question it sent
// to a conversation, which it does after sending it.
func waitPendingQuestion(t *testing.T, ts *testServer, ch, conv, questionID string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		id, err := ts.srv.store.PendingQuestion(context.Background(), ch, conv)
		if err == nil && id == questionID {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("PendingQuestion() = %q, %v, want %q", id, err, questionID)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestContactClientIsShared(t *testing.T) {
	t.Parallel()
	coder := startServer(t, serverOpts{name: "coder"})
	assistant := startServer(t, serverOpts{name: "assistant"})
	addContact(t, assistant, coder, "coder", auth.RoleOwner)
	p, ctx := assistant.srv.peers, context.Background()

	first, err := p.contactClient(ctx, "coder")
	if err != nil {
		t.Fatalf("contactClient() error = %v", err)
	}
	for _, user := range []auth.Principal{auth.Owner(), {ID: "pair:anna", Role: auth.RoleAuthenticated}} {
		tg, err := p.target(ctx, "coder", user, 1)
		if err != nil {
			t.Fatalf("target() error = %v", err)
		}
		if got := tg.(remoteTarget).c; got != first {
			t.Fatalf("target() for %s built a new client, want the contact's client", user.ID)
		}
	}

	c, err := p.store.Contact(ctx, "coder")
	if err != nil {
		t.Fatal(err)
	}
	c.Token = coder.pair("assistant-again", auth.RoleOwner)
	if err := p.store.PutContact(ctx, c); err != nil {
		t.Fatal(err)
	}
	second, err := p.contactClient(ctx, "coder")
	if err != nil {
		t.Fatalf("contactClient() after a token change error = %v", err)
	}
	if second == first {
		t.Fatalf("contactClient() after a token change = the old client, want a new one")
	}

	if err := p.store.DeleteContact(ctx, "coder"); err != nil {
		t.Fatal(err)
	}
	if _, err := p.contactClient(ctx, "coder"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("contactClient() of a removed contact error = %v, want %v", err, store.ErrNotFound)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.clients) != 0 {
		t.Fatalf("clients after removing the contact = %d, want 0", len(p.clients))
	}
}

func TestContactCountsHops(t *testing.T) {
	t.Parallel()
	coder := startServer(t, serverOpts{name: "coder"})
	assistant := startServer(t, serverOpts{name: "assistant"})
	addContact(t, assistant, coder, "coder", auth.RoleOwner)
	p, ctx := assistant.srv.peers, context.Background()
	tests := []struct {
		name    string
		hops    int
		wantErr bool
	}{
		{name: "first hop", hops: 0},
		{name: "past the limit", hops: auth.MaxHops, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tg, err := p.target(ctx, "coder", auth.Owner(), tc.hops)
			if err != nil {
				t.Fatalf("target() error = %v", err)
			}
			_, err = tg.send(ctx, userText("hello", "", ""))
			if (err != nil) != tc.wantErr {
				t.Fatalf("send() at %d hops error = %v, want error %v", tc.hops, err, tc.wantErr)
			}
			if err != nil && !strings.Contains(err.Error(), "too many agents") {
				t.Fatalf("send() error = %v, want the hop limit", err)
			}
		})
	}
}

func TestAgentsURLHandles(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{})
	p := ts.srv.peers
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
	if _, err := p.claimsOf(ctx, h); !errors.Is(err, errNoTurn) {
		t.Fatalf("claimsOf() before a turn error = %v, want %v", err, errNoTurn)
	}

	turn := callClaims{ContextID: "ctx1", User: auth.Principal{ID: "pair:anna", Role: auth.RoleAuthenticated, Identity: auth.Identity{Hops: 1}}}
	end := p.beginTurn(turn)
	got, err := p.claimsOf(ctx, h)
	if err != nil {
		t.Fatalf("claimsOf() during a turn error = %v", err)
	}
	if diff := cmp.Diff(turn, got); diff != "" {
		t.Fatalf("claimsOf() wrong result (-want +got) diff = %s", diff)
	}
	end()

	if _, err := p.claimsOf(ctx, h); !errors.Is(err, errNoTurn) {
		t.Fatalf("claimsOf() after the turn error = %v, want %v", err, errNoTurn)
	}
	if _, err := p.claimsOf(ctx, "unknown"); !errors.Is(err, errUnknownHandle) {
		t.Fatalf("claimsOf(unknown) error = %v, want %v", err, errUnknownHandle)
	}
}

// turnURL returns the agents URL of a conversation with a running turn of
// the owner, which ends with the test.
func turnURL(t *testing.T, ts *testServer, contextID string) string {
	t.Helper()
	u, err := ts.srv.peers.agentsURL(context.Background(), contextID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ts.srv.peers.beginTurn(callClaims{ContextID: contextID, User: auth.Owner()}))
	return u
}

// harnessClient returns a client which reaches ts the way its harness does.
func harnessClient(ts *testServer) *http.Client {
	var d net.Dialer
	return &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return d.DialContext(ctx, "unix", ts.srv.bridge.socket)
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
	idle, err := ts.srv.peers.agentsURL(context.Background(), "c2")
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
			if err == nil && resultText(task) != tc.want {
				t.Fatalf("task result = %q, want %q", resultText(task), tc.want)
			}
		})
	}
}

func TestAgentsProxyOverTCPNeedsKey(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{name: "assistant"})
	addNode(t, ts, "grok", 0)
	hs := httptest.NewServer(ts.srv.peers.harnessRoutes("harness-key"))
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
			resp, err := http.DefaultClient.Do(req)
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
	assistant := startServer(t, serverOpts{name: "assistant", channel: newFakeChannel("fake")})
	addContact(t, assistant, coder, "coder", auth.RoleOwner)
	linkOwner(t, assistant, "telegram:1", "dm1")
	linkAs(t, assistant, "anna", auth.RoleAuthenticated, "telegram:2", "dm2")

	assistant.ch.write("telegram:2", "dm2", "call coder ask")
	ref, q := assistant.ch.waitFor(t, "dm2", hasQuestion)
	if !strings.Contains(q.Text, "coder wants to run git push") {
		t.Fatalf("question text = %q, want it to name coder and the tool", q.Text)
	}
	assistant.ch.click("telegram:2", "dm2", ref, q.Question, 0)
	assistant.ch.waitEdited(t, ref, "✓ Allow by")
}

// streamingHandler streams one working status update of a task.
type streamingHandler struct {
	a2asrv.RequestHandler
}

func (streamingHandler) SendStreamingMessage(ctx context.Context, req *a2a.SendMessageRequest) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		ev := a2a.NewStatusUpdateEvent(&a2a.Task{ID: "t1", ContextID: "remote"}, a2a.TaskStateWorking, nil)
		if !yield(ev, nil) {
			return
		}
		<-ctx.Done()
	}
}

func TestGuardedHandlerWatchesDroppedStream(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{name: "assistant"})
	addNode(t, ts, "grok", 0)
	g := &guardedHandler{RequestHandler: streamingHandler{}, p: ts.srv.peers, claims: callClaims{ContextID: "c1", User: auth.Owner()}, agent: "grok"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	for _, err := range g.SendStreamingMessage(ctx, &a2a.SendMessageRequest{Message: userText("hi", "", "")}) {
		if err != nil {
			t.Fatalf("SendStreamingMessage() error = %v", err)
		}
		cancel()
		break
	}

	ws, err := ts.srv.store.Watches(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(ws) != 1 || ws[0].TaskID != "t1" {
		t.Fatalf("store.Watches() = %+v, want a watch of task t1", ws)
	}
}

func TestWatchOfRemovedContactEnds(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{name: "assistant"})
	p, ctx := ts.srv.peers, context.Background()
	w := store.Watch{Contact: "gone", TaskID: "t1", OriginContextID: "c1", OnBehalfOf: auth.OwnerID}
	if err := p.store.PutWatch(ctx, w); err != nil {
		t.Fatal(err)
	}

	p.watch(w)

	deadline := time.Now().Add(10 * time.Second)
	for {
		ws, err := p.store.Watches(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(ws) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("store.Watches() = %+v, want none", ws)
		}
		time.Sleep(50 * time.Millisecond)
	}
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

func TestConcurrentContactClientsAreOne(t *testing.T) {
	t.Parallel()
	coder := startServer(t, serverOpts{name: "coder"})
	assistant := startServer(t, serverOpts{name: "assistant"})
	addContact(t, assistant, coder, "coder", auth.RoleOwner)
	p := assistant.srv.peers

	clients := make([]*a2aclient.Client, 8)
	var wg sync.WaitGroup
	for i := range clients {
		wg.Go(func() {
			c, err := p.contactClient(context.Background(), "coder")
			if err != nil {
				t.Errorf("contactClient() error = %v", err)
			}
			clients[i] = c
		})
	}
	wg.Wait()

	for _, c := range clients {
		if c != clients[0] {
			t.Fatalf("contactClient() built %d different clients concurrently, want one", len(slices.Compact(slices.Clone(clients))))
		}
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

// turnEvents yields the status updates of task in states, checking the
// claims of handle after each working update.
func turnEvents(t *testing.T, p *Peers, handle string, task *a2a.Task, check func(callClaims), states ...a2a.TaskState) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		for _, s := range states {
			if !yield(a2a.NewStatusUpdateEvent(task, s, nil), nil) {
				return
			}
			if s != a2a.TaskStateWorking {
				continue
			}
			c, err := p.claimsOf(context.Background(), handle)
			if err != nil {
				t.Errorf("claimsOf() during the turn error = %v", err)
				continue
			}
			check(c)
		}
	}
}

func TestAnsweredTurnKeepsItsUser(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{})
	p, ctx := ts.srv.peers, context.Background()
	h, err := p.handle(ctx, "c1")
	if err != nil {
		t.Fatal(err)
	}
	task := &a2a.Task{ID: "t1", ContextID: "c1"}
	anna := auth.Principal{ID: "pair:anna", Role: auth.RoleAuthenticated}
	wantAnna := func(c callClaims) {
		if c.User.ID != anna.ID {
			t.Errorf("claimsOf() user = %q, want %q", c.User.ID, anna.ID)
		}
	}

	prompt := &a2asrv.ExecutorContext{TaskID: task.ID, ContextID: "c1"}
	for range p.duringTurn(auth.InProcess(ctx, anna), prompt, turnEvents(t, p, h, task, wantAnna, a2a.TaskStateWorking, a2a.TaskStateInputRequired)) {
	}
	answer := &a2asrv.ExecutorContext{TaskID: task.ID, ContextID: "c1", StoredTask: &a2a.Task{ID: task.ID, ContextID: "c1", Status: a2a.TaskStatus{State: a2a.TaskStateInputRequired}}}
	for range p.duringTurn(auth.InProcess(ctx, auth.Owner()), answer, turnEvents(t, p, h, task, wantAnna, a2a.TaskStateWorking, a2a.TaskStateCompleted)) {
	}
}
