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
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/messenger"
	"github.com/a2aproject/a2a-serve/internal/store"
)

// PostedTurn is a turn the server posts into a context: a scheduled run or
// the result of a watched task.
type PostedTurn struct {
	ContextID string
	User      auth.Principal
	Text      string
	// Files are attached to the turn's message, which keeps them in the
	// blob store with its task.
	Files []messenger.File
	// Key names the source of repeated turns, such as a schedule. A turn
	// posted while another with the same key waits in the context is merged
	// into it.
	Key string
	// Accepted, if set, is called once the agent accepted the turn, or with
	// the error it refused it with. It isn't called if the server stops
	// before the turn is sent.
	Accepted func(error)
	// Quiet keeps a refusal out of the conversation's chat, for a turn
	// posted again after the agent refused it.
	Quiet bool
	// GroupChat sends the turn as one from a group chat, such as the run of
	// a schedule the agent made in one.
	GroupChat bool
}

// Parts are the message parts of the turn.
func (t *PostedTurn) Parts() []*a2a.Part {
	return messageParts([]*waitingMessage{{in: messenger.Inbound{Text: t.Text, Files: t.Files}}})
}

// Accept tells the poster of t that the agent accepted it, or err.
func (t *PostedTurn) Accept(err error) {
	if t.Accepted != nil {
		t.Accepted(err)
	}
}

// errTurnDropped is what a posted turn removed before it was sent is
// accepted with.
var errTurnDropped = errors.New("the turn was dropped before it was sent")

// heldTurn is a posted turn waiting in its context's line.
type heldTurn struct {
	PostedTurn
	// merged counts the later turns with the same key merged into it.
	merged   int
	accepted []func(error)
}

func (t *heldTurn) accept(err error) {
	for _, f := range t.accepted {
		f(err)
	}
}

// text is the turn's text, with a note on the turns merged into it.
func (t *heldTurn) text() string {
	if t.merged == 0 {
		return t.Text
	}
	return fmt.Sprintf("%s\n\n(Runs skipped while this one waited for an answer in this conversation: %d.)", t.Text, t.merged)
}

// postLine is the line of turns posted into one context. One goroutine
// sends them in order.
type postLine struct {
	turns []*heldTurn
	// wake has a value when turns were dropped from the line.
	wake chan struct{}
}

// PostedTurns are the lines of the contexts that have posted turns.
type PostedTurns struct {
	mu    sync.Mutex
	lines map[string]*postLine
}

// add puts t in its context's line, or merges it into a waiting turn with
// the same key. It returns the line if it is new and needs a goroutine.
func (p *PostedTurns) add(t PostedTurn) (*postLine, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	line, ok := p.lines[t.ContextID]
	if ok && t.Key != "" {
		if i := slices.IndexFunc(line.turns, func(h *heldTurn) bool { return h.Key == t.Key }); i >= 0 {
			line.turns[i].merged++
			if t.Accepted != nil {
				line.turns[i].accepted = append(line.turns[i].accepted, t.Accepted)
			}
			return nil, false
		}
	}
	if !ok {
		if p.lines == nil {
			p.lines = map[string]*postLine{}
		}
		line = &postLine{wake: make(chan struct{}, 1)}
		p.lines[t.ContextID] = line
	}
	h := &heldTurn{PostedTurn: t}
	if t.Accepted != nil {
		h.accepted = []func(error){t.Accepted}
	}
	line.turns = append(line.turns, h)
	return line, !ok
}

// next takes the first turn out of a context's line. When the line is
// empty it is removed, and next returns false.
func (p *PostedTurns) next(contextID string) (*heldTurn, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	line := p.lines[contextID]
	if line == nil {
		return nil, false
	}
	if len(line.turns) == 0 {
		delete(p.lines, contextID)
		return nil, false
	}
	t := line.turns[0]
	line.turns = line.turns[1:]
	return t, true
}

// empty reports whether a context's line has no turns.
func (p *PostedTurns) empty(contextID string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	line := p.lines[contextID]
	return line == nil || len(line.turns) == 0
}

// drop removes the waiting turns with key from every line.
func (p *PostedTurns) drop(key string) {
	p.dropWhere(func(h *heldTurn) bool { return h.Key == key })
}

// DropUsers removes the waiting turns posted as one of the principals.
func (p *PostedTurns) DropUsers(principals []string) {
	p.dropWhere(func(h *heldTurn) bool { return slices.Contains(principals, h.User.ID) })
}

func (p *PostedTurns) dropWhere(match func(*heldTurn) bool) {
	var dropped []*heldTurn
	p.mu.Lock()
	for _, line := range p.lines {
		n := len(line.turns)
		line.turns = slices.DeleteFunc(line.turns, func(h *heldTurn) bool {
			if !match(h) {
				return false
			}
			dropped = append(dropped, h)
			return true
		})
		if len(line.turns) != n {
			select {
			case line.wake <- struct{}{}:
			default:
			}
		}
	}
	p.mu.Unlock()
	for _, h := range dropped {
		h.accept(errTurnDropped)
	}
}

// InjectTurn posts a turn into its context, rendered in the context's
// conversation if it has one. Turns of a context are sent one at a time,
// in order. While the context's latest task waits for input, they are held
// back: text sent to that task would interrupt it as if it were the
// answer. They are sent once an execution in the context ends with the
// task in another state.
func (c *ChatTurns) InjectTurn(ctx context.Context, t PostedTurn) error {
	chName, conv, err := c.store.ConvOf(ctx, t.ContextID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	line, isNew := c.posted.add(t)
	if line == nil {
		slog.Info("merged a turn into the one waiting in its context", "context", t.ContextID, "key", t.Key)
		return nil
	}
	if isNew {
		held := c.holdsTurns(ctx, t.ContextID, false)
		go c.post(ctx, t.ContextID, line, chName, messenger.ConvKey(conv), held)
	}
	return nil
}

// DropPosted removes the turns with key that wait to be posted, such as
// the runs of a removed schedule.
func (c *ChatTurns) DropPosted(key string) { c.posted.drop(key) }

// post sends the turns of a context's line until it is empty or ctx ends.
// held tells whether the first turn was posted while the context's latest
// task waited for input.
func (c *ChatTurns) post(ctx context.Context, contextID string, line *postLine, chName string, conv messenger.ConvKey, held bool) {
	for {
		unlock, ok := c.awaitTurn(ctx, contextID, line, chName, conv, held)
		if !ok {
			return
		}
		t, ok := c.posted.next(contextID)
		if !ok {
			unlock()
			return
		}
		c.send(ctx, t, chName, conv)
		unlock()
		held = false
	}
}

// awaitTurn waits until the context's latest task holds turns no longer,
// holding the conversation if the context has one. It reports false when
// ctx ends.
func (c *ChatTurns) awaitTurn(ctx context.Context, contextID string, line *postLine, chName string, conv messenger.ConvKey, held bool) (func(), bool) {
	for {
		changed, unsubscribe := c.bus.SubscribeContext(contextID)
		unlock := func() {}
		if chName != "" {
			unlock = c.queue.lock(chName, conv)
		}
		held = c.holdsTurns(ctx, contextID, held)
		if !held || c.posted.empty(contextID) {
			unsubscribe()
			return unlock, true
		}
		unlock()
		select {
		case <-ctx.Done():
			unsubscribe()
			return nil, false
		case <-changed:
		case <-line.wake:
		}
		unsubscribe()
	}
}

func (c *ChatTurns) send(ctx context.Context, t *heldTurn, chName string, conv messenger.ConvKey) {
	msg := a2a.NewMessage(a2a.MessageRoleUser, messageParts([]*waitingMessage{{in: messenger.Inbound{Text: t.text(), Files: t.Files}}})...)
	msg.ContextID = t.ContextID
	ctx = auth.Posted(ctx)
	if t.GroupChat {
		ctx = auth.InGroupChat(ctx)
	}
	ch := c.supervisor.Messenger(chName)
	if ch == nil {
		c.unattended.Add(t.ContextID)
		_, err := c.sender.SendMessage(auth.InProcess(ctx, t.User), &a2a.SendMessageRequest{Message: msg, Config: &a2a.SendMessageConfig{ReturnImmediately: true}})
		if err != nil {
			slog.Error("posting a turn", "context", t.ContextID, "error", err)
		}
		t.accept(err)
		return
	}
	c.streamThen(ctx, ch, conv, messenger.Ref{}, t.User, &a2a.SendMessageRequest{Message: msg}, t.Quiet, t.accept)
}

// holdsTurns reports whether the context's latest task holds back posted
// turns: while it waits for input, and, for turns it held already, while it
// runs. The execution that left the task waiting for input may have resumed
// by the time its end wakes the turns.
func (c *ChatTurns) holdsTurns(ctx context.Context, contextID string, held bool) bool {
	_, state, err := c.tasks.LatestStateInContext(ctx, contextID)
	if err != nil {
		return false
	}
	switch state {
	case a2a.TaskStateInputRequired:
		return true
	case a2a.TaskStateSubmitted, a2a.TaskStateWorking:
		return held
	default:
		return false
	}
}
