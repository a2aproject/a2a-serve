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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/a2aproject/a2a-go/v2/a2aclient/agentcard"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/a2aproject/a2a-serve/core/contact"
	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/jsonhttp"
	"github.com/a2aproject/a2a-serve/internal/network"
	"github.com/a2aproject/a2a-serve/internal/pairings"
	"github.com/a2aproject/a2a-serve/internal/store"
)

var idleTimings = Timings{WatchInterval: time.Hour, WatchMaxInterval: time.Hour}

type proxyFixture struct {
	*watchesFixture
	claims *fakeClaims
	proxy  *HarnessProxy
}

// fakeClaims gives each conversation a handle, whose claims are the ones of
// the conversation's running turn.
type fakeClaims struct {
	mu    sync.Mutex
	turns map[string]Claims
}

func (c *fakeClaims) handle(_ context.Context, contextID string) (string, error) {
	return "h-" + contextID, nil
}

func (c *fakeClaims) beginTurn(cl Claims) (end func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.turns[cl.ContextID] = cl
	return func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		delete(c.turns, cl.ContextID)
	}
}

func (c *fakeClaims) ClaimsOf(_ context.Context, handle, _ string) (Claims, error) {
	contextID, ok := strings.CutPrefix(handle, "h-")
	if !ok {
		return Claims{}, ErrUnknownHandle
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	cl, ok := c.turns[contextID]
	if !ok {
		return Claims{}, ErrNoTurn
	}
	return cl, nil
}

func newProxyFixture(t *testing.T) *proxyFixture {
	t.Helper()
	wf := newWatchesFixtureWith(t, idleTimings)
	claims := &fakeClaims{turns: map[string]Claims{}}
	return &proxyFixture{
		watchesFixture: wf,
		claims:         claims,
		proxy:          NewHarnessProxy(HarnessProxyDeps{Store: wf.store, Claims: claims, Clients: wf.clients, Watches: wf.ws}),
	}
}

func (f *proxyFixture) serve(t *testing.T, key string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(f.proxy.Routes(harnessKeyCaller(key)))
	t.Cleanup(srv.Close)
	return srv
}

// harnessKeyCaller takes requests with key, or every request for no key,
// as calls from the owner's agent process.
func harnessKeyCaller(key string) Caller {
	return func(r *http.Request) (string, bool) {
		if key == "" {
			return "", true
		}
		tok, ok := auth.BearerToken(r.Header.Get("Authorization"))
		return "", ok && tok == key
	}
}

func (f *proxyFixture) handle(t *testing.T, contextID string) string {
	t.Helper()
	h, err := f.claims.handle(context.Background(), contextID)
	if err != nil {
		t.Fatalf("fakeClaims.handle() error = %v", err)
	}
	return h
}

func (f *proxyFixture) runTurn(t *testing.T, contextID string, user auth.Principal) string {
	t.Helper()
	h := f.handle(t, contextID)
	t.Cleanup(f.claims.beginTurn(Claims{ContextID: contextID, User: user}))
	return h
}

type proxyResponse struct {
	Status int
	Error  string
	Body   []byte
}

func getURL(t *testing.T, u, authorization string) proxyResponse {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, u, nil)
	if err != nil {
		t.Fatalf("http.NewRequestWithContext() error = %v", err)
	}
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	resp, err := testHTTP.Do(req)
	if err != nil {
		t.Fatalf("GET %s error = %v", u, err)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading the response body error = %v", err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatalf("closing the response body error = %v", err)
	}
	out := proxyResponse{Status: resp.StatusCode, Body: body}
	var apiErr jsonhttp.APIError
	if json.Unmarshal(body, &apiErr) == nil {
		out.Error = apiErr.Error
	}
	return out
}

func TestHarnessProxyRoutesAgentCard(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		key       string
		header    string
		user      auth.Principal
		noTurn    bool
		handle    string
		agent     string
		wantCode  int
		wantError string
	}{
		{name: "owner calls a contact", user: auth.Owner(), agent: "coder", wantCode: http.StatusOK},
		{name: "member calls a contact", user: bob, agent: "coder", wantCode: http.StatusOK},
		{name: "owner calls a node", user: auth.Owner(), agent: "grok", wantCode: http.StatusOK},
		{name: "owner calls the schedules agent", user: auth.Owner(), agent: pairings.SchedulesAgent, wantCode: http.StatusOK},
		{name: "unknown handle", user: auth.Owner(), handle: "unknownhandle", agent: "coder", wantCode: http.StatusNotFound, wantError: ErrUnknownHandle.Error()},
		{name: "conversation without a running turn", user: auth.Owner(), noTurn: true, agent: "coder", wantCode: http.StatusForbidden, wantError: ErrNoTurn.Error()},
		{name: "guest user", user: auth.Principal{ID: "telegram:9", Role: auth.RoleGuest}, agent: "coder", wantCode: http.StatusOK},
		{name: "user at the hop limit", user: auth.Principal{ID: "pair:anna", Role: auth.RoleMember, Identity: auth.Identity{Hops: auth.MaxHops}}, agent: "coder", wantCode: http.StatusOK},
		{name: "user below the hop limit", user: auth.Principal{ID: "pair:anna", Role: auth.RoleMember, Identity: auth.Identity{Hops: auth.MaxHops - 1}}, agent: "coder", wantCode: http.StatusOK},
		{name: "name that isn't callable", user: auth.Owner(), agent: "nobody", wantCode: http.StatusOK},
		{name: "harness key", key: "harness-key", header: "Bearer harness-key", user: auth.Owner(), agent: "coder", wantCode: http.StatusOK},
		{name: "no harness key", key: "harness-key", user: auth.Owner(), agent: "coder", wantCode: http.StatusUnauthorized, wantError: "agent key required"},
		{name: "wrong harness key", key: "harness-key", header: "Bearer nope", user: auth.Owner(), agent: "coder", wantCode: http.StatusUnauthorized, wantError: "agent key required"},
		{name: "harness key of another scheme", key: "harness-key", header: "Basic harness-key", user: auth.Owner(), agent: "coder", wantCode: http.StatusUnauthorized, wantError: "agent key required"},
		{name: "key is checked before the handle", key: "harness-key", user: auth.Owner(), handle: "unknownhandle", agent: "coder", wantCode: http.StatusUnauthorized, wantError: "agent key required"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newProxyFixture(t)
			putContact(t, f.store, store.Contact{Name: "coder", URL: f.remote.URL})
			putNode(t, f.store, "grok")
			srv := f.serve(t, tc.key)
			handle := tc.handle
			switch {
			case handle != "":
			case tc.noTurn:
				handle = f.handle(t, "c1")
			default:
				handle = f.runTurn(t, "c1", tc.user)
			}

			got := getURL(t, srv.URL+"/"+handle+"/"+tc.agent+"/.well-known/agent-card.json", tc.header)

			if got.Status != tc.wantCode {
				t.Fatalf("GET agent card status = %d (%s), want %d", got.Status, got.Error, tc.wantCode)
			}
			if !strings.Contains(got.Error, tc.wantError) {
				t.Fatalf("GET agent card error = %q, want it to contain %q", got.Error, tc.wantError)
			}
		})
	}
}

func TestHarnessProxyRefusesTheCall(t *testing.T) {
	t.Parallel()
	guest := auth.Principal{ID: "telegram:9", Role: auth.RoleGuest}
	atHopLimit := auth.Principal{ID: "pair:anna", Role: auth.RoleMember, Identity: auth.Identity{Hops: auth.MaxHops}}
	tests := []struct {
		name      string
		user      auth.Principal
		agent     string
		transport a2a.TransportProtocol
		want      string
	}{
		{name: "guest user", user: guest, agent: "coder", transport: a2a.TransportProtocolJSONRPC, want: "the user of this conversation can't call other agents; answer without calling one"},
		{name: "guest user and the schedules agent", user: guest, agent: pairings.SchedulesAgent, transport: a2a.TransportProtocolJSONRPC, want: "the user of this conversation can't call other agents; answer without calling one"},
		{name: "user without a role", user: auth.Principal{ID: "pair:gone"}, agent: "coder", transport: a2a.TransportProtocolJSONRPC, want: "the user of this conversation can't call other agents; answer without calling one"},
		{name: "user at the hop limit", user: atHopLimit, agent: "coder", transport: a2a.TransportProtocolJSONRPC, want: "too many agents in this chain (3); answer without calling another agent"},
		{name: "user at the hop limit over REST", user: atHopLimit, agent: "coder", transport: a2a.TransportProtocolHTTPJSON, want: "too many agents in this chain (3); answer without calling another agent"},
		{name: "name that isn't callable", user: auth.Owner(), agent: "nobody", transport: a2a.TransportProtocolJSONRPC, want: `"nobody" is not an agent you can call; the [a2a agents] line lists them`},
		{name: "name that isn't callable over REST", user: auth.Owner(), agent: "nobody", transport: a2a.TransportProtocolHTTPJSON, want: `"nobody" is not an agent you can call; the [a2a agents] line lists them`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newProxyFixture(t)
			putContact(t, f.store, store.Contact{Name: "coder", URL: f.remote.URL})
			srv := f.serve(t, "")
			handle := f.runTurn(t, "c1", tc.user)
			client := proxyClientOver(t, srv.URL+"/"+handle+"/"+tc.agent, tc.transport)
			msg := &a2a.SendMessageRequest{Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(promptDone))}

			_, err := client.SendMessage(context.Background(), msg)
			if err == nil || err.Error() != tc.want || !errors.Is(err, a2a.ErrUnauthorized) {
				t.Fatalf("SendMessage() error = %v, want permission denied with %q", err, tc.want)
			}
			for _, err := range client.SendStreamingMessage(context.Background(), msg) {
				if err == nil || err.Error() != tc.want {
					t.Fatalf("SendStreamingMessage() error = %v, want %q", err, tc.want)
				}
			}
			if _, err := client.GetTask(context.Background(), &a2a.GetTaskRequest{ID: "t1"}); err == nil || err.Error() != tc.want {
				t.Fatalf("GetTask() error = %v, want %q", err, tc.want)
			}
			if got := f.remote.received(); len(got) != 0 {
				t.Fatalf("calls that reached the contact = %v, want none", got)
			}
		})
	}
}

func TestHarnessProxyRoutesWithoutKeyConfigured(t *testing.T) {
	t.Parallel()
	f := newProxyFixture(t)
	putContact(t, f.store, store.Contact{Name: "coder", URL: f.remote.URL})
	srv := f.serve(t, "")
	handle := f.runTurn(t, "c1", auth.Owner())

	got := getURL(t, srv.URL+"/"+handle+"/coder", "")

	if got.Status != http.StatusOK {
		t.Fatalf("GET agent card without a configured key status = %d (%s), want %d", got.Status, got.Error, http.StatusOK)
	}
}

func TestHarnessProxyRefusesOversizedBody(t *testing.T) {
	t.Parallel()
	f := newProxyFixture(t)
	putContact(t, f.store, store.Contact{Name: "coder", URL: f.remote.URL})
	srv := f.serve(t, "")
	handle := f.runTurn(t, "c1", auth.Owner())

	got := postOversized(t, srv.URL+"/"+handle+"/coder/message:send", "")

	if got != http.StatusRequestEntityTooLarge {
		t.Fatalf("POST to coder status = %d, want %d", got, http.StatusRequestEntityTooLarge)
	}
}

func TestHarnessProxyServesCardAtTheAgentsURL(t *testing.T) {
	t.Parallel()
	f := newProxyFixture(t)
	putContact(t, f.store, store.Contact{Name: "coder", URL: f.remote.URL})
	srv := f.serve(t, "")
	handle := f.runTurn(t, "c1", auth.Owner())
	base := srv.URL + "/" + handle + "/coder"
	tests := []struct {
		name string
		path string
	}{
		{name: "agent URL", path: base},
		{name: "well-known card", path: base + "/.well-known/agent-card.json"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := getURL(t, tc.path, "")
			if got.Status != http.StatusOK {
				t.Fatalf("GET %s status = %d (%s), want %d", tc.path, got.Status, got.Error, http.StatusOK)
			}
			var card a2a.AgentCard
			if err := json.Unmarshal(got.Body, &card); err != nil {
				t.Fatalf("json.Unmarshal(%s) error = %v", got.Body, err)
			}
			var urls []string
			for _, i := range card.SupportedInterfaces {
				urls = append(urls, i.URL)
			}
			if diff := cmp.Diff([]string{base, base}, urls); diff != "" {
				t.Fatalf("agent card interface URLs wrong result (-want +got) diff = %s", diff)
			}
			if card.Name != "coder" {
				t.Fatalf("agent card name = %q, want %q", card.Name, "coder")
			}
		})
	}
}

func TestHarnessProxyCallsContactForTheUserOfTheTurn(t *testing.T) {
	t.Parallel()
	f := newProxyFixture(t)
	putContact(t, f.store, store.Contact{Name: "coder", URL: f.remote.URL, Token: "a2p_agent"})
	srv := f.serve(t, "")
	user := auth.Principal{ID: "pair:anna", Role: auth.RoleMember, Identity: auth.Identity{Hops: 1}}
	own := f.runTurn(t, "c1", user)
	otherHandle := f.runTurn(t, "c2", auth.Owner())
	client := proxyClient(t, srv.URL+"/"+own+"/coder")

	res, err := client.SendMessage(context.Background(), &a2a.SendMessageRequest{Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(promptDone))})
	if err != nil {
		t.Fatalf("SendMessage() through the agents URL error = %v", err)
	}
	task, ok := res.(*a2a.Task)
	if !ok {
		t.Fatalf("SendMessage() = %T, want a task", res)
	}
	f.remote.waitState(t, task.ID, a2a.TaskStateCompleted)

	var hops []string
	for _, r := range f.remote.received() {
		if r.Path == "/" {
			if r.Authorization != "Bearer a2p_agent" {
				t.Fatalf("call to the contact authorization = %q, want the agent's token for the contact", r.Authorization)
			}
			if r.Caller == "" {
				t.Fatalf("call to the contact has no caller header, want the ID of the turn's user")
			}
			hops = append(hops, r.Hops)
		}
	}
	if len(hops) == 0 || hops[0] != "2" {
		t.Fatalf("hops of the call to the contact = %v, want 2 for a user one hop deep", hops)
	}
	started, err := f.store.HasCallTask(context.Background(), "c1", "coder", string(task.ID))
	if err != nil || !started {
		t.Fatalf("store.HasCallTask() = %v, %v, want the conversation recorded as the starter of the task", started, err)
	}
	if _, err := client.GetTask(context.Background(), &a2a.GetTaskRequest{ID: task.ID}); err != nil {
		t.Fatalf("GetTask() of its own task error = %v", err)
	}
	if _, err := proxyClient(t, srv.URL+"/"+otherHandle+"/coder").GetTask(context.Background(), &a2a.GetTaskRequest{ID: task.ID}); err == nil {
		t.Fatalf("GetTask() of another conversation's task error = nil, want an error")
	}
}

func proxyClient(t *testing.T, agentURL string) *a2aclient.Client {
	t.Helper()
	return proxyClientOver(t, agentURL, a2a.TransportProtocolJSONRPC)
}

// proxyClientOver returns a client of the agent at agentURL that prefers
// transport.
func proxyClientOver(t *testing.T, agentURL string, transport a2a.TransportProtocol) *a2aclient.Client {
	t.Helper()
	ctx := context.Background()
	card, err := agentcard.NewResolver(testHTTP).Resolve(ctx, agentURL)
	if err != nil {
		t.Fatalf("Resolve(%s) error = %v", agentURL, err)
	}
	client, err := a2aclient.NewFromCard(ctx, card, a2aclient.WithJSONRPCTransport(testHTTP), a2aclient.WithRESTTransport(testHTTP), a2aclient.WithConfig(a2aclient.Config{PreferredTransports: []a2a.TransportProtocol{transport}}))
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

// recordingAgent stands in for the agent behind a guardedHandler, and
// records which calls get through to it.
type recordingAgent struct {
	a2asrv.RequestHandler

	mu      sync.Mutex
	reached []string
	// task is what the agent returns for a task, and sends back.
	task *a2a.Task
	// listed are the tasks it lists.
	listed []*a2a.Task
	// stream are the events it streams before it waits for the end of the call.
	stream []a2a.Event
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

func (a *recordingAgent) SendStreamingMessage(ctx context.Context, _ *a2a.SendMessageRequest) iter.Seq2[a2a.Event, error] {
	a.reach("SendStreamingMessage")
	return func(yield func(a2a.Event, error) bool) {
		if a.stream == nil {
			if a.task != nil {
				yield(a.task, nil)
			}
			return
		}
		for _, ev := range a.stream {
			if !yield(ev, nil) {
				return
			}
		}
		<-ctx.Done()
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

// ListTasks pages a.listed, where the token of a page is the index of its
// first task.
func (a *recordingAgent) ListTasks(_ context.Context, req *a2a.ListTasksRequest) (*a2a.ListTasksResponse, error) {
	a.reach("ListTasks")
	pageSize := req.PageSize
	if pageSize == 0 {
		pageSize = len(a.listed)
	}
	start := 0
	if req.PageToken != "" {
		var err error
		if start, err = strconv.Atoi(req.PageToken); err != nil {
			return nil, err
		}
	}
	end := min(start+pageSize, len(a.listed))
	res := &a2a.ListTasksResponse{Tasks: a.listed[start:end], TotalSize: len(a.listed), PageSize: pageSize}
	if end < len(a.listed) {
		res.NextPageToken = strconv.Itoa(end)
	}
	return res, nil
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

func (f *proxyFixture) guarded(agent a2asrv.RequestHandler, contextID string, user auth.Principal) a2asrv.RequestHandler {
	return f.proxy.calls.Guard(Claims{ContextID: contextID, User: user}.turn(), "coder", contact.Local(agent))
}

func (f *proxyFixture) recordStarted(t *testing.T, contextID, taskID, remoteContextID string) {
	t.Helper()
	call := store.CallTask{ContextID: contextID, Agent: "coder", TaskID: taskID, RemoteContextID: remoteContextID}
	if err := f.store.PutCallTask(context.Background(), call); err != nil {
		t.Fatalf("store.PutCallTask() error = %v", err)
	}
}

func message(taskID, contextID string) *a2a.SendMessageRequest {
	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("hi"))
	msg.TaskID, msg.ContextID = a2a.TaskID(taskID), contextID
	return &a2a.SendMessageRequest{Message: msg}
}

func TestGuardedHandlerRecordsTasksItStarts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newProxyFixture(t)
	agent := &recordingAgent{task: completedTask("t9", "r9")}
	own := f.guarded(agent, "c1", auth.Owner())
	other := f.guarded(agent, "c2", auth.Owner())

	if _, err := own.SendMessage(ctx, message("", "")); err != nil {
		t.Fatalf("guardedHandler.SendMessage() error = %v", err)
	}

	if _, err := own.GetTask(ctx, &a2a.GetTaskRequest{ID: "t9"}); err != nil {
		t.Fatalf("guardedHandler.GetTask() of the task it started error = %v", err)
	}
	if _, err := own.SendMessage(ctx, message("", "r9")); err != nil {
		t.Fatalf("guardedHandler.SendMessage() into the context it started error = %v", err)
	}
	if _, err := other.GetTask(ctx, &a2a.GetTaskRequest{ID: "t9"}); !errors.Is(err, a2a.ErrTaskNotFound) {
		t.Fatalf("guardedHandler.GetTask() of another conversation's task error = %v, want %v", err, a2a.ErrTaskNotFound)
	}
}

func TestGuardedHandlerWatchesTasksTheHarnessDoesNotWaitFor(t *testing.T) {
	t.Parallel()
	identity := auth.Identity{Pairing: "anna", Hops: 1}
	user := auth.Principal{ID: "pair:anna", Role: auth.RoleMember, Identity: identity}
	withStatus := func(state a2a.TaskState, msg *a2a.Message) *a2a.Task {
		return &a2a.Task{ID: "t9", ContextID: "r9", Status: a2a.TaskStatus{State: state, Message: msg}}
	}
	tests := []struct {
		name      string
		task      func(t *testing.T) *a2a.Task
		wantWatch bool
		wantAsked int
	}{
		{name: "working", task: func(*testing.T) *a2a.Task { return withStatus(a2a.TaskStateWorking, nil) }, wantWatch: true},
		{name: "submitted", task: func(*testing.T) *a2a.Task { return withStatus(a2a.TaskStateSubmitted, nil) }, wantWatch: true},
		{name: "completed", task: func(*testing.T) *a2a.Task { return withStatus(a2a.TaskStateCompleted, nil) }},
		{name: "failed", task: func(*testing.T) *a2a.Task { return withStatus(a2a.TaskStateFailed, nil) }},
		{name: "auth required", task: func(*testing.T) *a2a.Task { return withStatus(a2a.TaskStateAuthRequired, nil) }},
		{name: "plain question", task: func(*testing.T) *a2a.Task {
			return withStatus(a2a.TaskStateInputRequired, a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart("Which branch?")))
		}},
		{name: "permission question", wantWatch: true, wantAsked: 1, task: func(t *testing.T) *a2a.Task {
			return withStatus(a2a.TaskStateInputRequired, permissionStatus(t).Message)
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			f := newProxyFixture(t)
			if err := f.store.SetContext(ctx, "fake", "dm1", "c1"); err != nil {
				t.Fatalf("store.SetContext() error = %v", err)
			}
			agent := &recordingAgent{task: tc.task(t)}

			if _, err := f.guarded(agent, "c1", user).SendMessage(ctx, message("", "")); err != nil {
				t.Fatalf("guardedHandler.SendMessage() error = %v", err)
			}

			got, err := f.store.Watches(ctx)
			if err != nil {
				t.Fatalf("store.Watches() error = %v", err)
			}
			var want []store.Watch
			if tc.wantWatch {
				want = []store.Watch{{Contact: "coder", TaskID: "t9", RemoteContextID: "r9", OriginContextID: "c1", OnBehalfOf: "pair:anna"}}
			}
			if diff := cmp.Diff(want, got, cmpopts.IgnoreFields(store.Watch{}, "Identity", "Created", "FailingSince")); diff != "" {
				t.Fatalf("store.Watches() wrong result (-want +got) diff = %s", diff)
			}
			if tc.wantWatch {
				var gotIdentity auth.Identity
				if err := json.Unmarshal([]byte(got[0].Identity), &gotIdentity); err != nil {
					t.Fatalf("json.Unmarshal(%q) error = %v", got[0].Identity, err)
				}
				if diff := cmp.Diff(identity, gotIdentity); diff != "" {
					t.Fatalf("watch identity wrong result (-want +got) diff = %s", diff)
				}
			}
			if n := len(f.chat.questions()); n != tc.wantAsked {
				t.Fatalf("questions asked = %d, want %d", n, tc.wantAsked)
			}
		})
	}
}

func TestGuardedHandlerStoresWatchBeforeAskingQuestionOnTask(t *testing.T) {
	t.Parallel()
	f := newProxyFixture(t)
	agent := &recordingAgent{task: &a2a.Task{ID: "t9", ContextID: "r9", Status: permissionStatus(t)}}
	agent.task.Status.State = a2a.TaskStateInputRequired

	if _, err := f.guarded(agent, "c1", auth.Owner()).SendMessage(context.Background(), message("", "")); err != nil {
		t.Fatalf("guardedHandler.SendMessage() error = %v", err)
	}

	asked := f.asked.questions()
	if len(asked) != 1 || !asked[0].Watched {
		t.Fatalf("questions asked on tasks = %+v, want one asked after its watch was stored", asked)
	}
}

func TestGuardedHandlerWatchesTaskOfDroppedStream(t *testing.T) {
	t.Parallel()
	f := newProxyFixture(t)
	task := &a2a.Task{ID: "t1", ContextID: "r1"}
	agent := &recordingAgent{stream: []a2a.Event{a2a.NewStatusUpdateEvent(task, a2a.TaskStateWorking, nil)}}
	g := f.guarded(agent, "c1", auth.Owner())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	for _, err := range g.SendStreamingMessage(ctx, message("", "")) {
		if err != nil {
			t.Fatalf("guardedHandler.SendStreamingMessage() error = %v", err)
		}
		cancel()
		break
	}

	got, err := f.store.Watches(context.Background())
	if err != nil {
		t.Fatalf("store.Watches() error = %v", err)
	}
	if len(got) != 1 || got[0].TaskID != "t1" || got[0].OriginContextID != "c1" {
		t.Fatalf("store.Watches() = %+v, want a watch of task t1 for conversation c1", got)
	}
	started, err := f.store.HasCallTask(context.Background(), "c1", "coder", "t1")
	if err != nil || !started {
		t.Fatalf("store.HasCallTask() = %v, %v, want the streamed task recorded", started, err)
	}
}

func TestGuardedHandlerKeepsApprovalsWithTheUser(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tests := []struct {
		name      string
		status    a2a.TaskStatus
		wantErr   error
		wantAsked int
	}{
		{
			name:      "permission question",
			status:    permissionStatus(t),
			wantErr:   a2a.ErrUnauthorized,
			wantAsked: 1,
		},
		{
			name:   "plain question",
			status: a2a.TaskStatus{State: a2a.TaskStateInputRequired, Message: a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart("Which branch?"))},
		},
		{
			name:   "running task",
			status: a2a.TaskStatus{State: a2a.TaskStateWorking},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newProxyFixture(t)
			f.recordStarted(t, "c1", "t1", "r1")
			if err := f.store.SetContext(ctx, "fake", "dm1", "c1"); err != nil {
				t.Fatalf("store.SetContext() error = %v", err)
			}
			agent := &recordingAgent{task: &a2a.Task{ID: "t1", ContextID: "r1", Status: tc.status}}

			_, err := f.guarded(agent, "c1", auth.Owner()).SendMessage(ctx, message("t1", "r1"))

			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("guardedHandler.SendMessage() answering the task error = %v, want %v", err, tc.wantErr)
			}
			for _, call := range agent.calls() {
				if call != "GetTask" && tc.wantErr != nil {
					t.Fatalf("calls that reached the agent = %v, want the answer to the permission question held back", agent.calls())
				}
			}
			if n := len(f.chat.questions()); n != tc.wantAsked {
				t.Fatalf("questions asked = %d, want %d", n, tc.wantAsked)
			}
			for _, a := range f.chat.questions() {
				if _, err := f.store.QuestionByID(ctx, a.Rec.ID); err != nil {
					t.Fatalf("store.QuestionByID() error = %v", err)
				}
			}
		})
	}
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
	f := newProxyFixture(t)
	g := f.guarded(streamingHandler{}, "c1", auth.Owner())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	for _, err := range g.SendStreamingMessage(ctx, &a2a.SendMessageRequest{Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("hi"))}) {
		if err != nil {
			t.Fatalf("SendStreamingMessage() error = %v", err)
		}
		cancel()
		break
	}

	ws, err := f.store.Watches(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(ws) != 1 || ws[0].TaskID != "t1" {
		t.Fatalf("store.Watches() = %+v, want a watch of task t1", ws)
	}
}

// failingStreamHandler fails every stream with err.
type failingStreamHandler struct {
	a2asrv.RequestHandler
	err error
}

func (h failingStreamHandler) SendStreamingMessage(context.Context, *a2a.SendMessageRequest) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) { yield(nil, h.err) }
}

func TestGuardedHandlerNamesTheAgentThatTheNetworkDoesNotReach(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
	}{
		{name: "agent called doesn't answer", err: network.ErrPeerUnavailable},
		{name: "network provider doesn't answer", err: network.ErrProviderStalled},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newProxyFixture(t)
			g := f.guarded(networkNamed{RequestHandler: failingStreamHandler{err: fmt.Errorf("failed to send HTTP request: %w", tc.err)}, agent: "coder"}, "c1", auth.Owner())

			_, err := g.SendMessage(t.Context(), &a2a.SendMessageRequest{Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("hi"))})

			if want := "coder: " + tc.err.Error(); err == nil || err.Error() != want {
				t.Fatalf("guardedHandler.SendMessage() error = %v, want %q", err, want)
			}
		})
	}
}
