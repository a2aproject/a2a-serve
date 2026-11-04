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
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/agentsrv/approvals"
	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/channel"
	"github.com/a2aproject/a2a-serve/internal/controlapi"
)

// ownerCode mints a code which links a chat account to you, as the one
// printed at start.
func ownerCode(t *testing.T, ts *testServer) string {
	t.Helper()
	code, _, err := auth.MintCode(context.Background(), ts.srv.store, auth.LocalPairing)
	if err != nil {
		t.Fatalf("MintCode() error = %v", err)
	}
	return code
}

// linkOwner links identity as you in conversation conv.
func linkOwner(t *testing.T, ts *testServer, identity, conv string) {
	t.Helper()
	ts.ch.write(identity, conv, "link "+ownerCode(t, ts))
	ts.ch.waitFor(t, conv, hasText("Linked as owner"))
}

func TestRouterUnknownSenderGetsPairingHint(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{channel: newFakeChannel("fake")})
	ts.ch.write("telegram:9", "dm9", "hello")
	ts.ch.waitFor(t, "dm9", hasText("I don't know you yet"))
	ts.ch.write("telegram:9", "dm9", "link AAAA-BBBB")
	ts.ch.waitFor(t, "dm9", hasText("invalid or has expired"))
}

func TestRouterPublicAccessAnyone(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{channel: newFakeChannel("fake"), configure: func(c *agentdir.Config) { c.Policy.PublicAccess = agentdir.AccessAnyone }})
	ts.ch.write("telegram:9", "dm9", "hello")
	ts.ch.waitFor(t, "dm9", hasText("Hello from fakeacp."))

	linkOwner(t, ts, "telegram:1", "dm1")
	inGroup := func(identity, text string) {
		in := ts.ch.message(identity, "grp", text)
		in.Direct = false
		ts.ch.deliver(in)
	}
	inGroup("telegram:9", "hello")
	inGroup("telegram:1", "hello")
	ts.ch.waitFor(t, "grp", hasText("Hello from fakeacp."))
	ts.ch.mu.Lock()
	defer ts.ch.mu.Unlock()
	var replies int
	for _, m := range ts.ch.msgs {
		if m.Conv == "grp" {
			replies++
		}
	}
	if replies != 1 {
		t.Fatalf("replies in the group = %d, want 1, to the owner only; messages:\n%v", replies, ts.ch.msgs)
	}
}

func TestRouterRestartsFailingChannel(t *testing.T) {
	t.Parallel()
	ch := newFakeChannel("fake")
	ch.failures = 2
	ts := startServer(t, serverOpts{channel: ch})
	ts.ch.waitReady(t)
	ts.ch.write("telegram:9", "dm9", "hello")
	ts.ch.waitFor(t, "dm9", hasText("I don't know you yet"))
}

func TestRouterPreparesOnlyAllowedSenders(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{channel: newFakeChannel("fake")})
	linkOwner(t, ts, "telegram:1", "dm1")
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

	ts.ch.deliver(withPrepare("telegram:9", "dm9", "hello"))
	ts.ch.waitFor(t, "dm9", hasText("I don't know you yet"))
	ts.ch.deliver(withPrepare("telegram:1", "dm1", "hello"))
	ts.ch.waitFor(t, "dm1", hasText("Hello from fakeacp."))

	mu.Lock()
	defer mu.Unlock()
	if diff := cmp.Diff(map[string]bool{"telegram:1": true}, prepared); diff != "" {
		t.Fatalf("Prepare() calls wrong result (-want +got) diff = %s", diff)
	}
}

func TestRouterTurnAndApprovalInChat(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{channel: newFakeChannel("fake")})
	linkOwner(t, ts, "telegram:1", "dm1")

	ts.ch.write("telegram:1", "dm1", "hello")
	ts.ch.waitFor(t, "dm1", hasText("Hello from fakeacp."))

	ts.ch.write("telegram:1", "dm1", "ask")
	ref, q := ts.ch.waitFor(t, "dm1", hasQuestion)
	if len(q.Question.Options) != 3 {
		t.Fatalf("question options = %v, want 3", q.Question.Options)
	}
	ts.ch.click("telegram:1", "dm1", ref, q.Question, 1)
	ts.ch.waitFor(t, "dm1", hasText("Pushed (always)."))
}

func TestRouterTypedAnswer(t *testing.T) {
	t.Parallel()
	ch := newFakeChannel("fake")
	ch.buttons = false
	ts := startServer(t, serverOpts{channel: ch})
	linkOwner(t, ts, "me@example.com", "thread1")

	ts.ch.write("me@example.com", "thread1", "ask")
	ts.ch.waitFor(t, "thread1", hasText("Reply approve, always, deny"))
	ts.ch.write("me@example.com", "thread1", "deny")
	ts.ch.waitFor(t, "thread1", hasText("Push denied."))
}

func TestRouterAnonymousApprovalGoesToOwnerChat(t *testing.T) {
	t.Parallel()
	ch := newFakeChannel(ChannelEmail)
	ch.buttons = false
	ts := startServer(t, serverOpts{channel: ch, configure: func(c *agentdir.Config) { c.Policy.PublicAccess = agentdir.AccessAnyone }})
	linkOwner(t, ts, "me@example.com", "owner-thread")

	ts.ch.write("stranger@example.org", "thread9", "ask")
	ts.ch.waitFor(t, "thread9", hasText("This needs the owner's approval"))
	_, q := ts.ch.waitFor(t, "owner-thread", hasText("an anonymous caller, needs your approval"))
	if !strings.Contains(q.Text, "stranger@example.org") || !strings.Contains(q.Text, "Reply approve, always, deny") {
		t.Fatalf("owner chat question = %q, want it to name the sender and how to answer", q.Text)
	}
	ts.ch.write("me@example.com", "owner-thread", "approve")
	ts.ch.waitFor(t, "thread9", hasText("Pushed (allow)."))

	ts.ch.mu.Lock()
	defer ts.ch.mu.Unlock()
	for _, m := range ts.ch.msgs {
		if m.Conv == "thread9" && strings.Contains(m.Text, "/hitl/") {
			t.Fatalf("message to the anonymous sender = %q, want no approval link", m.Text)
		}
	}
}

func TestRouterFollowUpAndNewConversation(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{channel: newFakeChannel("fake")})
	linkOwner(t, ts, "telegram:1", "dm1")

	ts.ch.write("telegram:1", "dm1", "remember red")
	ts.ch.waitFor(t, "dm1", hasText("OK."))
	ts.ch.write("telegram:1", "dm1", "recall")
	ts.ch.waitFor(t, "dm1", hasText("You said: red"))
	ts.ch.write("telegram:1", "dm1", "/new")
	ts.ch.waitFor(t, "dm1", hasText("Started a new conversation."))
	ts.ch.write("telegram:1", "dm1", "recall")
	ts.ch.waitFor(t, "dm1", func(m channel.Outbound) bool { return m.Text == "You said: " })
}

func TestRouterStop(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{channel: newFakeChannel("fake")})
	linkOwner(t, ts, "telegram:1", "dm1")
	go ts.ch.write("telegram:1", "dm1", "slow")
	ts.ch.waitFor(t, "dm1", hasText("..."))
	ts.ch.write("telegram:1", "dm1", "/stop")
	ts.ch.waitFor(t, "dm1", hasText("Stopped."))
}

// linkAs links identity as name with role in conversation conv.
func linkAs(t *testing.T, ts *testServer, name, role, identity, conv string) {
	t.Helper()
	code, err := controlapi.NewClient(ts.dir).Pair(context.Background(), controlapi.PairCodeRequest{Name: name, Owner: role == auth.RoleOwner})
	if err != nil {
		t.Fatal(err)
	}
	ts.ch.write(identity, conv, "link "+code.Code)
	ts.ch.waitFor(t, conv, hasText("Linked as "+role))
}

func TestRouterMemberAnswersOwnApprovalInChat(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{channel: newFakeChannel("fake")})
	linkOwner(t, ts, "telegram:1", "dm1")
	linkAs(t, ts, "anna", auth.RoleAuthenticated, "telegram:2", "dm2")

	ts.ch.write("telegram:2", "dm2", "ask")
	ref, q := ts.ch.waitFor(t, "dm2", hasQuestion)
	if got := ts.ch.questions(); got != 1 {
		t.Fatalf("questions sent = %d, want only anna's: she answers her own task", got)
	}
	ts.ch.click("telegram:2", "dm2", ref, q.Question, 0)

	ts.ch.waitFor(t, "dm2", hasText("Pushed (allow)."))
}

func TestRouterUnattendedApprovalGoesToOwnerChat(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{channel: newFakeChannel("telegram")})
	linkOwner(t, ts, "telegram:1", "dm1")
	linkAs(t, ts, "anna", auth.RoleAuthenticated, "telegram:2", "dm2")
	ctx := context.Background()

	if err := ts.srv.router.injectTurn(ctx, a2a.NewContextID(), auth.Principal{ID: "pair:anna", Role: auth.RoleAuthenticated}, "ask"); err != nil {
		t.Fatalf("injectTurn() error = %v", err)
	}
	ref, q := ts.ch.waitFor(t, "dm1", hasQuestion)
	if !strings.Contains(q.Text, "anna needs your approval") {
		t.Fatalf("question text = %q, want it to name anna", q.Text)
	}
	ts.ch.click("telegram:1", "dm1", ref, q.Question, 0)
	ts.ch.waitEdited(t, ref, "✓ Allow by owner")
}

func TestRouterChatTurnApprovalGoesToItsUser(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{channel: newFakeChannel("telegram")})
	linkOwner(t, ts, "telegram:1", "dm1")
	linkAs(t, ts, "anna", auth.RoleAuthenticated, "telegram:2", "dm2")
	ctx := context.Background()
	contextID, err := ts.srv.router.contextOf(ctx, "telegram", "dm2")
	if err != nil {
		t.Fatalf("contextOf() error = %v", err)
	}

	if err := ts.srv.router.injectTurn(ctx, contextID, auth.Principal{ID: "pair:anna", Role: auth.RoleAuthenticated}, "ask"); err != nil {
		t.Fatalf("injectTurn() error = %v", err)
	}
	ref, q := ts.ch.waitFor(t, "dm2", hasQuestion)
	if got := ts.ch.questions(); got != 1 {
		t.Fatalf("questions sent = %d, want only the one in anna's chat", got)
	}
	ts.ch.click("telegram:2", "dm2", ref, q.Question, 0)
	ts.ch.waitFor(t, "dm2", hasText("Pushed (allow)."))
}

func TestRouterOwnerChatOrder(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{channel: newFakeChannel("discord")})
	linkOwner(t, ts, "discord:1", "dm1")
	ctx := context.Background()
	if got, ok := ts.srv.router.OwnerChat(ctx); !ok || got.Channel != "discord" {
		t.Fatalf("OwnerChat() with discord only = %+v, %v, want discord", got, ok)
	}

	tg := newFakeChannel("telegram")
	ts.srv.router.add(tg)
	tg.waitReady(t)
	tg.write("telegram:1", "tgdm1", "join "+ownerCode(t, ts))
	tg.waitFor(t, "tgdm1", hasText("Linked as owner"))

	got, ok := ts.srv.router.OwnerChat(ctx)
	if want := (approvals.Chat{Channel: "telegram", Conv: "tgdm1"}); !ok || got != want {
		t.Fatalf("OwnerChat() with discord and telegram = %+v, %v, want %+v: telegram comes first", got, ok, want)
	}
}

func TestRouterOwnerChat(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		channel   string
		ownerChat string
		link      bool
		linkBen   bool
		want      approvals.Chat
		wantOK    bool
	}{
		{name: "first messenger with a linked owner", channel: "discord", link: true, want: approvals.Chat{Channel: "discord", Conv: "dm1"}, wantOK: true},
		{name: "setting naming an owner", channel: "telegram", ownerChat: "telegram:ben", link: true, linkBen: true, want: approvals.Chat{Channel: "telegram", Conv: "dm3"}, wantOK: true},
		{name: "setting naming a member", channel: "telegram", ownerChat: "telegram:anna", link: true},
		{name: "messenger outside the order", channel: "fake", link: true},
		{name: "no linked owner", channel: "telegram"},
		{name: "setting of a stopped messenger", channel: "telegram", ownerChat: "email", link: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ts := startServer(t, serverOpts{channel: newFakeChannel(tc.channel), configure: func(c *agentdir.Config) { c.Approvals.OwnerChat = tc.ownerChat }})
			linkAs(t, ts, "anna", auth.RoleAuthenticated, "telegram:2", "dm2")
			if tc.linkBen {
				linkAs(t, ts, "ben", auth.RoleOwner, "telegram:3", "dm3")
			}
			if tc.link {
				linkOwner(t, ts, "telegram:1", "dm1")
			}

			got, ok := ts.srv.router.OwnerChat(context.Background())

			if ok != tc.wantOK || got != tc.want {
				t.Fatalf("OwnerChat() = %+v, %v, want %+v, %v", got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

func TestRouterHandlesConversationInOrder(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{channel: newFakeChannel("fake")})
	linkOwner(t, ts, "telegram:1", "dm1")

	slow := ts.ch.message("telegram:1", "dm1", "remember red")
	slow.Prepare = func(context.Context, *channel.Inbound) error {
		time.Sleep(200 * time.Millisecond)
		return nil
	}
	ts.ch.deliver(slow)
	ts.ch.write("telegram:1", "dm1", "recall")

	ts.ch.waitFor(t, "dm1", hasText("You said: red"))
}

func TestRouterInjectedTurnWaitsForPendingApproval(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{channel: newFakeChannel("fake")})
	linkOwner(t, ts, "telegram:1", "dm1")
	ts.ch.write("telegram:1", "dm1", "ask")
	ref, q := ts.ch.waitFor(t, "dm1", hasQuestion)
	ctx := context.Background()
	contextID, err := ts.srv.store.ContextOf(ctx, "fake", "dm1")
	if err != nil {
		t.Fatal(err)
	}

	if err := ts.srv.router.injectTurn(ctx, contextID, auth.Owner(), "hello"); err != nil {
		t.Fatalf("injectTurn() error = %v", err)
	}
	time.Sleep(3 * injectRetry)
	ts.ch.click("telegram:1", "dm1", ref, q.Question, 0)

	ts.ch.waitFor(t, "dm1", hasText("Pushed (allow)."))
	ts.ch.waitFor(t, "dm1", hasText("Hello from fakeacp."))
}

func TestRouterGroupMessageDoesNotJoinOthersPendingTask(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{channel: newFakeChannel("fake")})
	linkOwner(t, ts, "telegram:1", "dm1")
	linkAs(t, ts, "anna", auth.RoleAuthenticated, "telegram:2", "dm2")
	linkAs(t, ts, "bob", auth.RoleAuthenticated, "telegram:3", "dm3")
	inGroup := func(identity, text string) {
		in := ts.ch.message(identity, "grp", text)
		in.Direct = false
		ts.ch.deliver(in)
	}

	inGroup("telegram:2", "ask")
	ref, q := ts.ch.waitFor(t, "grp", hasQuestion)
	inGroup("telegram:3", "hello")
	ts.ch.waitFor(t, "grp", hasText("yours starts after it"))
	ts.ch.click("telegram:3", "grp", ref, q.Question, 0)
	ts.ch.waitFor(t, "grp", hasText("Only the person who started the task or an owner can answer this."))
	ts.ch.click("telegram:2", "grp", ref, q.Question, 0)

	ts.ch.waitFor(t, "grp", hasText("Hello from fakeacp."))
}

func TestRouterDoesNotRecordMessagesOfUnknownSenders(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{channel: newFakeChannel("fake")})
	db, err := sql.Open("sqlite", ts.dir.DBPath())
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})
	stranger := ts.ch.message("telegram:9", "dm9", "hello")
	ts.ch.deliver(stranger)
	ts.ch.waitFor(t, "dm9", hasText("I don't know you yet"))
	linkOwner(t, ts, "telegram:1", "dm1")
	owner := ts.ch.message("telegram:1", "dm1", "hello")
	ts.ch.deliver(owner)
	ts.ch.waitFor(t, "dm1", hasText("Hello from fakeacp."))

	tests := []struct {
		name string
		in   channel.Inbound
		want int
	}{
		{name: "unknown sender", in: stranger, want: 0},
		{name: "owner", in: owner, want: 1},
	}
	for _, tc := range tests {
		var got int
		if err := db.QueryRow(`SELECT COUNT(*) FROM seen WHERE key = ?`, tc.in.Channel+"|"+tc.in.MessageID).Scan(&got); err != nil {
			t.Fatalf("counting seen keys of %s: %v", tc.name, err)
		}
		if got != tc.want {
			t.Fatalf("seen keys of the message of %s = %d, want %d", tc.name, got, tc.want)
		}
	}
}

func TestRouterDropsRedeliveredMessages(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{channel: newFakeChannel("fake")})
	linkOwner(t, ts, "telegram:1", "dm1")
	in := ts.ch.message("telegram:1", "dm1", "hello")
	ts.ch.deliver(in)
	ts.ch.waitFor(t, "dm1", hasText("Hello from fakeacp."))

	ts.ch.deliver(in)
	ts.ch.write("telegram:1", "dm1", "/start")
	ts.ch.waitFor(t, "dm1", hasText("Hi! I'm"))

	ts.ch.mu.Lock()
	defer ts.ch.mu.Unlock()
	var replies int
	for _, m := range ts.ch.msgs {
		if m.Conv == "dm1" && strings.Contains(m.Text, "Hello from fakeacp.") {
			replies++
		}
	}
	if replies != 1 {
		t.Fatalf("replies to a message delivered twice = %d, want 1", replies)
	}
}
