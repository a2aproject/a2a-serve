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
	"slices"
	"unicode/utf8"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
)

// NewHandler returns h with the size of its tasks bounded by limits:
//   - GetTask, ListTasks and SendMessage return at most limits.History
//     history messages per task, also when the request asks for more.
//   - The blocking SendMessage returns no history unless the request asks
//     for it. a2a-go returns the whole history there.
//   - ListTasks cuts parts longer than limits.ListPart, with a note that
//     GetTask returns the whole part.
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
	resp, err := h.RequestHandler.ListTasks(ctx, req)
	if err != nil {
		return nil, err
	}
	cut := *resp
	cut.Tasks = make([]*a2a.Task, len(resp.Tasks))
	for i, t := range resp.Tasks {
		cut.Tasks[i] = h.cutTask(t)
	}
	return &cut, nil
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

// cutTask returns t with its long parts cut. It copies what it changes.
func (h *handler) cutTask(t *a2a.Task) *a2a.Task {
	cut := *t
	cutMessage := func(m *a2a.Message) *a2a.Message {
		if m == nil {
			return nil
		}
		parts, changed := h.cutParts(t.ID, m.Parts)
		if !changed {
			return m
		}
		c := *m
		c.Parts = parts
		return &c
	}
	cut.Status.Message = cutMessage(t.Status.Message)
	cut.History = make([]*a2a.Message, len(t.History))
	for i, m := range t.History {
		cut.History[i] = cutMessage(m)
	}
	if t.Artifacts != nil {
		cut.Artifacts = make([]*a2a.Artifact, len(t.Artifacts))
		for i, a := range t.Artifacts {
			cut.Artifacts[i] = a
			if a == nil {
				continue
			}
			if parts, changed := h.cutParts(t.ID, a.Parts); changed {
				c := *a
				c.Parts = parts
				cut.Artifacts[i] = &c
			}
		}
	}
	return &cut
}

func (h *handler) cutParts(task a2a.TaskID, parts []*a2a.Part) ([]*a2a.Part, bool) {
	var out []*a2a.Part
	for i, p := range parts {
		c := h.cutPart(task, p)
		if c == p {
			continue
		}
		if out == nil {
			out = slices.Clone(parts)
		}
		out[i] = c
	}
	if out == nil {
		return parts, false
	}
	return out, true
}

// cutPart returns p, or a copy cut to the list limit: a long text keeps its
// start, and a long file or data part becomes a note.
func (h *handler) cutPart(task a2a.TaskID, p *a2a.Part) *a2a.Part {
	if p == nil {
		return p
	}
	limit := h.limits.ListPart
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
		return omitted(p, fmt.Sprintf("[file of %d bytes left out of the list; get task %s for it]", len(c), task))
	case a2a.Data:
		data, err := json.Marshal(c.Value)
		if err != nil || len(data) <= limit {
			return p
		}
		return omitted(p, fmt.Sprintf("[data of %d bytes left out of the list; get task %s for it]", len(data), task))
	}
	return p
}

func omitted(p *a2a.Part, note string) *a2a.Part {
	cut := *p
	cut.Content = a2a.Text(note)
	cut.MediaType = ""
	return &cut
}
