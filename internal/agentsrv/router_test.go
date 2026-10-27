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
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/channel"
	"github.com/a2aproject/a2a-serve/internal/controlapi"
)

// linkOwner links identity as the owner in conversation conv.
func linkOwner(t *testing.T, ts *testServer, identity, conv string) {
	t.Helper()
	code, err := controlapi.NewClient(ts.dir).Pair(context.Background(), controlapi.PairCodeRequest{Name: "owner", Role: auth.RoleOwner})
	if err != nil {
		t.Fatal(err)
	}
	ts.ch.write(identity, conv, "link "+code.Code)
	ts.ch.waitFor(t, conv, hasText("Linked as owner"))
}

func TestRouterUnknownSenderGetsPairingHint(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{channel: newFakeChannel("fake")})
	ts.ch.write("tg:9", "dm9", "hello")
	ts.ch.waitFor(t, "dm9", hasText("I don't know you yet"))
	ts.ch.write("tg:9", "dm9", "link AAAA-BBBB")
	ts.ch.waitFor(t, "dm9", hasText("invalid or has expired"))
}

func TestRouterRestartsFailingChannel(t *testing.T) {
	t.Parallel()
	ch := newFakeChannel("fake")
	ch.failures = 2
	ts := startServer(t, serverOpts{channel: ch})
	ts.ch.waitReady(t)
	ts.ch.write("tg:9", "dm9", "hello")
	ts.ch.waitFor(t, "dm9", hasText("I don't know you yet"))
}

func TestRouterPreparesOnlyAllowedSenders(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{channel: newFakeChannel("fake")})
	linkOwner(t, ts, "tg:1", "dm1")
	var mu sync.Mutex
	prepared := map[string]bool{}
	withPrepare := func(identity, conv, text string) channel.Inbound {
		in := ts.ch.message(identity, conv, text)
		in.Prepare = func(context.Context, *channel.Inbound) error {
			mu.Lock()
			defer mu.Unlock()
			prepared[identity] = true
			return nil
		}
		return in
	}

	ts.ch.deliver(withPrepare("tg:9", "dm9", "hello"))
	ts.ch.waitFor(t, "dm9", hasText("I don't know you yet"))
	ts.ch.deliver(withPrepare("tg:1", "dm1", "hello"))
	ts.ch.waitFor(t, "dm1", hasText("Hello from fakeacp."))

	mu.Lock()
	defer mu.Unlock()
	if diff := cmp.Diff(map[string]bool{"tg:1": true}, prepared); diff != "" {
		t.Fatalf("Prepare() calls wrong result (-want +got) diff = %s", diff)
	}
}

func TestRouterTurnAndApprovalInChat(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{channel: newFakeChannel("fake")})
	linkOwner(t, ts, "tg:1", "dm1")

	ts.ch.write("tg:1", "dm1", "hello")
	ts.ch.waitFor(t, "dm1", hasText("Hello from fakeacp."))

	ts.ch.write("tg:1", "dm1", "ask")
	ref, q := ts.ch.waitFor(t, "dm1", hasQuestion)
	if len(q.Question.Options) != 3 {
		t.Fatalf("question options = %v, want 3", q.Question.Options)
	}
	ts.ch.click("tg:1", "dm1", ref, q.Question, 1)
	ts.ch.waitFor(t, "dm1", hasText("Pushed (always)."))
}

func TestRouterTypedAnswer(t *testing.T) {
	t.Parallel()
	ch := newFakeChannel("fake")
	ch.buttons = false
	ts := startServer(t, serverOpts{channel: ch})
	linkOwner(t, ts, "mail:me@example.com", "thread1")

	ts.ch.write("mail:me@example.com", "thread1", "ask")
	ts.ch.waitFor(t, "thread1", hasText("Reply approve, always, deny"))
	ts.ch.write("mail:me@example.com", "thread1", "deny")
	ts.ch.waitFor(t, "thread1", hasText("Push denied."))
}

func TestRouterFollowUpAndNewConversation(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{channel: newFakeChannel("fake")})
	linkOwner(t, ts, "tg:1", "dm1")

	ts.ch.write("tg:1", "dm1", "remember red")
	ts.ch.waitFor(t, "dm1", hasText("OK."))
	ts.ch.write("tg:1", "dm1", "recall")
	ts.ch.waitFor(t, "dm1", hasText("You said: red"))
	ts.ch.write("tg:1", "dm1", "/new")
	ts.ch.waitFor(t, "dm1", hasText("Started a new conversation."))
	ts.ch.write("tg:1", "dm1", "recall")
	ts.ch.waitFor(t, "dm1", func(m channel.Outbound) bool { return m.Text == "You said: " })
}

func TestRouterStop(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{channel: newFakeChannel("fake")})
	linkOwner(t, ts, "tg:1", "dm1")
	go ts.ch.write("tg:1", "dm1", "slow")
	ts.ch.waitFor(t, "dm1", hasText("..."))
	ts.ch.write("tg:1", "dm1", "/stop")
	ts.ch.waitFor(t, "dm1", hasText("Stopped."))
}

// linkAs links identity as name with role in conversation conv.
func linkAs(t *testing.T, ts *testServer, name, role, identity, conv string) {
	t.Helper()
	code, err := controlapi.NewClient(ts.dir).Pair(context.Background(), controlapi.PairCodeRequest{Name: name, Role: role})
	if err != nil {
		t.Fatal(err)
	}
	ts.ch.write(identity, conv, "link "+code.Code)
	ts.ch.waitFor(t, conv, hasText("Linked as "+role))
}

func TestRouterApproverAnswersMembersApproval(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{channel: newFakeChannel("fake"), approver: "pair:bob"})
	linkAs(t, ts, "bob", auth.RoleMember, "tg:2", "dm2")
	c := ts.client(ts.pair("grok", auth.RoleMember))

	task := sendAsync(t, c, userText("ask", "", ""))
	ref, q := ts.ch.waitFor(t, "dm2", hasQuestion)
	ts.ch.click("tg:2", "dm2", ref, q.Question, 0)

	wantResult(t, waitState(t, c, task.ID, a2a.TaskStateCompleted), a2a.TaskStateCompleted, "Pushed (allow).")
}

func TestRouterHandlesConversationInOrder(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{channel: newFakeChannel("fake")})
	linkOwner(t, ts, "tg:1", "dm1")

	slow := ts.ch.message("tg:1", "dm1", "remember red")
	slow.Prepare = func(context.Context, *channel.Inbound) error {
		time.Sleep(500 * time.Millisecond)
		return nil
	}
	ts.ch.deliver(slow)
	ts.ch.write("tg:1", "dm1", "recall")

	ts.ch.waitFor(t, "dm1", hasText("You said: red"))
}

func TestRouterInjectedTurnWaitsForPendingApproval(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{channel: newFakeChannel("fake")})
	linkOwner(t, ts, "tg:1", "dm1")
	ts.ch.write("tg:1", "dm1", "ask")
	ref, q := ts.ch.waitFor(t, "dm1", hasQuestion)
	ctx := context.Background()
	contextID, err := ts.srv.store.ContextOf(ctx, "fake", "dm1")
	if err != nil {
		t.Fatal(err)
	}

	if err := ts.srv.router.injectTurn(ctx, contextID, auth.Owner(), "hello"); err != nil {
		t.Fatalf("injectTurn() error = %v", err)
	}
	time.Sleep(500 * time.Millisecond)
	ts.ch.click("tg:1", "dm1", ref, q.Question, 0)

	ts.ch.waitFor(t, "dm1", hasText("Pushed (allow)."))
	ts.ch.waitFor(t, "dm1", hasText("Hello from fakeacp."))
}

func TestRouterGroupMessageDoesNotJoinOthersPendingTask(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{channel: newFakeChannel("fake")})
	linkOwner(t, ts, "tg:1", "dm1")
	linkAs(t, ts, "anna", auth.RoleMember, "tg:2", "dm2")
	linkAs(t, ts, "bob", auth.RoleMember, "tg:3", "dm3")
	inGroup := func(identity, text string) {
		in := ts.ch.message(identity, "grp", text)
		in.Direct = false
		ts.ch.deliver(in)
	}

	inGroup("tg:2", "ask")
	ref, q := ts.ch.waitFor(t, "dm1", hasQuestion)
	inGroup("tg:3", "hello")
	ts.ch.waitFor(t, "grp", hasText("yours starts after it"))
	ts.ch.click("tg:1", "dm1", ref, q.Question, 0)

	ts.ch.waitFor(t, "grp", hasText("Hello from fakeacp."))
}
