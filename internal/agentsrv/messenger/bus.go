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

package messenger

import (
	"context"
	"sync"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
)

// Bus fans out task events to in-process followers, such as a channel
// conversation waiting for a task someone else answered. Followers never
// miss events: a slow follower's events queue up. Followers of a context
// learn when an execution in it ended.
type Bus struct {
	mu       sync.Mutex
	subs     map[a2a.TaskID]map[*Subscription]struct{}
	contexts map[string]map[chan struct{}]struct{}
}

// Subscription is the queue of events of one follower. ready has a value
// when events are queued.
type Subscription struct {
	mu     sync.Mutex
	events []a2a.Event
	ready  chan struct{}
}

// Next returns the next event, or false when ctx ends or done is closed.
func (s *Subscription) Next(ctx context.Context, done <-chan time.Time) (a2a.Event, bool) {
	for {
		s.mu.Lock()
		if len(s.events) > 0 {
			ev := s.events[0]
			s.events = s.events[1:]
			s.mu.Unlock()
			return ev, true
		}
		s.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, false
		case <-done:
			return nil, false
		case <-s.ready:
		}
	}
}

func (s *Subscription) push(ev a2a.Event) {
	s.mu.Lock()
	s.events = append(s.events, ev)
	s.mu.Unlock()
	select {
	case s.ready <- struct{}{}:
	default:
	}
}

// NewBus returns a bus without followers.
func NewBus() *Bus {
	return &Bus{subs: map[a2a.TaskID]map[*Subscription]struct{}{}, contexts: map[string]map[chan struct{}]struct{}{}}
}

// Subscribe follows the events of the task id until the returned function
// is called.
func (b *Bus) Subscribe(id a2a.TaskID) (*Subscription, func()) {
	sub := &Subscription{ready: make(chan struct{}, 1)}
	b.mu.Lock()
	if b.subs[id] == nil {
		b.subs[id] = map[*Subscription]struct{}{}
	}
	b.subs[id][sub] = struct{}{}
	b.mu.Unlock()
	return sub, func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		delete(b.subs[id], sub)
		if len(b.subs[id]) == 0 {
			delete(b.subs, id)
		}
	}
}

// Publish hands an event of the task id to its followers.
func (b *Bus) Publish(id a2a.TaskID, ev a2a.Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for sub := range b.subs[id] {
		sub.push(ev)
	}
}

// SubscribeContext returns a channel that has a value once an execution in
// the context ended after the call.
func (b *Bus) SubscribeContext(contextID string) (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	b.mu.Lock()
	if b.contexts[contextID] == nil {
		b.contexts[contextID] = map[chan struct{}]struct{}{}
	}
	b.contexts[contextID][ch] = struct{}{}
	b.mu.Unlock()
	return ch, func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		delete(b.contexts[contextID], ch)
		if len(b.contexts[contextID]) == 0 {
			delete(b.contexts, contextID)
		}
	}
}

// ExecutionEnded tells the followers of a context that an execution in it
// ended.
func (b *Bus) ExecutionEnded(contextID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.contexts[contextID] {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}
