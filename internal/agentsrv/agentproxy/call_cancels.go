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
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-serve/internal/store"
)

// cancelCallsTimeout bounds how long canceling the calls of a task takes.
const cancelCallsTimeout = 10 * time.Second

// CancelCalls cancels the tasks that the turn of the task of claims started
// at other agents, and stops watching them, so that their results aren't
// posted into the conversation. A task that it can't cancel stays watched,
// and its result still comes back.
func (ws *Watches) CancelCalls(ctx context.Context, claims Claims) error {
	calls, err := ws.store.CallTasksOf(ctx, claims.ContextID, claims.TaskID)
	if err != nil {
		return fmt.Errorf("listing the calls of task %s: %w", claims.TaskID, err)
	}
	ctx, cancel := context.WithTimeout(ctx, cancelCallsTimeout)
	defer cancel()
	errs := make([]error, len(calls))
	var wg sync.WaitGroup
	for i, c := range calls {
		wg.Go(func() { errs[i] = ws.cancelCall(ctx, claims, c) })
	}
	wg.Wait()
	return errors.Join(errs...)
}

// cancelCall cancels one call task and drops its watch. A task that already
// ended, or that the agent no longer has, counts as canceled.
func (ws *Watches) cancelCall(ctx context.Context, claims Claims, c store.CallTask) error {
	t, err := ws.clients.target(ctx, c.Agent, claims.User, claims.User.Hops)
	if err == nil {
		err = t.cancel(ctx, a2a.TaskID(c.TaskID))
	}
	if err != nil && !errors.Is(err, a2a.ErrTaskNotCancelable) && !errors.Is(err, a2a.ErrTaskNotFound) {
		return fmt.Errorf("canceling task %s at %s: %w", c.TaskID, c.Agent, err)
	}
	w, err := ws.store.Watch(ctx, c.Agent, c.TaskID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading the watch of task %s at %s: %w", c.TaskID, c.Agent, err)
	}
	slog.Info("stopped watching a canceled call", "agent", c.Agent, "task", c.TaskID)
	ws.deleteWatch(ctx, w)
	return nil
}
