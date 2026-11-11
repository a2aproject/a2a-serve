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

package agentproxy

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"iter"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aext/structuredinput"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/google/jsonschema-go/jsonschema"

	"github.com/a2aproject/a2a-serve/internal/a2atext"
	"github.com/a2aproject/a2a-serve/internal/agentsrv/approvals"
	"github.com/a2aproject/a2a-serve/internal/agentsrv/chat"
	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/jsonhttp"
	"github.com/a2aproject/a2a-serve/internal/messenger"
	"github.com/a2aproject/a2a-serve/internal/network"
	"github.com/a2aproject/a2a-serve/internal/store"
)

// Prompts a fakeContact runs. A message to a task that waits for input is
// the answer, which completes the task.
var (
	anna = auth.Principal{ID: "pair:anna", Role: auth.RoleMember, Identity: auth.Identity{Pairing: "anna", Hops: 1}}
	bob  = auth.Principal{ID: "pair:bob", Role: auth.RoleMember}
)

// testHTTP has a transport of its own: httptest.Server.Close closes the idle
// connections of http.DefaultTransport, which fails requests that parallel
// tests send through it.
var testHTTP = &http.Client{Transport: http.DefaultTransport.(*http.Transport).Clone()}

// postOversized declares a body over jsonhttp.MaxBody to u without sending
// it and returns the status of the response.
func postOversized(t *testing.T, u, authorization string) int {
	t.Helper()
	parsed, err := url.Parse(u)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("tcp", parsed.Host)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := conn.Close(); err != nil {
			t.Errorf("closing the connection: %v", err)
		}
	}()
	if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	req := fmt.Sprintf("POST %s HTTP/1.1\r\nHost: %s\r\nAuthorization: %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n", parsed.RequestURI(), parsed.Host, authorization, jsonhttp.MaxBody+1)
	if _, err := io.WriteString(conn, req); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("POST %s error = %v, want a response", u, err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode
}

func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "server.db"))
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

// waitTimeout bounds how long tests wait for something to happen.
const waitTimeout = 10 * time.Second

const (
	promptDone     = "done"
	promptFail     = "fail"
	promptHold     = "hold"
	promptAsk      = "ask"
	promptQuestion = "question"
	promptLong     = "long"
)

const (
	fakeContactResult = "fake result"
	fakeContactAnswer = "fake answer"
)

// fakeContactLongResult is the result of promptLong: 1,000 numbered lines.
func fakeContactLongResult() string {
	var sb strings.Builder
	for i := range 1000 {
		fmt.Fprintf(&sb, "line %d of the long result\n", i+1)
	}
	return sb.String()
}

// contactRequest is an HTTP request a fakeContact received.
type contactRequest struct {
	Path          string
	Authorization string
	Hops          string
	Caller        string
}

// fakeContact is a remote agent: an A2A server that runs the prompts above.
type fakeContact struct {
	URL     string
	Handler a2asrv.RequestHandler

	release  chan struct{}
	gets     atomic.Int64
	mu       sync.Mutex
	requests []contactRequest
	messages []*a2a.Message
	// historyLengths are the history lengths the GetTask calls asked for,
	// -1 for none.
	historyLengths []int
}

func newFakeContact(t *testing.T) *fakeContact {
	t.Helper()
	fc := &fakeContact{release: make(chan struct{})}
	fc.Handler = &countingHandler{RequestHandler: a2asrv.NewHandler(fc), gets: &fc.gets, seen: fc.recordGet}
	mux := http.NewServeMux()
	var card *a2a.AgentCard
	mux.Handle("/.well-known/agent-card.json", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a2asrv.NewStaticAgentCardHandler(card).ServeHTTP(w, r)
	}))
	mux.Handle("/", a2asrv.NewJSONRPCHandler(fc.Handler))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fc.mu.Lock()
		fc.requests = append(fc.requests, contactRequest{Path: r.URL.Path, Authorization: r.Header.Get("Authorization"), Hops: r.Header.Get(auth.HeaderHops), Caller: r.Header.Get(auth.HeaderCaller)})
		fc.mu.Unlock()
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(fc.releaseHeld)
	card = &a2a.AgentCard{
		Name:                "fake",
		Version:             "1.0.0",
		SupportedInterfaces: []*a2a.AgentInterface{a2a.NewAgentInterface(srv.URL, a2a.TransportProtocolJSONRPC)},
		Capabilities:        a2a.AgentCapabilities{Streaming: true},
		DefaultInputModes:   []string{"text/plain"},
		DefaultOutputModes:  []string{"text/plain"},
	}
	fc.URL = srv.URL
	return fc
}

// releaseHeld lets the tasks that run promptHold finish.
func (fc *fakeContact) releaseHeld() {
	select {
	case <-fc.release:
	default:
		close(fc.release)
	}
}

func (fc *fakeContact) received() []contactRequest {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	return append([]contactRequest(nil), fc.requests...)
}

func (fc *fakeContact) recordGet(req *a2a.GetTaskRequest) {
	length := -1
	if req.HistoryLength != nil {
		length = *req.HistoryLength
	}
	fc.mu.Lock()
	defer fc.mu.Unlock()
	fc.historyLengths = append(fc.historyLengths, length)
}

func (fc *fakeContact) getHistoryLengths() []int {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	return append([]int(nil), fc.historyLengths...)
}

func (fc *fakeContact) receivedMessages() []*a2a.Message {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	return append([]*a2a.Message(nil), fc.messages...)
}

// startTask starts a task that runs prompt and returns it as it is when
// the call returns.
func (fc *fakeContact) startTask(t *testing.T, prompt string) *a2a.Task {
	t.Helper()
	res, err := fc.Handler.SendMessage(context.Background(), &a2a.SendMessageRequest{
		Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(prompt)),
		Config:  &a2a.SendMessageConfig{ReturnImmediately: true},
	})
	if err != nil {
		t.Fatalf("SendMessage(%q) error = %v", prompt, err)
	}
	task, ok := res.(*a2a.Task)
	if !ok {
		t.Fatalf("SendMessage(%q) = %T, want a task", prompt, res)
	}
	return task
}

// waitState waits until the task is in state.
func (fc *fakeContact) waitState(t *testing.T, id a2a.TaskID, state a2a.TaskState) *a2a.Task {
	t.Helper()
	var task *a2a.Task
	waitFor(t, "task "+string(id)+" to be "+string(state), func() bool {
		var err error
		if task, err = fc.Handler.GetTask(context.Background(), &a2a.GetTaskRequest{ID: id}); err != nil {
			t.Fatalf("GetTask() error = %v", err)
		}
		return task.Status.State == state
	})
	return task
}

func (fc *fakeContact) Execute(ctx context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		fc.mu.Lock()
		fc.messages = append(fc.messages, execCtx.Message)
		fc.mu.Unlock()
		if execCtx.StoredTask != nil && execCtx.StoredTask.Status.State == a2a.TaskStateInputRequired {
			finish(execCtx, yield, fakeContactAnswer)
			return
		}
		if !yield(a2a.NewSubmittedTask(execCtx, execCtx.Message), nil) {
			return
		}
		switch a2atext.Message(execCtx.Message) {
		case promptDone:
			if yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateWorking, nil), nil) {
				finish(execCtx, yield, fakeContactResult)
			}
		case promptFail:
			yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateFailed, a2a.NewMessageForTask(a2a.MessageRoleAgent, execCtx, a2a.NewTextPart("boom"))), nil)
		case promptHold:
			if !yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateWorking, nil), nil) {
				return
			}
			select {
			case <-fc.release:
				finish(execCtx, yield, fakeContactResult)
			case <-ctx.Done():
			}
		case promptAsk:
			ev, err := structuredinput.NewRequestEvent(execCtx, permissionRequest(), a2a.NewTextPart("Run git push?"))
			yield(ev, err)
		case promptQuestion:
			yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateInputRequired, a2a.NewMessageForTask(a2a.MessageRoleAgent, execCtx, a2a.NewTextPart("Which branch?"))), nil)
		case promptLong:
			finish(execCtx, yield, fakeContactLongResult())
		}
	}
}

func finish(execCtx *a2asrv.ExecutorContext, yield func(a2a.Event, error) bool, result string) {
	if !yield(a2a.NewArtifactEvent(execCtx, a2a.NewTextPart(result)), nil) {
		return
	}
	yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateCompleted, nil), nil)
}

func (fc *fakeContact) Cancel(_ context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateCanceled, nil), nil)
	}
}

func permissionRequest() *structuredinput.Request {
	optionSchema := &jsonschema.Schema{Type: "string"}
	for _, o := range [][2]string{{"once", "Allow"}, {"reject", "Deny"}} {
		optionSchema.OneOf = append(optionSchema.OneOf, &jsonschema.Schema{Const: jsonschema.Ptr[any](o[0]), Title: o[1]})
	}
	return &structuredinput.Request{ID: "tool1", Schema: &jsonschema.Schema{
		Type: "object", Title: "git push", Properties: map[string]*jsonschema.Schema{"option": optionSchema},
	}}
}

// permissionStatus is the input-required status of a task that asks for
// permission to run a tool.
func permissionStatus(t *testing.T) a2a.TaskStatus {
	t.Helper()
	ev, err := structuredinput.NewRequestEvent(&a2a.Task{ID: "t9", ContextID: "r9"}, permissionRequest(), a2a.NewTextPart("Run git push?"))
	if err != nil {
		t.Fatalf("structuredinput.NewRequestEvent() error = %v", err)
	}
	return ev.Status
}

// countingHandler counts the GetTask calls it gets.
type countingHandler struct {
	a2asrv.RequestHandler
	gets *atomic.Int64
	seen func(*a2a.GetTaskRequest)
}

func (h *countingHandler) GetTask(ctx context.Context, req *a2a.GetTaskRequest) (*a2a.Task, error) {
	h.gets.Add(1)
	if h.seen != nil {
		h.seen(req)
	}
	return h.RequestHandler.GetTask(ctx, req)
}

// waitFor polls cond until it holds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(waitTimeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// staticPeers is a private network that lists fixed peers.
type staticPeers struct {
	network.Provider
	peers []network.PeerInfo
}

func (p *staticPeers) Peers(context.Context) ([]network.PeerInfo, error) { return p.peers, nil }

func (p *staticPeers) HTTPClient() *http.Client { return testHTTP }

type askedQuestion struct {
	To     approvals.Chat
	Header string
	Rec    store.Question
}

// fakeQuestionChat records the questions shown, and has a configurable
// owner chat.
type fakeQuestionChat struct {
	owner  approvals.Chat
	hasOwn bool

	mu    sync.Mutex
	asked []askedQuestion
}

func (c *fakeQuestionChat) Ask(_ context.Context, to approvals.Chat, header string, rec store.Question) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.asked = append(c.asked, askedQuestion{To: to, Header: header, Rec: rec})
}

func (c *fakeQuestionChat) OwnerChat(context.Context) (approvals.Chat, bool) {
	return c.owner, c.hasOwn
}

func (c *fakeQuestionChat) questions() []askedQuestion {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]askedQuestion(nil), c.asked...)
}

// fakeQuestionTasks records the questions asked on tasks of their own and
// the tasks ended.
type fakeQuestionTasks struct {
	store *store.Store

	mu    sync.Mutex
	asked []taskQuestion
	ended []endedQuestions
	// open are the question tasks that exist from before the server started.
	open []endedQuestions
}

type taskQuestion struct {
	ContextID string
	Header    string
	Rec       store.Question
	// Watched is whether the watch of the question's task existed when it was asked.
	Watched bool
}

type endedQuestions struct {
	ContextID, Agent, TaskID string
}

func (f *fakeQuestionTasks) Ask(ctx context.Context, contextID string, q *CallerQuestion) error {
	_, err := f.store.Watch(ctx, q.Rec.Contact, q.Rec.TaskID)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, taskQuestion{ContextID: contextID, Header: q.Header, Rec: q.Rec, Watched: err == nil})
	return nil
}

func (f *fakeQuestionTasks) End(_ context.Context, contextID, agent, taskID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ended = append(f.ended, endedQuestions{ContextID: contextID, Agent: agent, TaskID: taskID})
}

func (f *fakeQuestionTasks) EndOrphaned(_ context.Context, watched func(agent, taskID string) bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, q := range f.open {
		if !watched(q.Agent, q.TaskID) {
			f.ended = append(f.ended, q)
		}
	}
}

func (f *fakeQuestionTasks) questions() []taskQuestion {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]taskQuestion(nil), f.asked...)
}

func (f *fakeQuestionTasks) ends() []endedQuestions {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]endedQuestions(nil), f.ended...)
}

type injectedTurn struct {
	ContextID string
	User      auth.Principal
	Text      string
	Files     []messenger.File
	Quiet     bool
}

// fakeTurnInjector records the turns injected. The agent accepts them at
// once, or, with hold set, when the test calls their accept function.
type fakeTurnInjector struct {
	turns    chan injectedTurn
	accepted chan func(error)
	hold     bool

	mu  sync.Mutex
	err error
}

func (f *fakeTurnInjector) setErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

func newFakeTurnInjector() *fakeTurnInjector {
	return &fakeTurnInjector{turns: make(chan injectedTurn, 16), accepted: make(chan func(error), 16)}
}

func (f *fakeTurnInjector) InjectTurn(_ context.Context, t chat.PostedTurn) error {
	f.mu.Lock()
	err := f.err
	f.mu.Unlock()
	accept := t.Accepted
	if accept == nil {
		accept = func(error) {}
	}
	switch {
	case err != nil:
	case f.hold:
		f.accepted <- accept
	default:
		accept(nil)
	}
	f.turns <- injectedTurn{ContextID: t.ContextID, User: t.User, Text: t.Text, Files: t.Files, Quiet: t.Quiet}
	return err
}

// statusContact is a remote agent that serves its card and answers every
// other request with status.
type statusContact struct {
	URL      string
	requests atomic.Int64
}

func newStatusContact(t *testing.T, status int) *statusContact {
	t.Helper()
	sc := &statusContact{}
	var card http.Handler
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sc.requests.Add(1)
		if r.URL.Path == "/.well-known/agent-card.json" {
			card.ServeHTTP(w, r)
			return
		}
		http.Error(w, http.StatusText(status), status)
	}))
	t.Cleanup(srv.Close)
	card = a2asrv.NewStaticAgentCardHandler(&a2a.AgentCard{
		Name:                "status",
		Version:             "1.0.0",
		SupportedInterfaces: []*a2a.AgentInterface{a2a.NewAgentInterface(srv.URL, a2a.TransportProtocolJSONRPC)},
		DefaultInputModes:   []string{"text/plain"},
		DefaultOutputModes:  []string{"text/plain"},
	})
	sc.URL = srv.URL
	return sc
}
