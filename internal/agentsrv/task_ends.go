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

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/a2aproject/a2a-serve/internal/agentsrv/approvals"
	"github.com/a2aproject/a2a-serve/internal/agentsrv/chat"
)

// taskEnds does what follows the end of a task's execution, wherever it
// ends: in a2a-go, in an agent that stopped while the task waited for
// input, or in a question task that another agent stopped waiting on.
type taskEnds struct {
	bus       *chat.Bus
	approvals *approvals.Service
	// unattended is told when the task of a context ends.
	unattended *chat.ContextSet
}

func newTaskEnds(bus *chat.Bus, service *approvals.Service, unattended *chat.ContextSet) *taskEnds {
	return &taskEnds{bus: bus, approvals: service, unattended: unattended}
}

// finish publishes ev, the stored event that ended an execution outside
// a2a-go, and then ends the execution.
func (te *taskEnds) finish(ctx context.Context, ev *a2a.TaskStatusUpdateEvent) {
	te.bus.Publish(ev.TaskID, ev)
	te.ended(ctx, ev.TaskID, ev.ContextID, ev.Status.State)
}

// ended closes the questions of a task that no longer waits for input,
// forgets that nobody watches the context of a task that ended, and wakes
// the followers of the context, such as held chat turns. An empty state
// stands for an execution that failed.
func (te *taskEnds) ended(ctx context.Context, taskID a2a.TaskID, contextID string, state a2a.TaskState) {
	if state != "" && state != a2a.TaskStateInputRequired {
		te.approvals.CloseQuestions(ctx, taskID)
	}
	if state == "" || state.Terminal() {
		te.unattended.Remove(contextID)
	}
	te.bus.ExecutionEnded(contextID)
}
