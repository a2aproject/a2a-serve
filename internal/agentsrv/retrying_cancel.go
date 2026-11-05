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
	"errors"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
)

const cancelAttempts = 3

// withRetryingCancel returns h, whose CancelTask retries a cancel that lost a
// race with the end of the task's execution.
func withRetryingCancel(h a2asrv.RequestHandler) a2asrv.RequestHandler {
	return &retryingCancel{RequestHandler: h}
}

type retryingCancel struct {
	a2asrv.RequestHandler
}

// CancelTask cancels the task. The handler sends a cancel of a running task
// to its execution, and fails it when the execution ends first in a state
// that isn't terminal, such as input-required. The execution is gone by
// then, so another attempt cancels the stored task.
func (r *retryingCancel) CancelTask(ctx context.Context, req *a2a.CancelTaskRequest) (*a2a.Task, error) {
	var err error
	for range cancelAttempts {
		var task *a2a.Task
		task, err = r.RequestHandler.CancelTask(ctx, req)
		if !errors.Is(err, a2a.ErrTaskNotCancelable) {
			return task, err
		}
	}
	return nil, err
}
