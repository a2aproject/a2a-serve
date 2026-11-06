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
	"fmt"
	"iter"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
)

// withFinishedSubscribeRefused returns h, whose SubscribeToTask refuses a
// finished task whether or not its execution is still registered.
func withFinishedSubscribeRefused(h a2asrv.RequestHandler) a2asrv.RequestHandler {
	return &finishedSubscribeRefused{RequestHandler: h}
}

type finishedSubscribeRefused struct {
	a2asrv.RequestHandler
}

// SubscribeToTask subscribes to the events of a running task. a2a-go keeps
// an execution registered for a moment after it stored the final event, and
// a subscription in that moment starts with the finished task; a2a-go
// refuses a finished task once the execution is gone.
func (f *finishedSubscribeRefused) SubscribeToTask(ctx context.Context, req *a2a.SubscribeToTaskRequest) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		first := true
		for ev, err := range f.RequestHandler.SubscribeToTask(ctx, req) {
			if task, ok := ev.(*a2a.Task); ok && first && err == nil && task.Status.State.Terminal() {
				yield(nil, fmt.Errorf("task in a terminal state %q: %w", task.Status.State, a2a.ErrUnsupportedOperation))
				return
			}
			first = false
			if !yield(ev, err) {
				return
			}
		}
	}
}
