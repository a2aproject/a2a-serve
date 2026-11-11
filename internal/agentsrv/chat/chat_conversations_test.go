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

package chat

import (
	"fmt"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/messenger"
	"github.com/a2aproject/a2a-serve/internal/messenger/messengertest"
)

const newConversationReply = "Started a new conversation. /conversations lists the earlier ones."

// putTurn stores a task of contextID that the message prompt started.
func (fx *turnsFixture) putTurn(t *testing.T, id a2a.TaskID, contextID string, prompt *a2a.Message) {
	t.Helper()
	task := &a2a.Task{ID: id, ContextID: contextID, Status: a2a.TaskStatus{State: a2a.TaskStateCompleted}, History: []*a2a.Message{prompt}}
	if _, err := fx.tasks.Create(fx.ctx, task); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
}

// startConversation sends hello in dm1 and returns the context it went to.
func (fx *turnsFixture) startConversation(t *testing.T) string {
	t.Helper()
	fx.ch.Write("telegram:1", "dm1", "hello")
	return fx.sender.nextStream(t).req.Message.ContextID
}

// newConversation sends /new in dm1 and waits for the reply that makes n
// replies to /new in all.
func (fx *turnsFixture) newConversation(t *testing.T, n int) {
	t.Helper()
	fx.ch.Write("telegram:1", "dm1", "/new")
	waitFor(t, "the reply to /new", func() bool {
		replies := 0
		for _, m := range fx.ch.Transcript() {
			if m.Text == newConversationReply {
				replies++
			}
		}
		return replies == n
	})
}

// listConversations sends /conversations in dm1 and returns the list.
func (fx *turnsFixture) listConversations(t *testing.T) (string, *messenger.Question) {
	t.Helper()
	fx.ch.Write("telegram:1", "dm1", "/conversations")
	ref, m := fx.ch.WaitFor(t, "dm1", func(m messenger.Outbound) bool {
		return m.Question != nil && m.Text == "Your recent conversations here:"
	})
	return ref, m.Question
}

func TestChatTurnsConversationsSwitchesBackAfterNew(t *testing.T) {
	t.Parallel()
	fx := newTurnsFixture(t)
	first := fx.startConversation(t)
	fx.putTurn(t, "t1", first, a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("migrate the CLI tests to the new framework")))
	fx.putTurn(t, "t2", first, a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("and the e2e ones")))
	fx.newConversation(t, 1)

	ref, q := fx.listConversations(t)

	want := []string{"Now: no messages yet", `a moment ago: 2 turns, "migrate the CLI tests to the n…"`}
	if diff := cmp.Diff(want, q.Options); diff != "" {
		t.Fatalf("/conversations buttons wrong result (-want +got) diff = %s", diff)
	}

	fx.ch.Click("telegram:1", "dm1", ref, q, 1)

	fx.ch.WaitFor(t, "dm1", messengertest.HasText(`Switched to the conversation from a moment ago: "migrate the CLI tests to the n…".`))
	if got := fx.contextOf(t, "dm1"); got != first {
		t.Fatalf("ContextOf() after switching = %q, want %q", got, first)
	}
	if got := fx.startConversation(t); got != first {
		t.Fatalf("context of a message after switching = %q, want %q", got, first)
	}
}

func TestChatTurnsConversationsShowsVoiceNote(t *testing.T) {
	t.Parallel()
	fx := newTurnsFixture(t)
	first := fx.startConversation(t)
	note := a2a.NewRawPart([]byte("OggS"))
	note.MediaType = "audio/ogg"
	fx.putTurn(t, "t1", first, a2a.NewMessage(a2a.MessageRoleUser, note))
	fx.newConversation(t, 1)
	ref, q := fx.listConversations(t)

	if got, want := q.Options[1], "a moment ago: 1 turn, voice note"; got != want {
		t.Fatalf("/conversations button = %q, want %q", got, want)
	}
	fx.ch.Click("telegram:1", "dm1", ref, q, 1)

	fx.ch.WaitFor(t, "dm1", messengertest.HasText("Switched to the conversation from a moment ago."))
}

func TestChatTurnsConversationsListsAtMostTheCap(t *testing.T) {
	t.Parallel()
	fx := newTurnsFixture(t)
	fx.startConversation(t)
	for i := range 12 {
		fx.newConversation(t, i+1)
	}

	_, q := fx.listConversations(t)

	if len(q.Options) != 10 || q.Options[0] != "Now: no messages yet" {
		t.Fatalf("/conversations buttons = %q, want 10 with the current one first", q.Options)
	}
}

func TestChatTurnsConversationsWithoutButtons(t *testing.T) {
	t.Parallel()
	fx := newTurnsFixture(t)
	fx.ch.Buttons = false
	first := fx.startConversation(t)
	fx.putTurn(t, "t1", first, a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("fix the release workflow")))
	fx.newConversation(t, 1)

	fx.ch.Write("telegram:1", "dm1", "/conversations")

	fx.ch.WaitFor(t, "dm1", messengertest.HasText("Your recent conversations here:\n1. Now: no messages yet\n"+
		`2. a moment ago: 1 turn, "fix the release workflow"`+"\nTo switch, send: /conversations <number>"))

	fx.ch.Write("telegram:1", "dm1", "/conversations 2")

	fx.ch.WaitFor(t, "dm1", messengertest.HasText(`Switched to the conversation from a moment ago: "fix the release workflow".`))
	if got := fx.contextOf(t, "dm1"); got != first {
		t.Fatalf("ContextOf() after switching = %q, want %q", got, first)
	}
}

func TestChatTurnsConversationsRefusesButton(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		// tap presses a button of the owner's list q, and returns the
		// context the chat is in after it.
		tap func(t *testing.T, fx *turnsFixture, ref string, q *messenger.Question) string
	}{
		{
			name: "of a list that changed",
			tap: func(t *testing.T, fx *turnsFixture, ref string, q *messenger.Question) string {
				fx.newConversation(t, 2)
				fx.ch.Click("telegram:1", "dm1", ref, q, 1)
				return fx.contextOf(t, "dm1")
			},
		},
		{
			name: "of someone else's list",
			tap: func(t *testing.T, fx *turnsFixture, ref string, q *messenger.Question) string {
				fx.link(t, linkSpec{pairing: "bob", role: auth.RoleMember, messenger: "fake", identity: "telegram:2", conv: "dm2"})
				fx.ch.Click("telegram:2", "dm1", ref, q, 1)
				return fx.contextOf(t, "dm1")
			},
		},
		{
			name: "past the end of the list",
			tap: func(t *testing.T, fx *turnsFixture, ref string, q *messenger.Question) string {
				fx.ch.Click("telegram:1", "dm1", ref, q, 5)
				return fx.contextOf(t, "dm1")
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fx := newTurnsFixture(t)
			fx.startConversation(t)
			fx.newConversation(t, 1)
			ref, q := fx.listConversations(t)

			want := tc.tap(t, fx, ref, q)

			fx.ch.WaitFor(t, "dm1", messengertest.HasText("This list changed or isn't yours. Send /conversations to see yours."))
			if got := fx.contextOf(t, "dm1"); got != want {
				t.Fatalf("ContextOf() after a refused tap = %q, want %q", got, want)
			}
		})
	}
}

func TestChatTurnsConversationsSwitchWaitsForRunningTurn(t *testing.T) {
	t.Parallel()
	fx := newTurnsFixture(t)
	first := fx.startConversation(t)
	fx.ch.WaitFor(t, "dm1", messengertest.HasText(agentReply))
	fx.newConversation(t, 1)
	ref, q := fx.listConversations(t)
	fx.sender.hold = make(chan struct{})
	second := fx.startConversation(t)

	fx.ch.Click("telegram:1", "dm1", ref, q, 1)

	time.Sleep(50 * time.Millisecond)
	if got := fx.contextOf(t, "dm1"); got != second {
		t.Fatalf("ContextOf() while a turn runs = %q, want %q", got, second)
	}
	close(fx.sender.hold)
	fx.ch.WaitFor(t, "dm1", messengertest.HasText("Switched to the conversation from a moment ago."))
	if got := fx.contextOf(t, "dm1"); got != first {
		t.Fatalf("ContextOf() after the turn = %q, want %q", got, first)
	}
}

func TestAgo(t *testing.T) {
	t.Parallel()
	const day = 24 * time.Hour
	tests := []struct {
		d    time.Duration
		want string
	}{
		{d: 10 * time.Second, want: "a moment ago"},
		{d: time.Minute, want: "1 minute ago"},
		{d: 5 * time.Hour, want: "5 hours ago"},
		{d: 30 * time.Hour, want: "yesterday"},
		{d: 3 * day, want: "3 days ago"},
		{d: 15 * day, want: "2 weeks ago"},
		{d: 90 * day, want: "3 months ago"},
	}
	for _, tc := range tests {
		t.Run(fmt.Sprint(tc.d), func(t *testing.T) {
			t.Parallel()
			if got := ago(tc.d); got != tc.want {
				t.Fatalf("ago(%v) = %q, want %q", tc.d, got, tc.want)
			}
		})
	}
}
