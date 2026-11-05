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
	"slices"
	"sync"

	"github.com/a2aproject/a2a-serve/internal/channel"
)

// ConversationQueue orders the turns of each conversation: one turn holds
// a conversation at a time, in the order they took their place in line.
type ConversationQueue struct {
	mu    sync.Mutex
	convs map[string][]*convTicket
}

func newConversationQueue() *ConversationQueue {
	return &ConversationQueue{convs: map[string][]*convTicket{}}
}

// convTicket is a place in the line of a conversation. turn is closed when
// the ticket is first in line, or when a turn ahead took its message.
type convTicket struct {
	key  string
	turn chan struct{}
	once sync.Once
	// msg is the message the ticket waits with, and taken is set when a
	// turn ahead took it. The queue's mu guards both.
	msg   *waitingMessage
	taken bool
}

// enqueue puts a ticket at the end of the line of a conversation.
func (q *ConversationQueue) enqueue(ch string, conv channel.ConvKey) *convTicket {
	t := &convTicket{key: ch + "\x00" + string(conv), turn: make(chan struct{})}
	q.mu.Lock()
	defer q.mu.Unlock()
	q.convs[t.key] = append(q.convs[t.key], t)
	if len(q.convs[t.key]) == 1 {
		close(t.turn)
	}
	return t
}

// release takes a ticket out of line, wherever it is, and lets the next one
// go if it was first. Releasing twice is a no-op.
func (q *ConversationQueue) release(t *convTicket) {
	t.once.Do(func() {
		q.mu.Lock()
		defer q.mu.Unlock()
		line := q.convs[t.key]
		i := slices.Index(line, t)
		if i < 0 {
			return
		}
		line = slices.Delete(line, i, i+1)
		if len(line) == 0 {
			delete(q.convs, t.key)
			return
		}
		q.convs[t.key] = line
		if i == 0 {
			close(line[0].turn)
		}
	})
}

// waitTurn waits for the turn of t with message m. It returns m and the
// messages waiting right behind it that m joins, which leave the line, so
// that messages that arrived during a turn reach the agent in one turn. It
// returns nil if a turn ahead took m.
func (q *ConversationQueue) waitTurn(t *convTicket, m *waitingMessage) []*waitingMessage {
	q.mu.Lock()
	t.msg = m
	q.mu.Unlock()
	<-t.turn
	q.mu.Lock()
	defer q.mu.Unlock()
	t.msg = nil
	if t.taken {
		return nil
	}
	batch := []*waitingMessage{m}
	line := q.convs[t.key]
	if len(line) == 0 || line[0] != t {
		return batch
	}
	end := 1
	for end < len(line) && line[end].msg != nil && m.joins(line[end].msg) {
		next := line[end]
		batch = append(batch, next.msg)
		next.msg, next.taken = nil, true
		close(next.turn)
		end++
	}
	q.convs[t.key] = slices.Delete(line, 1, end)
	return batch
}

// lock serializes the handling of one conversation's messages in the
// order they lock it. The returned function unlocks it.
func (q *ConversationQueue) lock(ch string, conv channel.ConvKey) func() {
	t := q.enqueue(ch, conv)
	<-t.turn
	return func() { q.release(t) }
}

// busy reports whether a turn holds or waits for the conversation.
func (q *ConversationQueue) busy(ch string, conv channel.ConvKey) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.convs[ch+"\x00"+string(conv)]) > 0
}
