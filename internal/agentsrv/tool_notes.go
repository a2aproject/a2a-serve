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
	"iter"
	"log/slog"
	"strings"
	"sync"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/integrations/executor/a2aacp"

	"github.com/a2aproject/a2a-serve/internal/agentsrv/approvals"
	"github.com/a2aproject/a2a-serve/internal/auth"
)

// toolNotes collects notes on the tool calls of running executions, such
// as the calls that were rejected. The agent may not mention them, so the
// result of the task does.
type toolNotes struct {
	mu      sync.Mutex
	running map[a2a.TaskID][]string
}

func newToolNotes() *toolNotes {
	return &toolNotes{running: map[a2a.TaskID][]string{}}
}

// start collects the notes of an execution of a task until stop.
func (n *toolNotes) start(id a2a.TaskID) (stop func()) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.running[id] = nil
	return func() {
		n.mu.Lock()
		defer n.mu.Unlock()
		delete(n.running, id)
	}
}

// add notes a tool call of the running execution of a task.
func (n *toolNotes) add(id a2a.TaskID, note string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if notes, ok := n.running[id]; ok {
		n.running[id] = append(notes, note)
	}
}

// take returns the notes of the running execution of a task, and forgets
// them.
func (n *toolNotes) take(id a2a.TaskID) []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	notes := n.running[id]
	if notes != nil {
		n.running[id] = nil
	}
	return notes
}

// withNotes puts notes at the start of the status message of an event.
func withNotes(execCtx *a2asrv.ExecutorContext, ev a2a.Event, notes []string) a2a.Event {
	if len(notes) == 0 {
		return ev
	}
	noted := func(msg *a2a.Message) *a2a.Message {
		note := a2a.NewTextPart(strings.Join(notes, "\n"))
		if msg == nil {
			return a2a.NewMessageForTask(a2a.MessageRoleAgent, execCtx, note)
		}
		c := *msg
		c.Parts = append([]*a2a.Part{note}, msg.Parts...)
		return &c
	}
	switch e := ev.(type) {
	case *a2a.TaskStatusUpdateEvent:
		c := *e
		c.Status.Message = noted(e.Status.Message)
		return &c
	case *a2a.Task:
		c := *e
		c.Status.Message = noted(e.Status.Message)
		return &c
	}
	return ev
}

// isRejection reports whether an option rejects a tool call.
func isRejection(option string) bool {
	switch option {
	case a2aacp.OptionReject, a2aacp.OptionRejectAlways, a2aacp.OptionCancel:
		return true
	}
	return false
}

// withAnswer logs the answer that a reply gives to the approval q of the
// task, and notes it if it rejects the tool call, before events run.
func (e *observedExecutor) withAnswer(ctx context.Context, execCtx *a2asrv.ExecutorContext, q *approvals.Question, events iter.Seq2[a2a.Event, error]) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		if opt, ok := replyOption(execCtx.Message, q); ok {
			p, _ := auth.Caller(ctx)
			slog.Info("approval answered", "task", execCtx.TaskID, "by", p.ID, "kind", q.Kind, "option", opt.ID)
			switch {
			case p.ID == denyPrincipal.ID:
				e.notes.add(execCtx.TaskID, "Tool call denied by --deny-tools: "+q.Title+".")
			case isRejection(opt.ID):
				e.notes.add(execCtx.TaskID, "Tool call rejected: "+q.Title+".")
			}
		}
		for ev, err := range events {
			if !yield(ev, err) {
				return
			}
		}
	}
}
