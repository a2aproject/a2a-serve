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
	"github.com/a2aproject/a2a-go/v2/a2asrv"
)

// withLimitedHistory returns h, whose SendMessage returns at most the history
// length the request asks for, as GetTask does. a2a-go returns the whole
// history there.
func withLimitedHistory(h a2asrv.RequestHandler) a2asrv.RequestHandler {
	return &limitedHistory{RequestHandler: h}
}

type limitedHistory struct {
	a2asrv.RequestHandler
}

func (l *limitedHistory) SendMessage(ctx context.Context, req *a2a.SendMessageRequest) (a2a.SendMessageResult, error) {
	res, err := l.RequestHandler.SendMessage(ctx, req)
	task, ok := res.(*a2a.Task)
	if err != nil || !ok || req.Config == nil || req.Config.HistoryLength == nil {
		return res, err
	}
	n := max(*req.Config.HistoryLength, 0)
	if n >= len(task.History) {
		return task, nil
	}
	limited := *task
	limited.History = append([]*a2a.Message{}, task.History[len(task.History)-n:]...)
	return &limited, nil
}
