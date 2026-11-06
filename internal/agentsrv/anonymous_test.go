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
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/a2aproject/a2a-go/v2/a2aclient/agentcard"
	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/auth"
)

func startPublicServer(t *testing.T, configure func(*agentdir.Config)) *testServer {
	t.Helper()
	return startServer(t, serverOpts{public: publicNet(), channel: newFakeChannel("fake"), configure: func(c *agentdir.Config) {
		c.Policy.PublicAccess = agentdir.AccessAnyone
		if configure != nil {
			configure(c)
		}
	}})
}

// anonymousClient returns an A2A client of the public URL without a token.
func anonymousClient(t *testing.T, ts *testServer) *a2aclient.Client {
	t.Helper()
	ctx := context.Background()
	card, err := agentcard.NewResolver(testHTTP).Resolve(ctx, ts.publicURL())
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	c, err := a2aclient.NewFromCard(ctx, card, a2aclient.WithJSONRPCTransport(testHTTP), a2aclient.WithRESTTransport(testHTTP))
	if err != nil {
		t.Fatalf("NewFromCard() error = %v", err)
	}
	t.Cleanup(func() {
		if err := c.Destroy(); err != nil {
			t.Errorf("Destroy() error = %v", err)
		}
	})
	return c
}

func TestAnonymousConversationsRunInTheirOwnProcess(t *testing.T) {
	t.Parallel()
	ts := startPublicServer(t, nil)
	anon := anonymousClient(t, ts)
	owner := ts.client(ts.pair("phone", auth.RoleOwner))

	task := send(t, anon, userText("pid", "", ""))
	anonPID, ownerPID := resultText(task), resultText(send(t, owner, userText("pid", "", "")))
	if anonPID == ownerPID {
		t.Fatalf("anonymous and owner conversations both ran in %q, want separate processes", anonPID)
	}
	if got := resultText(send(t, owner, userText("pid", "", task.ContextID))); got != anonPID {
		t.Fatalf("owner message in an anonymous conversation ran in %q, want %q", got, anonPID)
	}
}

func TestAnonymousCallerSeesOnlyNamedConversation(t *testing.T) {
	t.Parallel()
	ts := startPublicServer(t, nil)
	anon := anonymousClient(t, ts)
	ctx := context.Background()
	task := send(t, anon, userText("hello", "", ""))
	send(t, anon, userText("hello", "", ""))

	tests := []struct {
		name      string
		contextID string
		want      []a2a.TaskID
	}{
		{name: "without a context", want: []a2a.TaskID{}},
		{name: "with its context", contextID: task.ContextID, want: []a2a.TaskID{task.ID}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res, err := anon.ListTasks(ctx, &a2a.ListTasksRequest{ContextID: tc.contextID})
			if err != nil {
				t.Fatalf("ListTasks() error = %v", err)
			}
			got := []a2a.TaskID{}
			for _, t := range res.Tasks {
				got = append(got, t.ID)
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Fatalf("ListTasks() wrong result (-want +got) diff = %s", diff)
			}
		})
	}
	if _, err := anon.GetTask(ctx, &a2a.GetTaskRequest{ID: task.ID}); err != nil {
		t.Fatalf("GetTask() error = %v", err)
	}
}

func TestAnonymousConversationCallsNoAgents(t *testing.T) {
	t.Parallel()
	ts := startPublicServer(t, nil)
	wantResult(t, send(t, anonymousClient(t, ts), userText("call coder hi", "", "")), a2a.TaskStateCompleted, "no agents URL in the prompt")
}

func TestAnonymousApprovalTimesOut(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{
		public:    publicNet(),
		channel:   newFakeChannel("fake"),
		configure: func(c *agentdir.Config) { c.Policy.PublicAccess = agentdir.AccessAnyone },
		timings:   func(tm *Timings) { tm.AnonymousQuestionTimeout = time.Second },
	})
	anon := anonymousClient(t, ts)
	task := send(t, anon, userText("ask", "", ""))
	if task.Status.State != a2a.TaskStateInputRequired {
		t.Fatalf("task state = %s, want %s", task.Status.State, a2a.TaskStateInputRequired)
	}
	_, err := anon.SendMessage(context.Background(), &a2a.SendMessageRequest{Message: userText("approve", string(task.ID), task.ContextID)})
	if !errors.Is(err, a2a.ErrUnauthorized) {
		t.Fatalf("SendMessage() of an answer error = %v, want %v", err, a2a.ErrUnauthorized)
	}
	wantResult(t, waitState(t, anon, task.ID, a2a.TaskStateCompleted), a2a.TaskStateCompleted, "Push denied.")
}

func TestAnonymousApprovalInOwnerChat(t *testing.T) {
	t.Parallel()
	ts := startPublicServer(t, func(c *agentdir.Config) { c.Approvals.OwnerChat = "fake" })
	linkOwner(t, ts, "telegram:1", "dm1")

	ts.ch.write("telegram:9", "dm9", "ask")
	ts.ch.waitFor(t, "dm9", hasText("This needs the owner's approval"))
	ref, q := ts.ch.waitFor(t, "dm1", hasText("telegram:9, an anonymous caller, needs your approval"))
	if n := ts.ch.questions(); n != 1 {
		t.Fatalf("questions with buttons = %d, want 1, in the owner chat only", n)
	}
	ts.ch.click("telegram:1", "dm1", ref, q.Question, 0)
	ts.ch.waitFor(t, "dm9", hasText("Pushed (allow)."))
}

func TestAnonymousBudget(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_000_000, 0)
	b := &anonymousBudget{perHour: 2, concurrent: 1, now: func() time.Time { return now }, listeners: map[string]*listenerBudget{}}

	if err := b.admit("public"); err != nil {
		t.Fatalf("admit() error = %v, want nil", err)
	}
	release, err := b.run("public")
	if err != nil {
		t.Fatalf("run() error = %v, want nil", err)
	}
	if err := b.admit("public"); !errors.Is(err, errAnonymousBusy) {
		t.Fatalf("admit() while a task runs error = %v, want %v", err, errAnonymousBusy)
	}
	if _, err := b.run("public"); !errors.Is(err, errAnonymousBusy) {
		t.Fatalf("run() while a task runs error = %v, want %v", err, errAnonymousBusy)
	}
	if err := b.admit("chat:telegram"); err != nil {
		t.Fatalf("admit() on another listener error = %v, want nil", err)
	}
	release()
	release()
	if err := b.admit("public"); err != nil {
		t.Fatalf("admit() after release error = %v, want nil", err)
	}
	if err := b.admit("public"); !errors.Is(err, errAnonymousRate) {
		t.Fatalf("admit() over the hourly budget error = %v, want %v", err, errAnonymousRate)
	}
	now = now.Add(time.Hour)
	if err := b.admit("public"); err != nil {
		t.Fatalf("admit() an hour later error = %v, want nil", err)
	}
}
