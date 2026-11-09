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

package a2alimit

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"unicode/utf8"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
)

// NewHandler returns h with the size of its tasks bounded by limits:
//   - GetTask, ListTasks and SendMessage return at most limits.History
//     history messages per task, also when the request asks for more.
//   - The blocking SendMessage returns no history unless the request asks
//     for it. a2a-go returns the whole history there.
func NewHandler(h a2asrv.RequestHandler, limits Limits) a2asrv.RequestHandler {
	return &handler{RequestHandler: h, limits: limits}
}

type handler struct {
	a2asrv.RequestHandler
	limits Limits
}

func (h *handler) GetTask(ctx context.Context, req *a2a.GetTaskRequest) (*a2a.Task, error) {
	if req.HistoryLength != nil {
		clamped := *req
		clamped.HistoryLength = h.clamp(req.HistoryLength)
		req = &clamped
	}
	return h.RequestHandler.GetTask(ctx, req)
}

func (h *handler) ListTasks(ctx context.Context, req *a2a.ListTasksRequest) (*a2a.ListTasksResponse, error) {
	if req.HistoryLength != nil {
		clamped := *req
		clamped.HistoryLength = h.clamp(req.HistoryLength)
		req = &clamped
	}
	return h.RequestHandler.ListTasks(ctx, req)
}

func (h *handler) SendMessage(ctx context.Context, req *a2a.SendMessageRequest) (a2a.SendMessageResult, error) {
	res, err := h.RequestHandler.SendMessage(ctx, req)
	if err != nil {
		return res, err
	}
	task, ok := res.(*a2a.Task)
	if !ok {
		return res, nil
	}
	length := new(int)
	if req.Config != nil && req.Config.HistoryLength != nil {
		length = h.clamp(req.Config.HistoryLength)
	}
	return limitHistory(task, *length), nil
}

func (h *handler) SendStreamingMessage(ctx context.Context, req *a2a.SendMessageRequest) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		for ev, err := range h.RequestHandler.SendStreamingMessage(ctx, req) {
			if task, ok := ev.(*a2a.Task); ok && err == nil && req.Config != nil && req.Config.HistoryLength != nil {
				ev = limitHistory(task, *h.clamp(req.Config.HistoryLength))
			}
			if !yield(ev, err) {
				return
			}
		}
	}
}

func (h *handler) clamp(n *int) *int {
	clamped := min(*n, h.limits.History)
	return &clamped
}

// limitHistory returns task with at most n history messages. It copies the
// task when it cuts, since the task may be shared.
func limitHistory(task *a2a.Task, n int) *a2a.Task {
	n = max(n, 0)
	if n >= len(task.History) {
		return task
	}
	limited := *task
	limited.History = append([]*a2a.Message{}, task.History[len(task.History)-n:]...)
	return &limited
}

// CutPart returns p, or a copy cut to limit bytes for a list of tasks: a
// long text keeps its start, and a long data part becomes a note. A file
// part becomes a note where its bytes are read, with [OmitFile].
func CutPart(task a2a.TaskID, p *a2a.Part, limit int) *a2a.Part {
	if p == nil {
		return p
	}
	switch c := p.Content.(type) {
	case a2a.Text:
		if len(c) <= limit {
			return p
		}
		keep := limit
		for keep > 0 && !utf8.RuneStart(c[keep]) {
			keep--
		}
		cut := *p
		cut.Content = c[:keep] + a2a.Text(fmt.Sprintf("\n[cut: %d of %d bytes shown; get task %s for the whole text]", keep, len(c), task))
		return &cut
	case a2a.Raw:
		if len(c) <= limit {
			return p
		}
		return OmitFile(task, p, len(c))
	case a2a.Data:
		data, err := json.Marshal(c.Value)
		if err != nil || len(data) <= limit {
			return p
		}
		return omitted(p, fmt.Sprintf("[data of %d bytes left out of the list; get task %s for it]", len(data), task))
	}
	return p
}

// OmitFile returns a copy of p, a file part of size bytes, with a note in
// place of the file.
func OmitFile(task a2a.TaskID, p *a2a.Part, size int) *a2a.Part {
	return omitted(p, fmt.Sprintf("[file of %d bytes left out of the list; get task %s for it]", size, task))
}

func omitted(p *a2a.Part, note string) *a2a.Part {
	cut := *p
	cut.Content = a2a.Text(note)
	cut.MediaType = ""
	return &cut
}
