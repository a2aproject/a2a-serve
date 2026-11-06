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

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
)

// withLimitedHistory returns h, whose SendMessage and SendStreamingMessage
// return at most the history length the request asks for, as GetTask does.
// a2a-go returns the whole history there.
func withLimitedHistory(h a2asrv.RequestHandler) a2asrv.RequestHandler {
	return &limitedHistory{RequestHandler: h}
}

type limitedHistory struct {
	a2asrv.RequestHandler
}

func (l *limitedHistory) SendMessage(ctx context.Context, req *a2a.SendMessageRequest) (a2a.SendMessageResult, error) {
	res, err := l.RequestHandler.SendMessage(ctx, req)
	if err != nil {
		return res, err
	}
	if task, ok := res.(*a2a.Task); ok {
		return limitHistory(task, req.Config), nil
	}
	return res, nil
}

func (l *limitedHistory) SendStreamingMessage(ctx context.Context, req *a2a.SendMessageRequest) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		for ev, err := range l.RequestHandler.SendStreamingMessage(ctx, req) {
			if task, ok := ev.(*a2a.Task); ok && err == nil {
				ev = limitHistory(task, req.Config)
			}
			if !yield(ev, err) {
				return
			}
		}
	}
}

// limitHistory returns task with the history cut to the length cfg asks for.
// It copies the task when it cuts, since the task may be shared.
func limitHistory(task *a2a.Task, cfg *a2a.SendMessageConfig) *a2a.Task {
	if cfg == nil || cfg.HistoryLength == nil {
		return task
	}
	n := max(*cfg.HistoryLength, 0)
	if n >= len(task.History) {
		return task
	}
	limited := *task
	limited.History = append([]*a2a.Message{}, task.History[len(task.History)-n:]...)
	return &limited
}
