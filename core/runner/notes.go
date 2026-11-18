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

package runner

import (
	"slices"
	"strings"
	"sync"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/integrations/executor/a2aacp"
)

// Notes collects notes on the tool calls of running executions, such as
// the calls that were rejected. The agent may not mention them, so the
// result of the task does.
type Notes struct {
	mu      sync.Mutex
	running map[a2a.TaskID][]string
}

// NewNotes returns notes with no running execution.
func NewNotes() *Notes {
	return &Notes{running: map[a2a.TaskID][]string{}}
}

// Start collects the notes of an execution of a task until stop.
func (n *Notes) Start(id a2a.TaskID) (stop func()) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.running[id] = nil
	return func() {
		n.mu.Lock()
		defer n.mu.Unlock()
		delete(n.running, id)
	}
}

// Add notes a tool call of the running execution of a task.
func (n *Notes) Add(id a2a.TaskID, note string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if notes, ok := n.running[id]; ok {
		n.running[id] = append(notes, note)
	}
}

// Take returns the notes of the running execution of a task, and forgets
// them.
func (n *Notes) Take(id a2a.TaskID) []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	notes := n.running[id]
	if notes != nil {
		n.running[id] = nil
	}
	return notes
}

// WithNotes puts notes at the start of the status message of an event.
func WithNotes(execCtx *a2asrv.ExecutorContext, ev a2a.Event, notes []string) a2a.Event {
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

// IsRejection reports whether an option rejects a tool call.
func IsRejection(option string) bool {
	switch option {
	case a2aacp.OptionReject, a2aacp.OptionRejectAlways, a2aacp.OptionCancel:
		return true
	}
	return false
}

// RejectedCall shows the failure of the tool call id, which an answer
// rejected, as a rejection: the agent reports the call as failed, though
// it never ran.
func RejectedCall(ev a2a.Event, id string) a2a.Event {
	up, ok := ev.(*a2a.TaskStatusUpdateEvent)
	if !ok || up.Status.Message == nil {
		return ev
	}
	msg := up.Status.Message
	i := slices.IndexFunc(msg.Parts, func(p *a2a.Part) bool {
		data, _ := p.Data().(map[string]any)
		tool, _ := data["toolCall"].(map[string]any)
		return tool["id"] == id && tool["status"] == "failed"
	})
	if i < 0 {
		return ev
	}
	parts := slices.Clone(msg.Parts)
	for j, p := range parts {
		if text, ok := strings.CutPrefix(p.Text(), "Failed "); ok && j != i {
			parts[j] = a2a.NewTextPart("Rejected " + text)
		}
	}
	c, m := *up, *msg
	m.Parts = parts
	c.Status.Message = &m
	return &c
}
