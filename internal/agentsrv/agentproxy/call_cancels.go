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
	"log/slog"

	"github.com/a2aproject/a2a-serve/internal/store"
)

// dropCanceled stops watching a canceled call task, so that its result
// isn't posted into the conversation.
func (ws *Watches) dropCanceled(ctx context.Context, agent, taskID string) {
	w, err := ws.store.Watch(ctx, agent, taskID)
	if errors.Is(err, store.ErrNotFound) {
		return
	}
	if err != nil {
		slog.Warn("reading the watch of a canceled call", "agent", agent, "task", taskID, "error", err)
		return
	}
	slog.Info("stopped watching a canceled call", "agent", agent, "task", taskID)
	ws.deleteWatch(ctx, w)
}
