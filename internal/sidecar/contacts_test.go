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
	"errors"
	"iter"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/a2aproject/a2a-go/v2/a2aext/structuredinput"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/google/go-cmp/cmp"
	"github.com/google/jsonschema-go/jsonschema"

	"github.com/a2aproject/a2a-serve/core/contact"
	"github.com/a2aproject/a2a-serve/internal/sidecar/serve"
	"github.com/a2aproject/a2a-serve/internal/testutil"
)

const contactSecret = "Bearer s3cret-contact-token"

// contactRequest is what a fakeContact received of a request.
type contactRequest struct {
	Authorization string
	Hops          string
}

// fakeContact is an A2A agent that a2a-serve-sidecar calls for the agent. It runs
// the prompts done, hold and ask, and finishes a task that asked once it
// gets the answer.
type fakeContact struct {
	url string

	mu       sync.Mutex
	requests []contactRequest
	// metadata are the metadata of the requests that sent messages or
	// canceled tasks.
	metadata []map[string]any
	held     []a2a.TaskID
	canceled []a2a.TaskID
	handler  a2asrv.RequestHandler
}

func newFakeContact() *fakeContact {
	fc := &fakeContact{}
	fc.handler = &recordingHandler{RequestHandler: a2asrv.NewHandler(fc), fc: fc}
	return fc
}

func (fc *fakeContact) serveHTTP(cardURL string) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/.well-known/agent-card.json", a2asrv.NewStaticAgentCardHandler(&a2a.AgentCard{
		Name:                "fake",
		Description:         "Reviews code.",
		Version:             "1.0.0",
		SupportedInterfaces: []*a2a.AgentInterface{a2a.NewAgentInterface(cardURL, a2a.TransportProtocolJSONRPC)},
		Capabilities:        a2a.AgentCapabilities{Streaming: true},
		DefaultInputModes:   []string{"text/plain"},
		DefaultOutputModes:  []string{"text/plain"},
		Skills:              []a2a.AgentSkill{{ID: "review", Name: "Review", Description: "Reviews a diff.", Tags: []string{}}},
	}))
	mux.Handle("/", a2asrv.NewJSONRPCHandler(fc.handler))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fc.mu.Lock()
		fc.requests = append(fc.requests, contactRequest{Authorization: r.Header.Get("Authorization"), Hops: r.Header.Get(contact.HeaderHops)})
		fc.mu.Unlock()
		mux.ServeHTTP(w, r)
	})
}

// startTLSContact serves a fake contact over https on a local port.
func startTLSContact(t *testing.T) (*fakeContact, *httptest.Server) {
	t.Helper()
	fc := newFakeContact()
	srv := httptest.NewUnstartedServer(nil)
	srv.StartTLS()
	srv.Config.Handler = fc.serveHTTP(srv.URL)
	t.Cleanup(srv.Close)
	fc.url = srv.URL
	return fc, srv
}

// startUnixContact serves a fake contact on a Unix socket.
func startUnixContact(t *testing.T) (*fakeContact, string) {
	t.Helper()
	fc := newFakeContact()
	sock := filepath.Join(testutil.ShortTempDir(t), "contact.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: fc.serveHTTP("http://contact"), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("http.Server.Serve() error = %v", err)
		}
	}()
	t.Cleanup(func() {
		if err := srv.Close(); err != nil {
			t.Errorf("http.Server.Close() error = %v", err)
		}
	})
	return fc, "unix://" + sock
}

func (fc *fakeContact) received() ([]contactRequest, []map[string]any) {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	return slices.Clone(fc.requests), slices.Clone(fc.metadata)
}

func (fc *fakeContact) heldTasks() []a2a.TaskID {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	return slices.Clone(fc.held)
}

func (fc *fakeContact) canceledTasks() []a2a.TaskID {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	return slices.Clone(fc.canceled)
}

func (fc *fakeContact) Execute(ctx context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		if execCtx.StoredTask != nil && execCtx.StoredTask.Status.State == a2a.TaskStateInputRequired {
			fc.answered(ctx, execCtx, yield)
			return
		}
		if !yield(a2a.NewSubmittedTask(execCtx, execCtx.Message), nil) {
			return
		}
		switch messageText(execCtx.Message) {
		case "done":
			finishContact(execCtx, yield, "fake result")
		case "hold":
			fc.mu.Lock()
			fc.held = append(fc.held, execCtx.TaskID)
			fc.mu.Unlock()
			if yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateWorking, nil), nil) {
				<-ctx.Done()
			}
		case "ask", "askhold":
			ev, err := structuredinput.NewRequestEvent(execCtx, permissionRequest(), a2a.NewTextPart("Run git push?"))
			yield(ev, err)
		}
	}
}

// answered runs the task once its approval is answered: after ask, it
// sends its result a while later without a status in between, and after
// askhold it works until canceled.
func (fc *fakeContact) answered(ctx context.Context, execCtx *a2asrv.ExecutorContext, yield func(a2a.Event, error) bool) {
	if len(execCtx.StoredTask.History) > 0 && messageText(execCtx.StoredTask.History[0]) == "askhold" {
		if !yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateWorking, nil), nil) {
			return
		}
		fc.mu.Lock()
		fc.held = append(fc.held, execCtx.TaskID)
		fc.mu.Unlock()
		<-ctx.Done()
		return
	}
	select {
	case <-ctx.Done():
		return
	case <-time.After(300 * time.Millisecond):
	}
	finishContact(execCtx, yield, "fake answer")
}

func finishContact(execCtx *a2asrv.ExecutorContext, yield func(a2a.Event, error) bool, result string) {
	if yield(a2a.NewArtifactEvent(execCtx, a2a.NewTextPart(result)), nil) {
		yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateCompleted, nil), nil)
	}
}

func (fc *fakeContact) Cancel(_ context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		fc.mu.Lock()
		fc.canceled = append(fc.canceled, execCtx.TaskID)
		fc.mu.Unlock()
		yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateCanceled, nil), nil)
	}
}

func permissionRequest() *structuredinput.Request {
	options := &jsonschema.Schema{Type: "string"}
	for _, o := range [][2]string{{"once", "Allow"}, {"reject", "Deny"}} {
		options.OneOf = append(options.OneOf, &jsonschema.Schema{Const: jsonschema.Ptr[any](o[0]), Title: o[1]})
	}
	return &structuredinput.Request{ID: "tool1", Schema: &jsonschema.Schema{
		Type: "object", Title: "git push", Properties: map[string]*jsonschema.Schema{"option": options},
	}}
}

// recordingHandler records the metadata of the requests that send
// messages and cancel tasks.
type recordingHandler struct {
	a2asrv.RequestHandler
	fc *fakeContact
}

func (h *recordingHandler) note(m map[string]any) {
	h.fc.mu.Lock()
	defer h.fc.mu.Unlock()
	h.fc.metadata = append(h.fc.metadata, m)
}

// SendMessage answers a reply to a task that asks to return at once with
// the task as it is, before the reply is processed, as a server may.
func (h *recordingHandler) SendMessage(ctx context.Context, req *a2a.SendMessageRequest) (a2a.SendMessageResult, error) {
	h.note(req.Metadata)
	if req.Config == nil || !req.Config.ReturnImmediately || req.Message.TaskID == "" {
		return h.RequestHandler.SendMessage(ctx, req)
	}
	task, err := h.RequestHandler.GetTask(ctx, &a2a.GetTaskRequest{ID: req.Message.TaskID})
	if err != nil {
		return nil, err
	}
	go func() {
		for _, err := range h.RequestHandler.SendStreamingMessage(context.WithoutCancel(ctx), req) {
			if err != nil {
				return
			}
		}
	}()
	return task, nil
}

func (h *recordingHandler) SendStreamingMessage(ctx context.Context, req *a2a.SendMessageRequest) iter.Seq2[a2a.Event, error] {
	h.note(req.Metadata)
	return h.RequestHandler.SendStreamingMessage(ctx, req)
}

func (h *recordingHandler) CancelTask(ctx context.Context, req *a2a.CancelTaskRequest) (*a2a.Task, error) {
	h.note(req.Metadata)
	return h.RequestHandler.CancelTask(ctx, req)
}

// contactsServer is a2a-serve-sidecar with the contacts reviewer, over https with
// auth, and planner, on a socket, and its A2A client.
type contactsServer struct {
	srv      *serve.Server
	client   *a2aclient.Client
	stderr   *syncBuffer
	reviewer *fakeContact
	planner  *fakeContact
	once     sync.Once
	closeErr error
}

// close closes the server once.
func (cs *contactsServer) close(ctx context.Context) error {
	cs.once.Do(func() { cs.closeErr = cs.srv.Close(ctx) })
	return cs.closeErr
}

func startWithContacts(t *testing.T, extra ...string) *contactsServer {
	t.Helper()
	reviewer, tlsSrv := startTLSContact(t)
	planner, plannerURL := startUnixContact(t)
	sock := filepath.Join(testutil.ShortTempDir(t), "a2a.sock")
	cs := &contactsServer{stderr: &syncBuffer{}, reviewer: reviewer, planner: planner}
	srv, err := serve.Start(context.Background(), serve.Config{
		Command:          fakeACP,
		Sandbox:          serve.SandboxLocal,
		Workspace:        "dir:" + t.TempDir(),
		Listen:           "unix://" + sock,
		Contacts:         append([]string{"reviewer=" + reviewer.url, "planner=" + plannerURL}, extra...),
		ContactAuth:      []string{"reviewer=Bearer env:REVIEWER_TOKEN"},
		ContactTransport: tlsSrv.Client().Transport,
		Getenv: func(name string) string {
			return map[string]string{"REVIEWER_TOKEN": strings.TrimPrefix(contactSecret, "Bearer ")}[name]
		},
		Stderr:  cs.stderr,
		Version: "test",
	})
	if err != nil {
		t.Fatalf("serve.Start() error = %v", err)
	}
	cs.srv = srv
	t.Cleanup(func() {
		if err := cs.close(context.Background()); err != nil {
			t.Errorf("Server.Close() error = %v", err)
		}
		if strings.Contains(cs.stderr.String(), contactSecret) {
			t.Errorf("stderr = %q, want it without the contact's auth", cs.stderr)
		}
	})
	cs.client, err = a2aclient.NewFromEndpoints(context.Background(), []*a2a.AgentInterface{a2a.NewAgentInterface("http://a2a/", a2a.TransportProtocolJSONRPC)}, a2aclient.WithJSONRPCTransport(unixClient(sock)))
	if err != nil {
		t.Fatalf("a2aclient.NewFromEndpoints() error = %v", err)
	}
	return cs
}

// sendTask sends text in contextID and returns the task it ends in.
func (cs *contactsServer) sendTask(ctx context.Context, t *testing.T, contextID, taskID, text string) *a2a.Task {
	t.Helper()
	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(text))
	msg.ContextID, msg.TaskID = contextID, a2a.TaskID(taskID)
	res, err := cs.client.SendMessage(ctx, &a2a.SendMessageRequest{Message: msg})
	if err != nil {
		t.Fatalf("Client.SendMessage(%q) error = %v", text, err)
	}
	task, ok := res.(*a2a.Task)
	if !ok {
		t.Fatalf("Client.SendMessage(%q) = %T, want a task", text, res)
	}
	return task
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestContactsCallsCarryAuthAndContext(t *testing.T) {
	t.Parallel()
	missing := "unix://" + filepath.Join(testutil.ShortTempDir(t), "missing.sock")
	cs := startWithContacts(t, "broken="+missing)

	ready := cs.srv.Ready().Contacts
	if len(ready) != 3 || ready[2].Card != serve.ContactCardFailed || ready[2].Reason == "" {
		t.Fatalf("Server.Ready().Contacts = %+v, want broken failed with a reason", ready)
	}
	if diff := cmp.Diff([]serve.ContactStatus{{Name: "reviewer", Card: serve.ContactCardOK}, {Name: "planner", Card: serve.ContactCardOK}}, ready[:2]); diff != "" {
		t.Fatalf("Server.Ready().Contacts wrong result (-want +got) diff = %s", diff)
	}
	ctx := context.Background()
	reviewed := testutil.AllArtifactText(cs.sendTask(ctx, t, "ctx-a", "", "call reviewer done"))
	planned := testutil.AllArtifactText(cs.sendTask(ctx, t, "ctx-a", "", "call planner done"))
	broken := testutil.AllArtifactText(cs.sendTask(ctx, t, "ctx-a", "", "call broken done"))
	environ := testutil.AllArtifactText(cs.sendTask(ctx, t, "ctx-a", "", "environ"))

	for name, got := range map[string]string{"reviewer": reviewed, "planner": planned} {
		if !strings.Contains(got, "COMPLETED: fake result") {
			t.Fatalf("call %s done = %q, want the contact's result", name, got)
		}
	}
	if !strings.Contains(broken, "tool error") {
		t.Fatalf("call broken done = %q, want a tool error", broken)
	}
	if strings.Contains(environ, strings.TrimPrefix(contactSecret, "Bearer ")) {
		t.Fatalf("the agent's environment = %q, want it without the contact's auth", environ)
	}
	reqs, meta := cs.reviewer.received()
	if !slices.ContainsFunc(reqs, func(r contactRequest) bool { return r.Authorization == contactSecret && r.Hops == "1" }) {
		t.Fatalf("reviewer requests = %+v, want the auth and one hop", reqs)
	}
	if len(meta) == 0 || meta[0][serve.ContextMetadataKey] != "ctx-a" {
		t.Fatalf("reviewer request metadata = %v, want %s ctx-a", meta, serve.ContextMetadataKey)
	}
	reqs, meta = cs.planner.received()
	if slices.ContainsFunc(reqs, func(r contactRequest) bool { return r.Authorization != "" }) {
		t.Fatalf("planner requests = %+v, want no auth", reqs)
	}
	if len(meta) == 0 || meta[0][serve.ContextMetadataKey] != "ctx-a" {
		t.Fatalf("planner request metadata = %v, want %s ctx-a", meta, serve.ContextMetadataKey)
	}
}

func TestContactsRefuseCallsPastTheHopLimit(t *testing.T) {
	t.Parallel()
	cs := startWithContacts(t)
	ctx := a2aclient.AttachServiceParams(context.Background(), a2aclient.ServiceParams{contact.HeaderHops: {"3"}})

	got := testutil.AllArtifactText(cs.sendTask(ctx, t, "ctx-hops", "", "call reviewer done"))

	if !strings.Contains(got, "too many agents in this chain") {
		t.Fatalf("call reviewer done with 3 hops = %q, want the hop limit refusal", got)
	}
	if reqs, _ := cs.reviewer.received(); slices.ContainsFunc(reqs, func(r contactRequest) bool { return r.Hops != "" }) {
		t.Fatalf("reviewer requests = %+v, want no call", reqs)
	}
}

func TestContactsCancelTheTasksOfACanceledTask(t *testing.T) {
	t.Parallel()
	cs := startWithContacts(t)
	ctx := context.Background()
	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("call reviewer hold"))
	msg.ContextID = "ctx-cancel"
	res, err := cs.client.SendMessage(ctx, &a2a.SendMessageRequest{Message: msg, Config: &a2a.SendMessageConfig{ReturnImmediately: true}})
	if err != nil {
		t.Fatalf("Client.SendMessage() error = %v", err)
	}
	outer, ok := res.(*a2a.Task)
	if !ok {
		t.Fatalf("Client.SendMessage() = %T, want a task", res)
	}
	waitUntil(t, "the reviewer to hold a task", func() bool { return len(cs.reviewer.heldTasks()) == 1 })
	// a2a-serve-sidecar records the reviewer's task once its first event arrives,
	// which the test can't observe.
	time.Sleep(500 * time.Millisecond)

	if _, err := cs.client.CancelTask(ctx, &a2a.CancelTaskRequest{ID: outer.ID}); err != nil {
		t.Fatalf("Client.CancelTask() error = %v", err)
	}

	waitUntil(t, "the reviewer's task to be canceled", func() bool { return slices.Equal(cs.reviewer.canceledTasks(), cs.reviewer.heldTasks()) })
	_, meta := cs.reviewer.received()
	if got := meta[len(meta)-1][serve.ContextMetadataKey]; got != "ctx-cancel" {
		t.Fatalf("reviewer cancel metadata %s = %v, want ctx-cancel", serve.ContextMetadataKey, got)
	}
}

func TestContactsRelayApprovalsToTheHost(t *testing.T) {
	t.Parallel()
	cs := startWithContacts(t)
	ctx := context.Background()

	asked := cs.sendTask(ctx, t, "ctx-ask", "", "call reviewer ask")

	if asked.Status.State != a2a.TaskStateInputRequired {
		t.Fatalf("call reviewer ask state = %v, want %v", asked.Status.State, a2a.TaskStateInputRequired)
	}
	text := messageText(asked.Status.Message)
	if !strings.Contains(text, "reviewer, an agent that this task called, asks for an approval") || !strings.Contains(text, "Run git push?") {
		t.Fatalf("call reviewer ask status = %q, want the reviewer's question", text)
	}
	req, err := structuredinput.ParseRequest(asked.Status.Message)
	if err != nil || req == nil || req.ID != "tool1" {
		t.Fatalf("structuredinput.ParseRequest() = %v, %v, want the reviewer's request", req, err)
	}

	again := cs.sendTask(ctx, t, "ctx-ask", string(asked.ID), "maybe")
	if again.Status.State != a2a.TaskStateInputRequired || !strings.Contains(messageText(again.Status.Message), "I couldn't use the reply") {
		t.Fatalf("reply maybe = %v %q, want the question again", again.Status.State, messageText(again.Status.Message))
	}
	answered := cs.sendTask(ctx, t, "ctx-ask", string(asked.ID), "once")

	if answered.Status.State != a2a.TaskStateCompleted {
		t.Fatalf("reply once state = %v, want %v", answered.Status.State, a2a.TaskStateCompleted)
	}
	_, meta := cs.reviewer.received()
	if len(meta) < 2 || meta[len(meta)-1][serve.ContextMetadataKey] != "ctx-ask" {
		t.Fatalf("reviewer request metadata = %v, want the answer with %s ctx-ask", meta, serve.ContextMetadataKey)
	}
	ref, _ := asked.Status.Message.Metadata[serve.ApprovalMetadataKey].(map[string]any)
	id, _ := ref["task"].(string)
	task, err := cs.reviewer.handler.GetTask(ctx, &a2a.GetTaskRequest{ID: a2a.TaskID(id)})
	if err != nil {
		t.Fatalf("reviewer GetTask(%q) error = %v", id, err)
	}
	if task.Status.State != a2a.TaskStateCompleted || testutil.AllArtifactText(task) != "fake answer" {
		t.Fatalf("reviewer task = %v %q, want completed with the fake answer", task.Status.State, testutil.AllArtifactText(task))
	}
}

func TestContactsFailATaskWaitingForAContactWhenTheServerStops(t *testing.T) {
	t.Parallel()
	cs := startWithContacts(t)
	ctx := context.Background()
	asked := cs.sendTask(ctx, t, "ctx-stop", "", "call reviewer askhold")
	if asked.Status.State != a2a.TaskStateInputRequired {
		t.Fatalf("call reviewer askhold state = %v, want %v", asked.Status.State, a2a.TaskStateInputRequired)
	}
	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("once"))
	msg.ContextID, msg.TaskID = "ctx-stop", asked.ID
	last := make(chan a2a.Event, 1)
	go func() {
		var ev a2a.Event
		for e, err := range cs.client.SendStreamingMessage(ctx, &a2a.SendMessageRequest{Message: msg}) {
			if err != nil {
				break
			}
			ev = e
		}
		last <- ev
	}()
	waitUntil(t, "the reviewer to work on the answer", func() bool { return len(cs.reviewer.heldTasks()) == 1 })

	closed := make(chan error, 1)
	go func() { closed <- cs.close(ctx) }()

	var ev a2a.Event
	select {
	case ev = <-last:
	case <-time.After(30 * time.Second):
		t.Fatal("the answer still waits for the contact 30s after Server.Close() started")
	}
	if err := <-closed; err != nil {
		t.Fatalf("Server.Close() error = %v", err)
	}
	up, ok := ev.(*a2a.TaskStatusUpdateEvent)
	if !ok || up.Status.State != a2a.TaskStateFailed || !strings.Contains(messageText(up.Status.Message), "The server stopped") {
		t.Fatalf("last event of the answer = %#v, want the task failed as the server stopped", ev)
	}
	waitUntil(t, "the reviewer's task to be canceled", func() bool { return slices.Equal(cs.reviewer.canceledTasks(), cs.reviewer.heldTasks()) })
}

func messageText(m *a2a.Message) string {
	if m == nil {
		return ""
	}
	var out []string
	for _, p := range m.Parts {
		out = append(out, p.Text())
	}
	return strings.Join(out, "")
}
