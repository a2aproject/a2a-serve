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

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/a2aproject/a2a-serve/internal/agentsrv/approvals"
	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/channel"
	"github.com/a2aproject/a2a-serve/internal/store"
)

func TestChatApprovalsAsk(t *testing.T) {
	t.Parallel()
	const (
		header   = "coder asks"
		question = "coder asks:\nRun git push?"
	)
	buttons := []string{"Allow", "Always allow", "Deny"}
	tests := []struct {
		name    string
		buttons bool
		user    string
		member  bool
		to      approvals.Chat
		// want is the message sent; its question ID is filled in from the record.
		want channel.Outbound
		// wantPageLink is the page of the question a typed hint refers to.
		wantPageLink bool
		wantSent     bool
	}{
		{
			name:     "channel with buttons gets one button per option",
			buttons:  true,
			user:     auth.OwnerID,
			to:       approvals.Chat{Channel: "fake", Conv: "dm1"},
			want:     channel.Outbound{Conv: "dm1", Text: question, Subject: header, Question: &channel.Question{Options: buttons}},
			wantSent: true,
		},
		{
			name:         "channel without buttons gets the words to type and the page of an owner",
			user:         auth.OwnerID,
			to:           approvals.Chat{Channel: "fake", Conv: "dm1"},
			want:         channel.Outbound{Conv: "dm1", Text: question + "\n\nReply approve, always, deny, or open ", Subject: header},
			wantPageLink: true,
			wantSent:     true,
		},
		{
			name:         "channel without buttons gets the page of a member who started the task",
			user:         auth.PairPrincipal("anna"),
			member:       true,
			to:           approvals.Chat{Channel: "fake", Conv: "dm1"},
			want:         channel.Outbound{Conv: "dm1", Text: question + "\n\nReply approve, always, deny, or open ", Subject: header},
			wantPageLink: true,
			wantSent:     true,
		},
		{
			name: "channel without buttons gets no page of a user the policy doesn't know",
			user: auth.PairPrincipal("anna"),
			to:   approvals.Chat{Channel: "fake", Conv: "dm1"},
			want: channel.Outbound{Conv: "dm1", Text: question + "\n\nReply approve, always, deny", Subject: header}, wantSent: true,
		},
		{name: "channel that doesn't run", buttons: true, user: auth.OwnerID, to: approvals.Chat{Channel: "email", Conv: "dm1"}},
		{name: "chat without conversation", buttons: true, user: auth.OwnerID, to: approvals.Chat{Channel: "fake"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ch := newFakeChannel("fake")
			ch.buttons = tc.buttons
			fx := newChatFixture(t, "", ch)
			if tc.member {
				fx.link(t, linkSpec{pairing: "anna", role: auth.RoleAuthenticated, channel: "fake", identity: "telegram:2", conv: "dm2"})
			}
			rec := fx.newQuestion(t, "t1", "c1", tc.user)

			fx.chat.Ask(fx.ctx, tc.to, header, rec)

			got := ch.transcript()
			if !tc.wantSent {
				if len(got) != 0 {
					t.Fatalf("chat.Ask() sent %+v, want nothing", got)
				}
				if id, err := fx.store.PendingQuestion(fx.ctx, tc.to.Channel, string(tc.to.Conv)); !errors.Is(err, store.ErrNotFound) {
					t.Fatalf("PendingQuestion() = %q, %v, want error %v", id, err, store.ErrNotFound)
				}
				return
			}
			want := tc.want
			if want.Question != nil {
				want.Question.ID = rec.ID
			}
			if tc.wantPageLink {
				want.Text += "https://agent.example/hitl/" + rec.Token
			}
			if diff := cmp.Diff([]channel.Outbound{want}, got); diff != "" {
				t.Fatalf("chat.Ask() sent wrong result (-want +got) diff = %s", diff)
			}
			pending, err := fx.store.PendingQuestion(fx.ctx, "fake", "dm1")
			if err != nil || pending != rec.ID {
				t.Fatalf("PendingQuestion() = %q, %v, want %q, nil", pending, err, rec.ID)
			}
			msgs, err := fx.store.TakeQuestionMessages(fx.ctx, rec.ID)
			if err != nil {
				t.Fatalf("TakeQuestionMessages() error = %v", err)
			}
			wantMsgs := []store.QuestionMessage{{QuestionID: rec.ID, Channel: "fake", Conv: "dm1", Text: question}}
			if diff := cmp.Diff(wantMsgs, msgs, cmpopts.IgnoreFields(store.QuestionMessage{}, "MessageID")); diff != "" {
				t.Fatalf("TakeQuestionMessages() wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestChatApprovalsAskForgetsQuestionWhenSendFails(t *testing.T) {
	t.Parallel()
	ch := &tweakedChannel{fakeChannel: newFakeChannel("fake"), sendErr: errors.New("messenger unreachable")}
	fx := newChatFixture(t, "", ch)
	rec := fx.newQuestion(t, "t1", "c1", auth.OwnerID)

	fx.chat.Ask(fx.ctx, approvals.Chat{Channel: "fake", Conv: "dm1"}, "coder asks", rec)

	if id, err := fx.store.PendingQuestion(fx.ctx, "fake", "dm1"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("PendingQuestion() = %q, %v, want error %v", id, err, store.ErrNotFound)
	}
	msgs, err := fx.store.TakeQuestionMessages(fx.ctx, rec.ID)
	if err != nil {
		t.Fatalf("TakeQuestionMessages() error = %v", err)
	}
	if len(msgs) != 0 {
		t.Fatalf("TakeQuestionMessages() = %+v, want none", msgs)
	}
}

func TestChatApprovalsEndQuestion(t *testing.T) {
	t.Parallel()
	const asked = "coder asks:\nRun git push?"
	answered := func(ca *ChatApprovals, ctx context.Context, id string) { ca.MarkAnswered(ctx, id, "Allow", "anna") }
	closed := func(ca *ChatApprovals, ctx context.Context, id string) { ca.MarkClosed(ctx, id) }
	tests := []struct {
		name   string
		mark   func(ca *ChatApprovals, ctx context.Context, questionID string)
		noEdit bool
		want   string
	}{
		{name: "answered question tells who answered", mark: answered, want: asked + "\n\n✓ Allow by anna"},
		{name: "closed question tells that its task stopped waiting", mark: closed, want: asked + "\n\n✗ The task no longer waits for an answer."},
		{name: "channel that can't edit keeps the message", mark: answered, noEdit: true, want: asked},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ch := &tweakedChannel{fakeChannel: newFakeChannel("fake"), noEdit: tc.noEdit}
			fx := newChatFixture(t, "", ch)
			rec := fx.newQuestion(t, "t1", "c1", auth.OwnerID)
			fx.chat.Ask(fx.ctx, approvals.Chat{Channel: "fake", Conv: "dm1"}, "coder asks", rec)

			tc.mark(fx.chat, fx.ctx, rec.ID)
			tc.mark(fx.chat, fx.ctx, rec.ID)

			var got []string
			for _, m := range ch.transcript() {
				got = append(got, m.Text)
			}
			if diff := cmp.Diff([]string{tc.want}, got); diff != "" {
				t.Fatalf("messages after marking the question wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestChatApprovalsOwnerChat(t *testing.T) {
	t.Parallel()
	local := func(ch, conv string) linkSpec {
		return linkSpec{pairing: auth.LocalPairing, channel: ch, identity: "telegram:1", conv: conv}
	}
	anna := linkSpec{pairing: "anna", role: auth.RoleAuthenticated, channel: "telegram", identity: "telegram:2", conv: "dm2"}
	ben := linkSpec{pairing: "ben", role: auth.RoleOwner, channel: "telegram", identity: "telegram:3", conv: "dm3"}
	tests := []struct {
		name      string
		ownerChat string
		running   []string
		links     []linkSpec
		want      approvals.Chat
		wantOK    bool
	}{
		{name: "no setting uses the linked owner of a messenger", running: []string{"discord"}, links: []linkSpec{local("discord", "dm1")}, want: approvals.Chat{Channel: "discord", Conv: "dm1"}, wantOK: true},
		{name: "no setting tries telegram before discord", running: []string{"discord", "telegram"}, links: []linkSpec{local("discord", "dm1"), local("telegram", "tgdm1")}, want: approvals.Chat{Channel: "telegram", Conv: "tgdm1"}, wantOK: true},
		{name: "no setting skips a messenger nobody linked on", running: []string{"discord", "telegram"}, links: []linkSpec{local("discord", "dm1")}, want: approvals.Chat{Channel: "discord", Conv: "dm1"}, wantOK: true},
		{name: "no setting falls back to the chat of an owner pairing", running: []string{"telegram"}, links: []linkSpec{anna, ben}, want: approvals.Chat{Channel: "telegram", Conv: "dm3"}, wantOK: true},
		{name: "no setting prefers the user running the agent to an owner pairing", running: []string{"telegram"}, links: []linkSpec{ben, local("telegram", "dm1")}, want: approvals.Chat{Channel: "telegram", Conv: "dm1"}, wantOK: true},
		{name: "no setting ignores a messenger outside the order", running: []string{"fake"}, links: []linkSpec{local("fake", "dm1")}},
		{name: "no setting and no linked owner", running: []string{"telegram"}, links: []linkSpec{anna}},
		{name: "no setting and no messenger", links: []linkSpec{local("telegram", "dm1")}},
		{name: "setting naming a messenger uses its linked owner", ownerChat: "telegram", running: []string{"discord", "telegram"}, links: []linkSpec{local("discord", "dm1"), local("telegram", "tgdm1")}, want: approvals.Chat{Channel: "telegram", Conv: "tgdm1"}, wantOK: true},
		{name: "setting naming a messenger outside the order", ownerChat: "fake", running: []string{"fake"}, links: []linkSpec{local("fake", "dm1")}, want: approvals.Chat{Channel: "fake", Conv: "dm1"}, wantOK: true},
		{name: "setting naming owner", ownerChat: "telegram:owner", running: []string{"telegram"}, links: []linkSpec{ben, local("telegram", "dm1")}, want: approvals.Chat{Channel: "telegram", Conv: "dm1"}, wantOK: true},
		{name: "setting naming an owner pairing", ownerChat: "telegram:ben", running: []string{"telegram"}, links: []linkSpec{local("telegram", "dm1"), ben}, want: approvals.Chat{Channel: "telegram", Conv: "dm3"}, wantOK: true},
		{name: "setting naming a member", ownerChat: "telegram:anna", running: []string{"telegram"}, links: []linkSpec{local("telegram", "dm1"), anna}},
		{name: "setting naming a pairing without a chat on the messenger", ownerChat: "telegram:ben", running: []string{"telegram"}, links: []linkSpec{local("telegram", "dm1"), {pairing: "ben", role: auth.RoleOwner, channel: "discord", identity: "discord:3", conv: "dm3"}}},
		{name: "setting naming an unknown pairing", ownerChat: "telegram:nobody", running: []string{"telegram"}, links: []linkSpec{local("telegram", "dm1")}},
		{name: "setting naming a messenger that doesn't run", ownerChat: "email", running: []string{"telegram"}, links: []linkSpec{local("telegram", "dm1")}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var chs []channel.Channel
			for _, name := range tc.running {
				chs = append(chs, newFakeChannel(name))
			}
			fx := newChatFixture(t, tc.ownerChat, chs...)
			for _, l := range tc.links {
				fx.link(t, l)
			}

			got, ok := fx.chat.OwnerChat(fx.ctx)

			if ok != tc.wantOK {
				t.Fatalf("chat.OwnerChat() ok = %v, want %v", ok, tc.wantOK)
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Fatalf("chat.OwnerChat() wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestChatApprovalsFollowDelivers(t *testing.T) {
	t.Parallel()
	deliverErr := errors.New("agent unreachable")
	tests := []struct {
		name string
		// conv is the conversation of context c1; it has none if empty.
		conv      string
		runs      bool
		anonymous bool
		contact   string
		deliver   error
	}{
		{name: "context without a conversation"},
		{name: "context without a conversation fails to deliver", deliver: deliverErr},
		{name: "conversation on a channel that doesn't run", conv: "dm1"},
		{name: "question of another agent's task", conv: "dm1", runs: true, contact: "peer"},
		{name: "anonymous conversation", conv: "dm1", runs: true, anonymous: true},
		{name: "conversation fails to deliver", conv: "dm1", runs: true, deliver: deliverErr},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ch := newFakeChannel("fake")
			var chs []channel.Channel
			if tc.runs {
				chs = append(chs, ch)
			}
			fx := newChatFixture(t, "", chs...)
			fx.chat.anonymous = func(context.Context, string) bool { return tc.anonymous }
			if tc.conv != "" {
				if err := fx.store.SetContext(fx.ctx, "fake", tc.conv, "c1"); err != nil {
					t.Fatalf("SetContext() error = %v", err)
				}
			}
			rec := fx.newQuestion(t, "t1", "c1", auth.OwnerID)
			rec.Contact = tc.contact
			delivered := 0

			err := fx.chat.Follow(fx.ctx, rec, auth.Owner(), func() error {
				delivered++
				return tc.deliver
			})

			if !errors.Is(err, tc.deliver) {
				t.Fatalf("chat.Follow() error = %v, want %v", err, tc.deliver)
			}
			if delivered != 1 {
				t.Fatalf("chat.Follow() delivered %d times, want 1", delivered)
			}
		})
	}
}

func TestChatApprovalsFollowRendersTaskInConversation(t *testing.T) {
	t.Parallel()
	ch := newFakeChannel("fake")
	fx := newChatFixture(t, "", ch)
	if err := fx.store.SetContext(fx.ctx, "fake", "dm1", "c1"); err != nil {
		t.Fatalf("SetContext() error = %v", err)
	}
	rec := fx.newQuestion(t, "t1", "c1", auth.OwnerID)
	task := &a2a.Task{ID: "t1", ContextID: "c1"}

	err := fx.chat.Follow(fx.ctx, rec, auth.Owner(), func() error {
		done := a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart("Pushed."))
		fx.bus.publish("t1", a2a.NewStatusUpdateEvent(task, a2a.TaskStateCompleted, done))
		return nil
	})
	if err != nil {
		t.Fatalf("chat.Follow() error = %v", err)
	}

	ch.waitFor(t, "dm1", hasText("Pushed."))
}
