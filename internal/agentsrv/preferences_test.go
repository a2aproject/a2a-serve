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
	"slices"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/messenger"
	"github.com/a2aproject/a2a-serve/internal/messenger/messengertest"
)

// chatSession follows the messages of one conversation of a test messenger.
type chatSession struct {
	t        *testing.T
	ch       *messengertest.Messenger
	identity string
	conv     string
	seen     map[string]bool
}

// write sends text and waits until the conversation has n messages with
// want, counting the ones before.
func (s *chatSession) write(text, want string, n int) {
	s.t.Helper()
	s.ch.Write(s.identity, s.conv, text)
	s.waitCount(want, n)
}

func (s *chatSession) waitCount(want string, n int) {
	s.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		got := 0
		for _, m := range s.ch.Transcript() {
			if string(m.Conv) == s.conv && messengertest.HasText(want)(m) {
				got++
			}
		}
		if got >= n {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	s.t.Fatalf("conversation %s has fewer than %d messages with %q; messages:\n%s", s.conv, n, want, s.ch.Dump())
}

// question waits for a message with buttons not seen before.
func (s *chatSession) question() (string, *messenger.Question) {
	s.t.Helper()
	ref, m := s.ch.WaitFor(s.t, s.conv, func(m messenger.Outbound) bool { return m.Question != nil && !s.seen[questionKey(m)] })
	s.seen[questionKey(m)] = true
	return ref, m.Question
}

func questionKey(m messenger.Outbound) string { return m.Question.ID + "|" + m.Text }

func (s *chatSession) click(ref string, q *messenger.Question, option int) {
	s.ch.Click(s.identity, s.conv, ref, q, option)
}

func TestAlwaysAllowInThreeConversationsOffersAPreference(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{messenger: messengertest.NewMessenger("fake")})
	linkOwner(t, ts, "telegram:1", "dm1")
	chat := &chatSession{t: t, ch: ts.ch, identity: "telegram:1", conv: "dm1", seen: map[string]bool{}}

	for i := 1; i <= 3; i++ {
		if i > 1 {
			chat.write("/new", "Started a new conversation.", i-1)
		}
		chat.ch.Write(chat.identity, chat.conv, "ask")
		ref, q := chat.question()
		chat.click(ref, q, 1)
		chat.waitCount("Pushed (allow).", i)
	}
	ref, offer := chat.question()
	if diff := cmp.Diff([]string{"Allow for me", "Not now"}, offer.Options); diff != "" {
		t.Fatalf("offer wrong options (-want +got) diff = %s", diff)
	}
	chat.write("hello", "Hello from fakeacp.", 1)
	linkAs(t, ts, "anna", auth.RoleAuthenticated, "telegram:2", "dm2")
	ts.ch.Click("telegram:2", "dm1", ref, offer, 0)
	chat.waitCount("can answer this.", 1)
	chat.click(ref, offer, 0)
	ts.ch.WaitEdited(t, ref, "✓ git push * runs without asking in your conversations.")

	chat.write("/new", "Started a new conversation.", 3)
	questions := ts.ch.Questions()
	chat.write("ask", "Pushed (allow).", 4)
	if got := ts.ch.Questions(); got != questions {
		t.Fatalf("questions after allowing git push for me = %d, want %d", got, questions)
	}
	saved, err := ts.dir.ReadConfig()
	if err != nil {
		t.Fatalf("ReadConfig() error = %v", err)
	}
	if len(saved.Approvals.Allow) != 0 {
		t.Fatalf("allow-tools after a preference = %q, want none", saved.Approvals.Allow)
	}

	chat.ch.Write(chat.identity, chat.conv, "/allowed")
	ref, list := chat.question()
	if diff := cmp.Diff([]string{"Remove git push *"}, list.Options); diff != "" {
		t.Fatalf("/allowed wrong buttons (-want +got) diff = %s", diff)
	}
	chat.click(ref, list, 0)
	ts.ch.WaitEdited(t, ref, "✓ Removed git push *.")
	chat.write("/new", "Started a new conversation.", 4)
	chat.ch.Write(chat.identity, chat.conv, "ask")
	chat.question()
}

func TestRejectionStartsTheCountOver(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{})
	c := ts.client(ts.pair("me", auth.RoleOwner))
	ctx := context.Background()
	for _, reply := range []string{"always", "always", "deny"} {
		task := send(t, c, userText("ask", "", ""))
		send(t, c, userText(reply, string(task.ID), task.ContextID))
	}

	if n, err := ts.srv.store.ToolTaps(ctx, "pair:me", "command:git push *"); err != nil || n != 0 {
		t.Fatalf("ToolTaps() after a rejection = %d, %v, want 0", n, err)
	}
}

func TestOwnerAnswersDontCountForOthers(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{})
	owner := ts.client(ts.pair("me", auth.RoleOwner))
	bob := ts.client(ts.pair("bob", auth.RoleAuthenticated))
	ctx := context.Background()

	asked := send(t, bob, userText("ask", "", ""))
	wantResult(t, send(t, owner, userText("always", string(asked.ID), asked.ContextID)), a2a.TaskStateCompleted, "Pushed (allow).")

	if n, err := ts.srv.store.ToolTaps(ctx, "pair:bob", "command:git push *"); err != nil || n != 0 {
		t.Fatalf("ToolTaps() of bob after the owner's answer = %d, %v, want 0", n, err)
	}
	wantResult(t, send(t, bob, userText("ask", "", asked.ContextID)), a2a.TaskStateCompleted, "Pushed (allow).")
}

func TestPreferencesCoverTheirPersonOnly(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{})
	owner := ts.client(ts.pair("me", auth.RoleOwner))
	bob := ts.client(ts.pair("bob", auth.RoleAuthenticated))
	if err := ts.srv.prefs.Allow(context.Background(), "pair:bob", "command:git push *"); err != nil {
		t.Fatalf("Preferences.Allow() error = %v", err)
	}

	wantResult(t, send(t, bob, userText("ask", "", "")), a2a.TaskStateCompleted, "Pushed (allow).")
	if task := send(t, owner, userText("ask", "", "")); task.Status.State != a2a.TaskStateInputRequired {
		t.Fatalf("owner's task state = %s, want %s", task.Status.State, a2a.TaskStateInputRequired)
	}
	got, err := ts.srv.pairings.Get(context.Background(), "bob")
	if err != nil {
		t.Fatalf("Pairings.Get() error = %v", err)
	}
	if !slices.Equal(got.Allowed, []string{"command:git push *"}) {
		t.Fatalf("Pairings.Get().Allowed = %q, want the preference", got.Allowed)
	}
}

func TestAllowedWithoutButtons(t *testing.T) {
	t.Parallel()
	ch := messengertest.NewMessenger("fake")
	ch.Buttons = false
	ts := startServer(t, serverOpts{messenger: ch})
	linkOwner(t, ts, "me@example.com", "thread1")
	chat := &chatSession{t: t, ch: ts.ch, identity: "me@example.com", conv: "thread1", seen: map[string]bool{}}
	chat.write("/allowed", "Nothing runs without asking in your conversations yet.", 1)
	if err := ts.srv.prefs.Allow(context.Background(), auth.OwnerID, "command:git push *"); err != nil {
		t.Fatalf("Preferences.Allow() error = %v", err)
	}

	chat.write("/allowed", "- git push *\nTo remove one, send: /allowed remove <one of them>", 1)
	chat.write("/allowed remove git push *", "✓ Removed git push *.", 1)

	if rules, err := ts.srv.prefs.List(context.Background(), auth.OwnerID); err != nil || len(rules) != 0 {
		t.Fatalf("Preferences.List() after /allowed remove = %q, %v, want none", rules, err)
	}
}
