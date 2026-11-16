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

// Package bridge holds what the C libraries share: tables of the objects
// that C callers hold by handle, and the queue through which a library
// asks its caller to decide something, such as an intercepted request.
package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// Handles maps the handles that C callers hold to their objects.
type Handles[T any] struct {
	mu   sync.Mutex
	next int64
	m    map[int64]T
}

// Add stores v and returns its handle, which is never 0.
func (h *Handles[T]) Add(v T) int64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.m == nil {
		h.m = map[int64]T{}
	}
	h.next++
	h.m[h.next] = v
	return h.next
}

// Reserve returns a new handle, which [Handles.Set] gives its object.
func (h *Handles[T]) Reserve() int64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.next++
	return h.next
}

// Set stores v as the object of handle.
func (h *Handles[T]) Set(handle int64, v T) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.m == nil {
		h.m = map[int64]T{}
	}
	h.m[handle] = v
}

// Get returns the object of handle.
func (h *Handles[T]) Get(handle int64) (T, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	v, ok := h.m[handle]
	return v, ok
}

// Remove forgets handle and returns its object.
func (h *Handles[T]) Remove(handle int64) (T, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	v, ok := h.m[handle]
	delete(h.m, handle)
	return v, ok
}

// ErrClosed is the answer to the questions of a closed [Calls].
var ErrClosed = errors.New("closed")

// MaxPending bounds the questions that wait for an answer.
const MaxPending = 1024

// Event is a question to the caller.
type Event struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
	// Data holds the fields of the question.
	Data any `json:"data"`
}

// Calls carries the questions of a library to its caller, which takes them
// with Next and answers each with Reply.
type Calls struct {
	queue   chan Event
	nextID  atomic.Int64
	done    chan struct{}
	once    sync.Once
	discard func(typ string, answer json.RawMessage)

	mu      sync.Mutex
	pending map[int64]chan json.RawMessage
}

// NewCalls returns an empty queue. discard, unless nil, gets the answers
// that come too late for their question, such as one that hands over a
// file descriptor that then needs closing.
func NewCalls(discard func(typ string, answer json.RawMessage)) *Calls {
	return &Calls{queue: make(chan Event, MaxPending), done: make(chan struct{}), pending: map[int64]chan json.RawMessage{}, discard: discard}
}

// Ask queues a question of typ with data and waits for its answer, until
// ctx ends or timeout passes.
func (c *Calls) Ask(ctx context.Context, typ string, data any, timeout time.Duration) (json.RawMessage, error) {
	id := c.nextID.Add(1)
	answer := make(chan json.RawMessage, 1)
	c.mu.Lock()
	if len(c.pending) >= MaxPending {
		c.mu.Unlock()
		return nil, fmt.Errorf("%d questions wait for an answer", MaxPending)
	}
	c.pending[id] = answer
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		// Reply hands answers over under the lock, so an answer is either
		// here now or refused to the caller.
		select {
		case a := <-answer:
			if c.discard != nil {
				c.discard(typ, a)
			}
		default:
		}
	}()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	select {
	case c.queue <- Event{ID: id, Type: typ, Data: data}:
	case <-c.done:
		return nil, ErrClosed
	case <-ctx.Done():
		return nil, fmt.Errorf("queueing the %s question: %w", typ, ctx.Err())
	}
	select {
	case a := <-answer:
		return a, nil
	case <-c.done:
		return nil, ErrClosed
	case <-ctx.Done():
		return nil, fmt.Errorf("waiting for the answer to the %s question: %w", typ, ctx.Err())
	}
}

// Next returns the next question, waiting up to timeout, or false when
// there is none or the queue is closed.
func (c *Calls) Next(timeout time.Duration) (Event, bool) {
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case e := <-c.queue:
		return e, true
	case <-c.done:
		return Event{}, false
	case <-t.C:
		return Event{}, false
	}
}

// Reply answers the question id. It fails when nobody waits for the
// answer, as after a timeout.
func (c *Calls) Reply(id int64, answer json.RawMessage) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch, ok := c.pending[id]
	if !ok {
		return fmt.Errorf("no question %d waits for an answer", id)
	}
	select {
	case ch <- answer:
		return nil
	default:
		return fmt.Errorf("question %d has an answer", id)
	}
}

// Close fails the questions that wait and every later one.
func (c *Calls) Close() {
	c.once.Do(func() { close(c.done) })
}

// Closed reports whether Close was called.
func (c *Calls) Closed() bool {
	select {
	case <-c.done:
		return true
	default:
		return false
	}
}
