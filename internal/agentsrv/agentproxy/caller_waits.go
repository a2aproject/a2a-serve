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

package agentproxy

import (
	"log/slog"
	"sync"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-serve/internal/agentsrv/messenger"
	"github.com/a2aproject/a2a-serve/internal/store"
)

// CallerQuestion is a permission question of another agent's task, which
// the A2A caller of the turn that called the agent answers on its own task.
type CallerQuestion struct {
	Rec    store.Question
	Header string
	// Msg is the other agent's status message, with its input request.
	Msg *a2a.Message
}

// CallerOutcome is what a task waiting for another agent gets next: that
// agent's next question, or its result.
type CallerOutcome struct {
	Question *CallerQuestion
	Result   *messenger.PostedTurn
}

// callerWait is a context whose A2A caller waits for its task: the question
// to ask when the turn ends, or, while the task waits for another agent
// after an answer, where that agent's next question or result goes.
type callerWait struct {
	question *CallerQuestion
	outcomes chan CallerOutcome
}

// Attend notes that the A2A caller of the turn running in contextID waits
// for it, so that the questions of the agents it calls are asked on its
// task. The returned func ends that and returns the question to ask; with
// ask false, a relayed question goes to the user's chats instead.
func (ws *Watches) Attend(contextID string) func(ask bool) *CallerQuestion {
	ws.mu.Lock()
	if ws.callers[contextID] != nil {
		ws.mu.Unlock()
		return func(bool) *CallerQuestion { return nil }
	}
	cw := &callerWait{}
	ws.callers[contextID] = cw
	ws.mu.Unlock()
	var once sync.Once
	var q *CallerQuestion
	return func(ask bool) *CallerQuestion {
		once.Do(func() {
			ws.mu.Lock()
			if ws.callers[contextID] == cw {
				delete(ws.callers, contextID)
			}
			q = cw.question
			ws.mu.Unlock()
			if q != nil && !ask {
				ws.askInChat(ws.ctx, contextID, q)
				q = nil
			}
		})
		return q
	}
}

// Await registers a task in contextID which waits for another agent after
// its caller answered that agent's question. The returned func ends the
// wait; an outcome which arrived and wasn't read goes where it goes when no
// caller waits.
func (ws *Watches) Await(contextID string) (<-chan CallerOutcome, func()) {
	cw := &callerWait{outcomes: make(chan CallerOutcome, 1)}
	ws.mu.Lock()
	ws.callers[contextID] = cw
	ws.mu.Unlock()
	return cw.outcomes, func() {
		ws.mu.Lock()
		if ws.callers[contextID] == cw {
			delete(ws.callers, contextID)
		}
		ws.mu.Unlock()
		select {
		case o := <-cw.outcomes:
			ws.redirect(contextID, o)
		default:
		}
	}
}

// redirect sends an outcome no caller read in contextID to the user's
// chats, or posts a result into its conversation.
func (ws *Watches) redirect(contextID string, o CallerOutcome) {
	if o.Question != nil {
		ws.askInChat(ws.ctx, contextID, o.Question)
		return
	}
	if err := ws.turns.InjectTurn(ws.ctx, *o.Result); err != nil {
		slog.Error("posting a watched result", "context", o.Result.ContextID, "error", err)
		o.Result.Accept(err)
	}
}

// awaited reports whether a task in contextID waits for another agent
// after its caller answered that agent's question.
func (ws *Watches) awaited(contextID string) bool {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	cw := ws.callers[contextID]
	return cw != nil && cw.outcomes != nil
}

// toCaller hands a question to the A2A caller waiting in contextID, and
// reports whether one took it.
func (ws *Watches) toCaller(contextID string, q *CallerQuestion) bool {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	cw := ws.callers[contextID]
	switch {
	case cw == nil:
		return false
	case cw.outcomes != nil:
		select {
		case cw.outcomes <- CallerOutcome{Question: q}:
			return true
		default:
			return false
		}
	case cw.question != nil:
		return false
	}
	cw.question = q
	return true
}

// resultToCaller hands a watched result to a task in its context which
// waits for it, and reports whether one took it.
func (ws *Watches) resultToCaller(t messenger.PostedTurn) bool {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	cw := ws.callers[t.ContextID]
	if cw == nil || cw.outcomes == nil {
		return false
	}
	select {
	case cw.outcomes <- CallerOutcome{Result: &t}:
		return true
	default:
		return false
	}
}
