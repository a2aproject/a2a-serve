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
	"github.com/a2aproject/a2a-serve/internal/agentsrv/messenger"
	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/channel"
	"github.com/a2aproject/a2a-serve/internal/channel/channeltest"
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
	ts.ch.Write(identity, conv, "link "+ownerCode(t, ts))
	ts.ch.WaitFor(t, conv, channeltest.HasText("Linked as owner"))
}

func TestRouterUnknownSenderGetsPairingHint(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{channel: channeltest.NewChannel("fake")})
	ts.ch.Write("telegram:9", "dm9", "hello")
	ts.ch.WaitFor(t, "dm9", channeltest.HasText("I don't know you yet"))
	ts.ch.Write("telegram:9", "dm9", "link AAAA-BBBB")
	ts.ch.WaitFor(t, "dm9", channeltest.HasText("invalid or has expired"))
}

func TestRouterPublicAllowIncomingAnyone(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{channel: channeltest.NewChannel("fake"), configure: func(c *agentdir.Config) { c.Policy.PublicAllowIncoming = agentdir.AccessAnyone }})
	ts.ch.Write("telegram:9", "dm9", "hello")
	ts.ch.WaitFor(t, "dm9", channeltest.HasText("Hello from fakeacp."))

	linkOwner(t, ts, "telegram:1", "dm1")
	inGroup := func(identity, text string) {
		in := ts.ch.Message(identity, "grp", text)
		in.Direct = false
		ts.ch.Deliver(in)
	}
	inGroup("telegram:9", "hello")
	inGroup("telegram:1", "hello")
	ts.ch.WaitFor(t, "grp", channeltest.HasText("Hello from fakeacp."))
	msgs := ts.ch.Transcript()
	var replies int
	for _, m := range msgs {
		if m.Conv == "grp" {
			replies++
		}
	}
	if replies != 1 {
		t.Fatalf("replies in the group = %d, want 1, to the owner only; messages:\n%v", replies, msgs)
	}
}

func TestRouterRestartsFailingChannel(t *testing.T) {
	t.Parallel()
	ch := channeltest.NewChannel("fake")
	ch.Failures = 2
	ts := startServer(t, serverOpts{channel: ch})
	ts.ch.WaitReady(t)
	ts.ch.Write("telegram:9", "dm9", "hello")
	ts.ch.WaitFor(t, "dm9", channeltest.HasText("I don't know you yet"))
}

func TestRouterPreparesOnlyAllowedSenders(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{channel: channeltest.NewChannel("fake")})
	linkOwner(t, ts, "telegram:1", "dm1")
	var mu sync.Mutex
	prepared := map[string]bool{}
	withPrepare := func(identity, conv, text string) channel.Inbound {
		in := ts.ch.Message(identity, conv, text)
		in.Prepare = func(context.Context, *channel.Inbound) error {
			mu.Lock()
			defer mu.Unlock()
			prepared[identity] = true
			return nil
		}
		return in
	}

	ts.ch.Deliver(withPrepare("telegram:9", "dm9", "hello"))
	ts.ch.WaitFor(t, "dm9", channeltest.HasText("I don't know you yet"))
	ts.ch.Deliver(withPrepare("telegram:1", "dm1", "hello"))
	ts.ch.WaitFor(t, "dm1", channeltest.HasText("Hello from fakeacp."))

	mu.Lock()
	defer mu.Unlock()
	if diff := cmp.Diff(map[string]bool{"telegram:1": true}, prepared); diff != "" {
		t.Fatalf("Prepare() calls wrong result (-want +got) diff = %s", diff)
	}
}

func TestRouterTurnAndApprovalInChat(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{channel: channeltest.NewChannel("fake")})
	linkOwner(t, ts, "telegram:1", "dm1")

	ts.ch.Write("telegram:1", "dm1", "hello")
	ts.ch.WaitFor(t, "dm1", channeltest.HasText("Hello from fakeacp."))

	ts.ch.Write("telegram:1", "dm1", "ask")
	ref, q := ts.ch.WaitFor(t, "dm1", channeltest.HasQuestion)
	if len(q.Question.Options) != 3 {
		t.Fatalf("question options = %v, want 3", q.Question.Options)
	}
	ts.ch.Click("telegram:1", "dm1", ref, q.Question, 1)
	ts.ch.WaitFor(t, "dm1", channeltest.HasText("Pushed (always)."))
}

func TestRouterTypedAnswer(t *testing.T) {
	t.Parallel()
	ch := channeltest.NewChannel("fake")
	ch.Buttons = false
	ts := startServer(t, serverOpts{channel: ch})
	linkOwner(t, ts, "me@example.com", "thread1")

	ts.ch.Write("me@example.com", "thread1", "ask")
	ts.ch.WaitFor(t, "thread1", channeltest.HasText("Reply approve, always, deny"))
	ts.ch.Write("me@example.com", "thread1", "deny")
	ts.ch.WaitFor(t, "thread1", channeltest.HasText("Push denied."))
}

func TestRouterTypedAnswerWithMoreTextIsAmbiguous(t *testing.T) {
	t.Parallel()
	ch := channeltest.NewChannel("fake")
	ch.Buttons = false
	ts := startServer(t, serverOpts{channel: ch})
	linkOwner(t, ts, "me@example.com", "thread1")
	ts.ch.Write("me@example.com", "thread1", "ask")
	ts.ch.WaitFor(t, "thread1", channeltest.HasText("Reply approve, always, deny"))

	ts.ch.Write("me@example.com", "thread1", "yes\n\nbut only in tests/")

	ts.ch.WaitFor(t, "thread1", channeltest.HasText("Reply with only one of: approve, always, deny."))
	ts.ch.Write("me@example.com", "thread1", "deny")
	ts.ch.WaitFor(t, "thread1", channeltest.HasText("Push denied."))
}

func TestRouterTypedReplyAfterStoppedQuestionReachesAgent(t *testing.T) {
	t.Parallel()
	ch := channeltest.NewChannel("fake")
	ch.Buttons = false
	ts := startServer(t, serverOpts{channel: ch})
	linkOwner(t, ts, "me@example.com", "thread1")
	ts.ch.Write("me@example.com", "thread1", "ask")
	ref, _ := ts.ch.WaitFor(t, "thread1", channeltest.HasText("Reply approve, always, deny"))

	ts.ch.Write("me@example.com", "thread1", "/stop")
	ts.ch.WaitFor(t, "thread1", channeltest.HasText("Stopped."))
	ts.ch.WaitEdited(t, ref, "no longer waits for an answer")
	ts.ch.Write("me@example.com", "thread1", "yes")

	ts.ch.WaitFor(t, "thread1", channeltest.HasText("Hello from fakeacp."))
}

func TestRouterStoppedQuestionLosesButtons(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{channel: channeltest.NewChannel("fake")})
	linkOwner(t, ts, "telegram:1", "dm1")
	ts.ch.Write("telegram:1", "dm1", "ask")
	ref, _ := ts.ch.WaitFor(t, "dm1", channeltest.HasQuestion)

	ts.ch.Write("telegram:1", "dm1", "/stop")

	ts.ch.WaitEdited(t, ref, "no longer waits for an answer")
	if got := ts.ch.Questions(); got != 0 {
		t.Fatalf("messages with buttons after the task stopped = %d, want 0; messages:\n%s", got, ts.ch.Dump())
	}
}

func TestRouterTypedReplyAfterRestartReachesAgent(t *testing.T) {
	t.Parallel()
	ch := channeltest.NewChannel("fake")
	ch.Buttons = false
	ts := startServer(t, serverOpts{channel: ch})
	linkOwner(t, ts, "me@example.com", "thread1")
	ts.ch.Write("me@example.com", "thread1", "ask")
	ts.ch.WaitFor(t, "thread1", channeltest.HasText("Reply approve, always, deny"))
	root := ts.dir.Root
	ts.stop()

	restarted := startServer(t, serverOpts{dir: root, channel: channeltest.NewChannel("fake")})
	restarted.ch.Write("me@example.com", "thread1", "yes")

	restarted.ch.WaitFor(t, "thread1", channeltest.HasText("Hello from fakeacp."))
}

func TestRouterStarterAnswersQuestionAfterOwnerAnswered(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{channel: channeltest.NewChannel("fake")})
	linkOwner(t, ts, "telegram:1", "dm1")
	linkAs(t, ts, "anna", auth.RoleAuthenticated, "telegram:2", "dm2")
	inGroup := func(identity, text string) {
		in := ts.ch.Message(identity, "grp", text)
		in.Direct = false
		ts.ch.Deliver(in)
	}

	inGroup("telegram:2", "ask twice")
	first, q1 := ts.ch.WaitFor(t, "grp", channeltest.HasQuestion)
	ts.ch.Click("telegram:1", "grp", first, q1.Question, 0)
	second, q2 := ts.ch.WaitFor(t, "grp", func(m channel.Outbound) bool { return channeltest.HasQuestion(m) && m.Question.ID != q1.Question.ID })
	ts.ch.Click("telegram:2", "grp", second, q2.Question, 0)

	ts.ch.WaitFor(t, "grp", channeltest.HasText("Pushed twice (allow, allow)."))
}

func TestRouterAnswerElsewhereRendersInChat(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{channel: channeltest.NewChannel("fake")})
	linkOwner(t, ts, "telegram:1", "dm1")
	ts.ch.Write("telegram:1", "dm1", "ask")
	_, q := ts.ch.WaitFor(t, "dm1", channeltest.HasQuestion)
	ctx := context.Background()
	rec, err := ts.srv.store.QuestionByID(ctx, q.Question.ID)
	if err != nil {
		t.Fatalf("QuestionByID() error = %v", err)
	}

	if err := ts.srv.approvals.Answer(ctx, rec, auth.Owner(), approvals.FromRecord(rec).Options[0]); err != nil {
		t.Fatalf("Answer() error = %v", err)
	}

	ts.ch.WaitFor(t, "dm1", channeltest.HasText("Pushed (allow)."))
}

func TestRouterAnonymousApprovalGoesToOwnerChat(t *testing.T) {
	t.Parallel()
	ch := channeltest.NewChannel(agentdir.ChannelEmail)
	ch.Buttons = false
	ch.SendDelay = 200 * time.Millisecond
	ts := startServer(t, serverOpts{channel: ch, configure: func(c *agentdir.Config) { c.Policy.PublicAllowIncoming = agentdir.AccessAnyone }})
	linkOwner(t, ts, "me@example.com", "owner-thread")

	ts.ch.Write("stranger@example.org", "thread9", "ask")
	ts.ch.WaitFor(t, "thread9", channeltest.HasText("This needs the owner's approval"))
	_, q := ts.ch.WaitFor(t, "owner-thread", channeltest.HasText("an anonymous caller, needs your approval"))
	if !strings.Contains(q.Text, "stranger@example.org") || !strings.Contains(q.Text, "Reply approve, always, deny") {
		t.Fatalf("owner chat question = %q, want it to name the sender and how to answer", q.Text)
	}
	ts.ch.Write("me@example.com", "owner-thread", "approve")
	ts.ch.WaitFor(t, "thread9", channeltest.HasText("Pushed (allow)."))

	for _, m := range ts.ch.Transcript() {
		if m.Conv == "thread9" && strings.Contains(m.Text, "/hitl/") {
			t.Fatalf("message to the anonymous sender = %q, want no approval link", m.Text)
		}
	}
}

func TestRouterFollowUpAndNewConversation(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{channel: channeltest.NewChannel("fake")})
	linkOwner(t, ts, "telegram:1", "dm1")

	ts.ch.Write("telegram:1", "dm1", "remember red")
	ts.ch.WaitFor(t, "dm1", channeltest.HasText("OK."))
	ts.ch.Write("telegram:1", "dm1", "recall")
	ts.ch.WaitFor(t, "dm1", channeltest.HasText("You said: red"))
	ts.ch.Write("telegram:1", "dm1", "/new")
	ts.ch.WaitFor(t, "dm1", channeltest.HasText("Started a new conversation."))
	ts.ch.Write("telegram:1", "dm1", "recall")
	ts.ch.WaitFor(t, "dm1", func(m channel.Outbound) bool { return m.Text == "You said: " })
}

func TestRouterStop(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{channel: channeltest.NewChannel("fake")})
	linkOwner(t, ts, "telegram:1", "dm1")
	go ts.ch.Write("telegram:1", "dm1", "slow")
	ts.ch.WaitFor(t, "dm1", channeltest.HasText("..."))
	ts.ch.Write("telegram:1", "dm1", "/stop")
	ts.ch.WaitFor(t, "dm1", channeltest.HasText("Stopped."))
}

// linkAs links identity as name with role in conversation conv.
func linkAs(t *testing.T, ts *testServer, name, role, identity, conv string) {
	t.Helper()
	code, err := controlapi.NewClient(ts.dir).Pair(context.Background(), controlapi.PairCodeRequest{Name: name, Owner: role == auth.RoleOwner})
	if err != nil {
		t.Fatal(err)
	}
	ts.ch.Write(identity, conv, "link "+code.Code)
	ts.ch.WaitFor(t, conv, channeltest.HasText("Linked as "+role))
}

func TestRouterMemberAnswersOwnApprovalInChat(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{channel: channeltest.NewChannel("fake")})
	linkOwner(t, ts, "telegram:1", "dm1")
	linkAs(t, ts, "anna", auth.RoleAuthenticated, "telegram:2", "dm2")

	ts.ch.Write("telegram:2", "dm2", "ask")
	ref, q := ts.ch.WaitFor(t, "dm2", channeltest.HasQuestion)
	if got := ts.ch.Questions(); got != 1 {
		t.Fatalf("questions sent = %d, want only anna's: she answers her own task", got)
	}
	ts.ch.Click("telegram:2", "dm2", ref, q.Question, 0)

	ts.ch.WaitFor(t, "dm2", channeltest.HasText("Pushed (allow)."))
}

func TestRouterUnattendedApprovalGoesToOwnerChat(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{channel: channeltest.NewChannel("telegram")})
	linkOwner(t, ts, "telegram:1", "dm1")
	linkAs(t, ts, "anna", auth.RoleAuthenticated, "telegram:2", "dm2")
	ctx := context.Background()

	if err := ts.srv.chatTurns.InjectTurn(ctx, messenger.PostedTurn{ContextID: a2a.NewContextID(), User: auth.Principal{ID: "pair:anna", Role: auth.RoleAuthenticated}, Text: "ask"}); err != nil {
		t.Fatalf("injectTurn() error = %v", err)
	}
	ref, q := ts.ch.WaitFor(t, "dm1", channeltest.HasQuestion)
	if !strings.Contains(q.Text, "anna needs your approval") {
		t.Fatalf("question text = %q, want it to name anna", q.Text)
	}
	ts.ch.Click("telegram:1", "dm1", ref, q.Question, 0)
	ts.ch.WaitEdited(t, ref, "✓ Allow by owner")
}

func TestRouterChatTurnApprovalGoesToItsUser(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{channel: channeltest.NewChannel("telegram")})
	linkOwner(t, ts, "telegram:1", "dm1")
	linkAs(t, ts, "anna", auth.RoleAuthenticated, "telegram:2", "dm2")
	ctx := context.Background()
	contextID, err := ts.srv.chatTurns.ContextOf(ctx, "telegram", "dm2")
	if err != nil {
		t.Fatalf("contextOf() error = %v", err)
	}

	if err := ts.srv.chatTurns.InjectTurn(ctx, messenger.PostedTurn{ContextID: contextID, User: auth.Principal{ID: "pair:anna", Role: auth.RoleAuthenticated}, Text: "ask"}); err != nil {
		t.Fatalf("injectTurn() error = %v", err)
	}
	ref, q := ts.ch.WaitFor(t, "dm2", channeltest.HasQuestion)
	if got := ts.ch.Questions(); got != 1 {
		t.Fatalf("questions sent = %d, want only the one in anna's chat", got)
	}
	ts.ch.Click("telegram:2", "dm2", ref, q.Question, 0)
	ts.ch.WaitFor(t, "dm2", channeltest.HasText("Pushed (allow)."))
}

func TestRouterOwnerChatOrder(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{channel: channeltest.NewChannel("discord")})
	linkOwner(t, ts, "discord:1", "dm1")
	ctx := context.Background()
	if got, ok := ts.srv.chatApprovals.OwnerChat(ctx); !ok || got.Channel != "discord" {
		t.Fatalf("OwnerChat() with discord only = %+v, %v, want discord", got, ok)
	}

	tg := channeltest.NewChannel("telegram")
	ts.srv.supervisor.Add(tg)
	tg.WaitReady(t)
	tg.Write("telegram:1", "tgdm1", "join "+ownerCode(t, ts))
	tg.WaitFor(t, "tgdm1", channeltest.HasText("Linked as owner"))

	got, ok := ts.srv.chatApprovals.OwnerChat(ctx)
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
		{name: "setting naming the local pairing", channel: "telegram", ownerChat: "telegram:local", link: true, linkBen: true, want: approvals.Chat{Channel: "telegram", Conv: "dm1"}, wantOK: true},
		{name: "setting naming the local pairing without a chat", channel: "telegram", ownerChat: "telegram:local", linkBen: true},
		{name: "setting naming owner falls back to an owner pairing", channel: "telegram", ownerChat: "telegram:owner", linkBen: true, want: approvals.Chat{Channel: "telegram", Conv: "dm3"}, wantOK: true},
		{name: "messenger outside the order", channel: "fake", link: true},
		{name: "no linked owner", channel: "telegram"},
		{name: "setting of a stopped messenger", channel: "telegram", ownerChat: "email", link: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ts := startServer(t, serverOpts{channel: channeltest.NewChannel(tc.channel), configure: func(c *agentdir.Config) { c.Approvals.OwnerChat = tc.ownerChat }})
			linkAs(t, ts, "anna", auth.RoleAuthenticated, "telegram:2", "dm2")
			if tc.linkBen {
				linkAs(t, ts, "ben", auth.RoleOwner, "telegram:3", "dm3")
			}
			if tc.link {
				linkOwner(t, ts, "telegram:1", "dm1")
			}

			got, ok := ts.srv.chatApprovals.OwnerChat(context.Background())

			if ok != tc.wantOK || got != tc.want {
				t.Fatalf("OwnerChat() = %+v, %v, want %+v, %v", got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

func TestRouterHandlesConversationInOrder(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{channel: channeltest.NewChannel("fake")})
	linkOwner(t, ts, "telegram:1", "dm1")
	linkOwner(t, ts, "telegram:2", "dm2")
	inGroup := func(identity, text string) channel.Inbound {
		in := ts.ch.Message(identity, "grp", text)
		in.Direct = false
		return in
	}

	slow := inGroup("telegram:1", "remember red")
	slow.Prepare = func(context.Context, *channel.Inbound) error {
		time.Sleep(200 * time.Millisecond)
		return nil
	}
	ts.ch.Deliver(slow)
	ts.ch.Deliver(inGroup("telegram:2", "recall"))

	ts.ch.WaitFor(t, "grp", channeltest.HasText("You said: red"))
}

func TestRouterInjectedTurnWaitsForPendingApproval(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{channel: channeltest.NewChannel("fake")})
	linkOwner(t, ts, "telegram:1", "dm1")
	ts.ch.Write("telegram:1", "dm1", "ask")
	ref, q := ts.ch.WaitFor(t, "dm1", channeltest.HasQuestion)
	ctx := context.Background()
	contextID, err := ts.srv.store.ContextOf(ctx, "fake", "dm1")
	if err != nil {
		t.Fatal(err)
	}

	if err := ts.srv.chatTurns.InjectTurn(ctx, messenger.PostedTurn{ContextID: contextID, User: auth.Owner(), Text: "hello"}); err != nil {
		t.Fatalf("injectTurn() error = %v", err)
	}
	ts.ch.Click("telegram:1", "dm1", ref, q.Question, 0)

	ts.ch.WaitFor(t, "dm1", channeltest.HasText("Pushed (allow)."))
	ts.ch.WaitFor(t, "dm1", channeltest.HasText("Hello from fakeacp."))
}

func TestRouterGroupMessageDoesNotJoinOthersPendingTask(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{channel: channeltest.NewChannel("fake")})
	linkOwner(t, ts, "telegram:1", "dm1")
	linkAs(t, ts, "anna", auth.RoleAuthenticated, "telegram:2", "dm2")
	linkAs(t, ts, "bob", auth.RoleAuthenticated, "telegram:3", "dm3")
	inGroup := func(identity, text string) {
		in := ts.ch.Message(identity, "grp", text)
		in.Direct = false
		ts.ch.Deliver(in)
	}

	inGroup("telegram:2", "ask")
	ref, q := ts.ch.WaitFor(t, "grp", channeltest.HasQuestion)
	inGroup("telegram:3", "hello")
	ts.ch.WaitFor(t, "grp", channeltest.HasText("yours starts after it"))
	ts.ch.Click("telegram:3", "grp", ref, q.Question, 0)
	ts.ch.WaitFor(t, "grp", channeltest.HasText("Only the person who started the task or an owner can answer this."))
	ts.ch.Click("telegram:2", "grp", ref, q.Question, 0)

	ts.ch.WaitFor(t, "grp", channeltest.HasText("Hello from fakeacp."))
}

func TestRouterDoesNotRecordMessagesOfUnknownSenders(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{channel: channeltest.NewChannel("fake")})
	db, err := sql.Open("sqlite", ts.dir.DBPath())
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})
	stranger := ts.ch.Message("telegram:9", "dm9", "hello")
	ts.ch.Deliver(stranger)
	ts.ch.WaitFor(t, "dm9", channeltest.HasText("I don't know you yet"))
	linkOwner(t, ts, "telegram:1", "dm1")
	owner := ts.ch.Message("telegram:1", "dm1", "hello")
	ts.ch.Deliver(owner)
	ts.ch.WaitFor(t, "dm1", channeltest.HasText("Hello from fakeacp."))

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
	ts := startServer(t, serverOpts{channel: channeltest.NewChannel("fake")})
	linkOwner(t, ts, "telegram:1", "dm1")
	in := ts.ch.Message("telegram:1", "dm1", "hello")
	ts.ch.Deliver(in)
	ts.ch.WaitFor(t, "dm1", channeltest.HasText("Hello from fakeacp."))

	ts.ch.Deliver(in)
	ts.ch.Write("telegram:1", "dm1", "/start")
	ts.ch.WaitFor(t, "dm1", channeltest.HasText("Hi! I'm"))

	var replies int
	for _, m := range ts.ch.Transcript() {
		if m.Conv == "dm1" && strings.Contains(m.Text, "Hello from fakeacp.") {
			replies++
		}
	}
	if replies != 1 {
		t.Fatalf("replies to a message delivered twice = %d, want 1", replies)
	}
}
