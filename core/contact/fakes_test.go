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

package contact

import (
	"context"
	"iter"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/a2aproject/a2a-go/v2/a2aclient/agentcard"
	"github.com/a2aproject/a2a-go/v2/a2aext/structuredinput"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/google/jsonschema-go/jsonschema"

	"github.com/a2aproject/a2a-serve/internal/a2atext"
)

// Prompts a fakeContact runs. A message to a task that waits for input is
// the answer, which completes the task.
const (
	promptDone = "done"
	promptHold = "hold"
	promptAsk  = "ask"
)

const (
	fakeResult = "fake result"
	fakeAnswer = "fake answer"
)

// waitTimeout bounds how long tests wait for something to happen.
const waitTimeout = 10 * time.Second

// contactRequest is an HTTP request a fakeContact received.
type contactRequest struct {
	Path          string
	Authorization string
	Hops          string
	Header        string
}

// fakeContact is a remote agent: an A2A server that runs the prompts above.
type fakeContact struct {
	URL     string
	Card    *a2a.AgentCard
	Handler a2asrv.RequestHandler
	srv     *httptest.Server

	mu       sync.Mutex
	requests []contactRequest
	// metadata are the metadata of the requests that sent messages.
	metadata []map[string]any
}

// newFakeContact serves a fake contact on srv, which isn't started yet.
func newFakeContact(t *testing.T, srv *httptest.Server, card *a2a.AgentCard) *fakeContact {
	t.Helper()
	fc := &fakeContact{srv: srv}
	fc.Handler = &metadataRecorder{RequestHandler: a2asrv.NewHandler(fc), fc: fc}
	mux := http.NewServeMux()
	cardHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fc.mu.Lock()
		c := fc.Card
		fc.mu.Unlock()
		if c == nil {
			http.Error(w, "no card", http.StatusServiceUnavailable)
			return
		}
		a2asrv.NewStaticAgentCardHandler(c).ServeHTTP(w, r)
	})
	mux.Handle("/.well-known/agent-card.json", cardHandler)
	mux.Handle("/card.json", cardHandler)
	mux.Handle("/", a2asrv.NewJSONRPCHandler(fc.Handler))
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fc.mu.Lock()
		fc.requests = append(fc.requests, contactRequest{Path: r.URL.Path, Authorization: r.Header.Get("Authorization"), Hops: r.Header.Get(HeaderHops), Header: r.Header.Get("X-Test")})
		fc.mu.Unlock()
		mux.ServeHTTP(w, r)
	})
	fc.Card = card
	return fc
}

// startFakeContact starts a fake contact on a local TCP port.
func startFakeContact(t *testing.T) *fakeContact {
	t.Helper()
	srv := httptest.NewUnstartedServer(nil)
	fc := newFakeContact(t, srv, nil)
	srv.Start()
	t.Cleanup(srv.Close)
	fc.URL = srv.URL
	fc.setCard(fakeCard(srv.URL))
	return fc
}

func fakeCard(u string) *a2a.AgentCard {
	return &a2a.AgentCard{
		Name:                "fake",
		Description:         "Reviews code.",
		Version:             "1.0.0",
		SupportedInterfaces: []*a2a.AgentInterface{a2a.NewAgentInterface(u, a2a.TransportProtocolJSONRPC)},
		Capabilities:        a2a.AgentCapabilities{Streaming: true},
		DefaultInputModes:   []string{"text/plain"},
		DefaultOutputModes:  []string{"text/plain"},
		Skills:              []a2a.AgentSkill{{ID: "review", Name: "Review", Description: "Reviews a diff.", Tags: []string{}}},
	}
}

func (fc *fakeContact) setCard(c *a2a.AgentCard) {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	fc.Card = c
}

func (fc *fakeContact) received() []contactRequest {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	return append([]contactRequest(nil), fc.requests...)
}

func (fc *fakeContact) sentMetadata() []map[string]any {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	return append([]map[string]any(nil), fc.metadata...)
}

// client returns an A2A client of the contact.
func (fc *fakeContact) client(t *testing.T) *a2aclient.Client {
	t.Helper()
	ctx := context.Background()
	c, err := a2aclient.NewFromCard(ctx, fakeCard(fc.URL), a2aclient.WithJSONRPCTransport(fc.srv.Client()))
	if err != nil {
		t.Fatalf("a2aclient.NewFromCard() error = %v", err)
	}
	return c
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
		if execCtx.StoredTask != nil && execCtx.StoredTask.Status.State == a2a.TaskStateInputRequired {
			finish(execCtx, yield, fakeAnswer)
			return
		}
		if !yield(a2a.NewSubmittedTask(execCtx, execCtx.Message), nil) {
			return
		}
		switch a2atext.Message(execCtx.Message) {
		case promptDone:
			if yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateWorking, nil), nil) {
				finish(execCtx, yield, fakeResult)
			}
		case promptHold:
			if !yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateWorking, nil), nil) {
				return
			}
			<-ctx.Done()
		case promptAsk:
			ev, err := structuredinput.NewRequestEvent(execCtx, permissionRequest(), a2a.NewTextPart("Run git push?"))
			yield(ev, err)
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

// metadataRecorder records the metadata of the requests that send
// messages and cancel tasks.
type metadataRecorder struct {
	a2asrv.RequestHandler
	fc *fakeContact
}

func (h *metadataRecorder) note(m map[string]any) {
	h.fc.mu.Lock()
	defer h.fc.mu.Unlock()
	h.fc.metadata = append(h.fc.metadata, m)
}

func (h *metadataRecorder) SendMessage(ctx context.Context, req *a2a.SendMessageRequest) (a2a.SendMessageResult, error) {
	h.note(req.Metadata)
	return h.RequestHandler.SendMessage(ctx, req)
}

func (h *metadataRecorder) SendStreamingMessage(ctx context.Context, req *a2a.SendMessageRequest) iter.Seq2[a2a.Event, error] {
	h.note(req.Metadata)
	return h.RequestHandler.SendStreamingMessage(ctx, req)
}

func (h *metadataRecorder) CancelTask(ctx context.Context, req *a2a.CancelTaskRequest) (*a2a.Task, error) {
	h.note(req.Metadata)
	return h.RequestHandler.CancelTask(ctx, req)
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

// proxyClient returns a client of the agent at agentURL, as the agent's
// a2a CLI calls it, preferring transport.
func proxyClient(t *testing.T, hc *http.Client, agentURL string, transport a2a.TransportProtocol) *a2aclient.Client {
	t.Helper()
	ctx := context.Background()
	card, err := agentcard.NewResolver(hc).Resolve(ctx, agentURL)
	if err != nil {
		t.Fatalf("Resolve(%s) error = %v", agentURL, err)
	}
	client, err := a2aclient.NewFromCard(ctx, card, a2aclient.WithJSONRPCTransport(hc), a2aclient.WithRESTTransport(hc), a2aclient.WithConfig(a2aclient.Config{PreferredTransports: []a2a.TransportProtocol{transport}}))
	if err != nil {
		t.Fatalf("NewFromCard() error = %v", err)
	}
	t.Cleanup(func() {
		if err := client.Destroy(); err != nil {
			t.Errorf("Destroy() error = %v", err)
		}
	})
	return client
}

// recordingAgent stands in for the contact behind a guard, and records
// which calls get through to it.
type recordingAgent struct {
	a2asrv.RequestHandler

	mu      sync.Mutex
	reached []string
	// task is what the agent returns for a task, and sends back.
	task *a2a.Task
	// listed are the tasks it lists.
	listed []*a2a.Task
	// historyLengths are the history lengths GetTask was asked for, -1 for none.
	historyLengths []int
}

func (a *recordingAgent) reach(method string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.reached = append(a.reached, method)
}

func (a *recordingAgent) calls() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.reached...)
}

func (a *recordingAgent) SendMessage(context.Context, *a2a.SendMessageRequest) (a2a.SendMessageResult, error) {
	a.reach("SendMessage")
	return a.task, nil
}

func (a *recordingAgent) SendStreamingMessage(context.Context, *a2a.SendMessageRequest) iter.Seq2[a2a.Event, error] {
	a.reach("SendStreamingMessage")
	return func(yield func(a2a.Event, error) bool) {
		if a.task != nil {
			yield(a.task, nil)
		}
	}
}

// GetTask returns the listed task with the ID, or else a.task.
func (a *recordingAgent) GetTask(_ context.Context, req *a2a.GetTaskRequest) (*a2a.Task, error) {
	a.reach("GetTask")
	length := -1
	if req.HistoryLength != nil {
		length = *req.HistoryLength
	}
	a.mu.Lock()
	a.historyLengths = append(a.historyLengths, length)
	a.mu.Unlock()
	for _, t := range a.listed {
		if t.ID == req.ID {
			return t, nil
		}
	}
	if a.task == nil {
		return nil, a2a.ErrTaskNotFound
	}
	return a.task, nil
}

func (a *recordingAgent) ListTasks(context.Context, *a2a.ListTasksRequest) (*a2a.ListTasksResponse, error) {
	a.reach("ListTasks")
	return &a2a.ListTasksResponse{Tasks: a.listed, TotalSize: len(a.listed), PageSize: len(a.listed), NextPageToken: strconv.Itoa(len(a.listed))}, nil
}

func (a *recordingAgent) CancelTask(context.Context, *a2a.CancelTaskRequest) (*a2a.Task, error) {
	a.reach("CancelTask")
	return a.task, nil
}

func (a *recordingAgent) SubscribeToTask(context.Context, *a2a.SubscribeToTaskRequest) iter.Seq2[a2a.Event, error] {
	a.reach("SubscribeToTask")
	return func(yield func(a2a.Event, error) bool) {}
}

func (a *recordingAgent) GetTaskPushConfig(context.Context, *a2a.GetTaskPushConfigRequest) (*a2a.PushConfig, error) {
	a.reach("GetTaskPushConfig")
	return &a2a.PushConfig{}, nil
}

func (a *recordingAgent) ListTaskPushConfigs(context.Context, *a2a.ListTaskPushConfigRequest) (*a2a.ListTaskPushConfigResponse, error) {
	a.reach("ListTaskPushConfigs")
	return &a2a.ListTaskPushConfigResponse{}, nil
}

func (a *recordingAgent) CreateTaskPushConfig(_ context.Context, req *a2a.PushConfig) (*a2a.PushConfig, error) {
	a.reach("CreateTaskPushConfig")
	return req, nil
}

func (a *recordingAgent) DeleteTaskPushConfig(context.Context, *a2a.DeleteTaskPushConfigRequest) error {
	a.reach("DeleteTaskPushConfig")
	return nil
}

func completedTask(id a2a.TaskID, contextID string) *a2a.Task {
	return &a2a.Task{ID: id, ContextID: contextID, Status: a2a.TaskStatus{State: a2a.TaskStateCompleted}}
}

func message(taskID, contextID string) *a2a.SendMessageRequest {
	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("hi"))
	msg.TaskID, msg.ContextID = a2a.TaskID(taskID), contextID
	return &a2a.SendMessageRequest{Message: msg}
}

func recordStarted(t *testing.T, calls Calls, contextID, taskID, remoteContextID string) {
	t.Helper()
	if err := calls.Record(context.Background(), Turn{ContextID: contextID}, Call{Contact: "coder", TaskID: a2a.TaskID(taskID), ContextID: remoteContextID}); err != nil {
		t.Fatalf("Calls.Record() error = %v", err)
	}
}

// noDirectory resolves no name; guards built directly don't ask it.
type noDirectory struct{}

func (noDirectory) Card(context.Context, Turn, string) (*a2a.AgentCard, error) {
	return nil, Refusef("no contacts")
}

func (noDirectory) Target(context.Context, Turn, string) (Target, error) {
	return Target{}, Refusef("no contacts")
}

// noTurns finds no turn.
type noTurns struct{}

func (noTurns) Turn(*http.Request, string) (Turn, error) { return Turn{}, ErrNoTurn }

func guarded(t *testing.T, calls Calls, agent a2asrv.RequestHandler, contextID string) a2asrv.RequestHandler {
	t.Helper()
	return guardOf(t, calls, agent, contextID, "coder")
}

// guardOf returns the guard of calls to agent as the contact name, in a
// turn of contextID.
func guardOf(t *testing.T, calls Calls, agent a2asrv.RequestHandler, contextID, name string) a2asrv.RequestHandler {
	t.Helper()
	p, err := New(Config{Turns: noTurns{}, Directory: noDirectory{}, Calls: calls})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return p.Guard(Turn{ContextID: contextID}, name, Local(agent))
}
