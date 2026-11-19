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

package contact

import (
	"context"
	"slices"
	"sync"

	"github.com/a2aproject/a2a-go/v2/a2a"
)

// maxCalls bounds the calls that the in-memory records keep per
// conversation; the oldest go first.
const maxCalls = 1000

// maxConversations bounds the conversations that the in-memory records
// keep calls of; the one used least recently goes first.
const maxConversations = 1000

// memoryCalls keeps the calls of each conversation in memory.
type memoryCalls struct {
	mu sync.Mutex
	// convs are the calls of each conversation, oldest first.
	convs map[string][]memoryCall
	// order is the conversations, used least recently first.
	order []string
}

type memoryCall struct {
	turn string
	Call
}

var _ Calls = (*memoryCalls)(nil)

func newMemoryCalls() *memoryCalls {
	return &memoryCalls{convs: map[string][]memoryCall{}}
}

func (m *memoryCalls) touch(contextID string) {
	if i := slices.Index(m.order, contextID); i >= 0 {
		m.order = slices.Delete(m.order, i, i+1)
	}
	m.order = append(m.order, contextID)
	if len(m.order) > maxConversations {
		delete(m.convs, m.order[0])
		m.order = m.order[1:]
	}
}

func (m *memoryCalls) Record(_ context.Context, t Turn, c Call) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	calls := m.convs[t.ContextID]
	if slices.ContainsFunc(calls, func(mc memoryCall) bool { return mc.Contact == c.Contact && mc.TaskID == c.TaskID }) {
		return nil
	}
	calls = append(calls, memoryCall{turn: t.TaskID, Call: c})
	if len(calls) > maxCalls {
		calls = calls[len(calls)-maxCalls:]
	}
	m.convs[t.ContextID] = calls
	m.touch(t.ContextID)
	return nil
}

func (m *memoryCalls) Started(_ context.Context, t Turn, contact string, id a2a.TaskID) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.ContainsFunc(m.convs[t.ContextID], func(mc memoryCall) bool { return mc.Contact == contact && mc.TaskID == id }), nil
}

func (m *memoryCalls) InContext(_ context.Context, t Turn, contact, contextID string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.ContainsFunc(m.convs[t.ContextID], func(mc memoryCall) bool { return mc.Contact == contact && mc.ContextID == contextID }), nil
}

func (m *memoryCalls) List(_ context.Context, t Turn, contact, contextID string) ([]a2a.TaskID, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var ids []a2a.TaskID
	calls := m.convs[t.ContextID]
	for i := len(calls) - 1; i >= 0; i-- {
		if c := calls[i]; c.Contact == contact && (contextID == "" || c.ContextID == contextID) {
			ids = append(ids, c.TaskID)
		}
	}
	return ids, nil
}

func (m *memoryCalls) OfTurn(_ context.Context, t Turn) ([]Call, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Call
	for _, c := range m.convs[t.ContextID] {
		if c.turn == t.TaskID {
			out = append(out, c.Call)
		}
	}
	return out, nil
}

// endTurn forgets that the calls of t belong to a turn, so that a later
// cancel of t doesn't reach them again.
func (m *memoryCalls) endTurn(t Turn) {
	m.mu.Lock()
	defer m.mu.Unlock()
	calls := m.convs[t.ContextID]
	for i := range calls {
		if calls[i].turn == t.TaskID {
			calls[i].turn = ""
		}
	}
}
