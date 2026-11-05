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
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/channel"
)

func hasTurn(t *convTicket) bool {
	select {
	case <-t.turn:
		return true
	default:
		return false
	}
}

func TestConversationQueueReleaseHandsTurnOver(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		tickets  int
		release  []int
		wantTurn []int
		wantBusy bool
	}{
		{name: "first in line holds the conversation", tickets: 3, wantTurn: []int{0}, wantBusy: true},
		{name: "releasing the first hands over to the second", tickets: 3, release: []int{0}, wantTurn: []int{0, 1}, wantBusy: true},
		{name: "releasing the last hands over to nobody", tickets: 3, release: []int{2}, wantTurn: []int{0}, wantBusy: true},
		{name: "releasing from the middle lets the first hand over to the third", tickets: 3, release: []int{1, 0}, wantTurn: []int{0, 2}, wantBusy: true},
		{name: "releasing twice is a no-op", tickets: 3, release: []int{0, 0}, wantTurn: []int{0, 1}, wantBusy: true},
		{name: "releasing everything frees the conversation", tickets: 3, release: []int{0, 1, 2}, wantTurn: []int{0, 1, 2}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			q := newConversationQueue()
			tickets := make([]*convTicket, tc.tickets)
			for i := range tickets {
				tickets[i] = q.enqueue("fake", "dm1")
			}

			for _, i := range tc.release {
				q.release(tickets[i])
			}

			var gotTurn []int
			for i, tk := range tickets {
				if hasTurn(tk) {
					gotTurn = append(gotTurn, i)
				}
			}
			if diff := cmp.Diff(tc.wantTurn, gotTurn); diff != "" {
				t.Fatalf("tickets with a turn after release(%v) wrong result (-want +got) diff = %s", tc.release, diff)
			}
			if got := q.busy("fake", "dm1"); got != tc.wantBusy {
				t.Fatalf("q.busy() = %v, want %v", got, tc.wantBusy)
			}
		})
	}
}

func TestConversationQueueKeepsConversationsApart(t *testing.T) {
	t.Parallel()
	q := newConversationQueue()
	first := q.enqueue("fake", "dm1")
	otherConv := q.enqueue("fake", "dm2")
	otherChannel := q.enqueue("other", "dm1")

	for name, tk := range map[string]*convTicket{"first": first, "other conversation": otherConv, "other channel": otherChannel} {
		if !hasTurn(tk) {
			t.Errorf("%s has a turn = false, want true", name)
		}
	}
	q.release(first)
	if got := q.busy("fake", "dm1"); got {
		t.Errorf("q.busy(fake, dm1) = %v, want false", got)
	}
	if got := q.busy("fake", "dm2"); !got {
		t.Errorf("q.busy(fake, dm2) = %v, want true", got)
	}
	if got := q.busy("other", "dm1"); !got {
		t.Errorf("q.busy(other, dm1) = %v, want true", got)
	}
	if got := q.busy("fake", "unknown"); got {
		t.Errorf("q.busy(fake, unknown) = %v, want false", got)
	}
}

func TestConversationQueueLock(t *testing.T) {
	t.Parallel()
	q := newConversationQueue()
	unlock := q.lock("fake", "dm1")

	acquired := make(chan func())
	go func() { acquired <- q.lock("fake", "dm1") }()
	waitFor(t, "the second lock to take its place in line", func() bool {
		q.mu.Lock()
		defer q.mu.Unlock()
		return len(q.convs["fake\x00dm1"]) == 2
	})
	select {
	case <-acquired:
		t.Fatal("lock() of a held conversation returned before it was unlocked")
	case <-time.After(20 * time.Millisecond):
	}

	unlock()

	select {
	case unlockSecond := <-acquired:
		unlockSecond()
	case <-time.After(waitTimeout):
		t.Fatal("lock() did not return after the conversation was unlocked")
	}
	if got := q.busy("fake", "dm1"); got {
		t.Fatalf("q.busy() after unlocking everything = %v, want false", got)
	}
}

func TestConversationQueueLockDoesNotBlockOtherConversations(t *testing.T) {
	t.Parallel()
	q := newConversationQueue()
	unlock := q.lock("fake", "dm1")
	defer unlock()

	done := make(chan struct{})
	go func() {
		q.lock("fake", "dm2")()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(waitTimeout):
		t.Fatal("lock() of an idle conversation blocked on another conversation")
	}
}

// waitingMessages counts the messages that wait in the line of a
// conversation.
func waitingMessages(q *ConversationQueue, ch string, conv channel.ConvKey) int {
	q.mu.Lock()
	defer q.mu.Unlock()
	n := 0
	for _, tk := range q.convs[ch+"\x00"+string(conv)] {
		if tk.msg != nil {
			n++
		}
	}
	return n
}

func waitingFrom(identity, text string) *waitingMessage {
	return &waitingMessage{in: channel.Inbound{Identity: identity, Text: text}, p: auth.Principal{ID: identity}}
}

func TestConversationQueueWaitTurnTakesMessagesOfTheSameSenderWaitingBehind(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		// line is who sent the messages behind the running turn; "" holds
		// a place without a message yet.
		line []string
		want [][]string
	}{
		{name: "messages of one sender go together", line: []string{"anna", "anna", "anna"}, want: [][]string{{"0", "1", "2"}}},
		{name: "another sender starts a turn of their own", line: []string{"anna", "anna", "bob", "anna"}, want: [][]string{{"0", "1"}, {"2"}, {"3"}}},
		{name: "a place without a message keeps the order", line: []string{"anna", "", "anna"}, want: [][]string{{"0"}, {"2"}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			q := newConversationQueue()
			running := q.enqueue("fake", "dm1")
			turns := make(chan []string, len(tc.line))
			waiting := 0
			var places []*convTicket
			for i, sender := range tc.line {
				tk := q.enqueue("fake", "dm1")
				if sender == "" {
					places = append(places, tk)
					continue
				}
				waiting++
				m := waitingFrom(sender, string(rune('0'+i)))
				go func() {
					batch := q.waitTurn(tk, m)
					if batch == nil {
						return
					}
					var texts []string
					for _, b := range batch {
						texts = append(texts, b.in.Text)
					}
					turns <- texts
					q.release(tk)
				}()
			}
			waitFor(t, "the messages to wait", func() bool { return waitingMessages(q, "fake", "dm1") == waiting })

			q.release(running)

			var got [][]string
			for range tc.want {
				select {
				case turn := <-turns:
					got = append(got, turn)
					for _, tk := range places {
						q.release(tk)
					}
				case <-time.After(waitTimeout):
					t.Fatalf("turns = %v, want %v", got, tc.want)
				}
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Fatalf("q.waitTurn() turns wrong result (-want +got) diff = %s", diff)
			}
			waitFor(t, "the conversation to be free", func() bool { return !q.busy("fake", "dm1") })
		})
	}
}
