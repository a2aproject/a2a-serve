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

package contact

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"iter"
	"slices"
	"strconv"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/a2aproject/a2a-serve/core/approval"
)

// Guard returns the A2A handler of the turn t's calls to the contact name
// at target: the handler that the proxy serves. It keeps the conversation
// to the tasks it started, records them, adds the hop count and metadata
// of t, and gives the tasks that the calls leave unfinished to the
// Followup. With a Followup, the agent may not answer an approval.
func (p *Proxy) Guard(t Turn, name string, target Target) a2asrv.RequestHandler {
	h := target.handlerFor(t)
	return &guard{RequestHandler: h, proxy: p, turn: t, contact: name}
}

// guard is the handler of one turn's calls to one contact.
type guard struct {
	a2asrv.RequestHandler
	proxy   *Proxy
	turn    Turn
	contact string
}

func (g *guard) SendMessage(ctx context.Context, req *a2a.SendMessageRequest) (a2a.SendMessageResult, error) {
	if err := g.checkMessage(ctx, req); err != nil {
		return nil, err
	}
	req = g.withMetadata(req)
	if req.Config == nil || !req.Config.ReturnImmediately {
		return g.sendStreamed(ctx, req)
	}
	res, err := g.RequestHandler.SendMessage(ctx, req)
	if task, ok := res.(*a2a.Task); ok && err == nil {
		g.record(ctx, task.ID, task.ContextID)
		g.follow(ctx, task)
	}
	return res, err
}

func (g *guard) withMetadata(req *a2a.SendMessageRequest) *a2a.SendMessageRequest {
	if len(g.turn.Metadata) == 0 {
		return req
	}
	out := *req
	out.Metadata = withMetadata(g.turn, req.Metadata)
	return &out
}

// sendStreamed sends a message that waits for the task to settle as a
// stream, so that the task is recorded once the contact starts it, and
// canceling the turn that sent it cancels it too.
func (g *guard) sendStreamed(ctx context.Context, req *a2a.SendMessageRequest) (a2a.SendMessageResult, error) {
	var last a2a.Event
	for ev, err := range g.stream(ctx, req) {
		if err != nil {
			return nil, err
		}
		last = ev
	}
	switch ev := last.(type) {
	case nil:
		return nil, fmt.Errorf("%s sent no result", g.contact)
	case *a2a.Message:
		return ev, nil
	case *a2a.Task:
		return ev, nil
	}
	// A blocking SendMessage returns no history unless asked, as the
	// server's does.
	history := new(int)
	if req.Config != nil && req.Config.HistoryLength != nil {
		history = req.Config.HistoryLength
	}
	return g.RequestHandler.GetTask(ctx, &a2a.GetTaskRequest{ID: last.TaskInfo().TaskID, HistoryLength: history})
}

func (g *guard) SendStreamingMessage(ctx context.Context, req *a2a.SendMessageRequest) iter.Seq2[a2a.Event, error] {
	if err := g.checkMessage(ctx, req); err != nil {
		return func(yield func(a2a.Event, error) bool) { yield(nil, err) }
	}
	return g.stream(ctx, g.withMetadata(req))
}

// stream sends a checked message as a stream, records the task it starts
// and follows it up if the stream ends before the task does.
func (g *guard) stream(ctx context.Context, req *a2a.SendMessageRequest) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		var last *a2a.Task
		recorded := false
		for ev, err := range g.RequestHandler.SendStreamingMessage(ctx, req) {
			switch e := ev.(type) {
			case *a2a.Task:
				last = &a2a.Task{ID: e.ID, ContextID: e.ContextID, Status: e.Status}
			case *a2a.TaskStatusUpdateEvent:
				last = &a2a.Task{ID: e.TaskID, ContextID: e.ContextID, Status: e.Status}
			}
			if last != nil && !recorded {
				g.record(ctx, last.ID, last.ContextID)
				recorded = true
			}
			if !yield(ev, err) {
				break
			}
		}
		if last != nil {
			// The agent may have dropped the stream, which cancels ctx; the
			// task still has to be followed up.
			g.follow(context.WithoutCancel(ctx), last)
		}
	}
}

func (g *guard) GetTask(ctx context.Context, req *a2a.GetTaskRequest) (*a2a.Task, error) {
	if err := g.checkTask(ctx, req.ID); err != nil {
		return nil, err
	}
	return g.RequestHandler.GetTask(ctx, req)
}

// Paging limits of ListTasks, which match the ones of the task stores.
const (
	defaultListPageSize = 50
	maxListPageSize     = 100
)

// ListTasks lists the tasks which this conversation started, newest first,
// from the recorded calls. It gets the tasks on the page from the contact,
// or every task when req filters by status, and leaves out a task the
// contact no longer has. A page token is the index of the first task of
// the page.
func (g *guard) ListTasks(ctx context.Context, req *a2a.ListTasksRequest) (*a2a.ListTasksResponse, error) {
	pageSize := cmp.Or(req.PageSize, defaultListPageSize)
	if pageSize < 1 || pageSize > maxListPageSize {
		return nil, fmt.Errorf("page size must be between 1 and %d inclusive, got %d: %w", maxListPageSize, pageSize, a2a.ErrInvalidRequest)
	}
	offset := 0
	if req.PageToken != "" {
		var err error
		if offset, err = strconv.Atoi(req.PageToken); err != nil || offset < 0 {
			return nil, fmt.Errorf("invalid page token: %w", a2a.ErrInvalidRequest)
		}
	}
	ids, err := g.proxy.calls.List(ctx, g.turn, g.contact, req.ContextID)
	if err != nil {
		return nil, err
	}
	page := func(total int) (start, end int) {
		return min(offset, total), min(offset+pageSize, total)
	}
	out := &a2a.ListTasksResponse{PageSize: pageSize}
	var start, end int
	if req.Status == "" && req.StatusTimestampAfter == nil {
		out.TotalSize = len(ids)
		start, end = page(len(ids))
		if out.Tasks, err = g.getTasks(ctx, ids[start:end], req); err != nil {
			return nil, err
		}
	} else {
		all, err := g.getTasks(ctx, ids, req)
		if err != nil {
			return nil, err
		}
		all = slices.DeleteFunc(all, func(t *a2a.Task) bool { return !statusMatches(t, req) })
		out.TotalSize = len(all)
		start, end = page(len(all))
		out.Tasks = all[start:end]
	}
	if end < out.TotalSize {
		out.NextPageToken = strconv.Itoa(end)
	}
	return out, nil
}

// getTasks gets the tasks with ids from the contact, with the history
// length of req or no history, and leaves out the ones the contact no
// longer has.
func (g *guard) getTasks(ctx context.Context, ids []a2a.TaskID, req *a2a.ListTasksRequest) ([]*a2a.Task, error) {
	history := cmp.Or(req.HistoryLength, new(int))
	tasks := []*a2a.Task{}
	for _, id := range ids {
		t, err := g.RequestHandler.GetTask(ctx, &a2a.GetTaskRequest{ID: id, HistoryLength: history})
		if errors.Is(err, a2a.ErrTaskNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if !req.IncludeArtifacts {
			listed := *t
			listed.Artifacts = nil
			t = &listed
		}
		tasks = append(tasks, t)
	}
	return tasks, nil
}

func statusMatches(t *a2a.Task, req *a2a.ListTasksRequest) bool {
	if req.Status != "" && t.Status.State != req.Status {
		return false
	}
	after := req.StatusTimestampAfter
	return after == nil || (t.Status.Timestamp != nil && t.Status.Timestamp.After(*after))
}

func (g *guard) CancelTask(ctx context.Context, req *a2a.CancelTaskRequest) (*a2a.Task, error) {
	if err := g.checkTask(ctx, req.ID); err != nil {
		return nil, err
	}
	if len(g.turn.Metadata) > 0 {
		r := *req
		r.Metadata = withMetadata(g.turn, req.Metadata)
		req = &r
	}
	return g.RequestHandler.CancelTask(ctx, req)
}

func (g *guard) SubscribeToTask(ctx context.Context, req *a2a.SubscribeToTaskRequest) iter.Seq2[a2a.Event, error] {
	if err := g.checkTask(ctx, req.ID); err != nil {
		return func(yield func(a2a.Event, error) bool) { yield(nil, err) }
	}
	return g.RequestHandler.SubscribeToTask(ctx, req)
}

func (g *guard) GetTaskPushConfig(ctx context.Context, req *a2a.GetTaskPushConfigRequest) (*a2a.PushConfig, error) {
	if err := g.checkTask(ctx, req.TaskID); err != nil {
		return nil, err
	}
	return g.RequestHandler.GetTaskPushConfig(ctx, req)
}

func (g *guard) ListTaskPushConfigs(ctx context.Context, req *a2a.ListTaskPushConfigRequest) (*a2a.ListTaskPushConfigResponse, error) {
	if err := g.checkTask(ctx, req.TaskID); err != nil {
		return nil, err
	}
	return g.RequestHandler.ListTaskPushConfigs(ctx, req)
}

func (g *guard) CreateTaskPushConfig(ctx context.Context, req *a2a.PushConfig) (*a2a.PushConfig, error) {
	if err := g.checkTask(ctx, req.TaskID); err != nil {
		return nil, err
	}
	return g.RequestHandler.CreateTaskPushConfig(ctx, req)
}

func (g *guard) DeleteTaskPushConfig(ctx context.Context, req *a2a.DeleteTaskPushConfigRequest) error {
	if err := g.checkTask(ctx, req.TaskID); err != nil {
		return err
	}
	return g.RequestHandler.DeleteTaskPushConfig(ctx, req)
}

// record notes that this conversation started a task.
func (g *guard) record(ctx context.Context, id a2a.TaskID, contextID string) {
	c := Call{Contact: g.contact, TaskID: id, ContextID: contextID}
	if err := g.proxy.calls.Record(context.WithoutCancel(ctx), g.turn, c); err != nil {
		g.proxy.log.Error("recording a call task", "contact", g.contact, "task", id, "error", err)
	}
}

// checkTask refuses a task this conversation didn't start.
func (g *guard) checkTask(ctx context.Context, id a2a.TaskID) error {
	ok, err := g.proxy.calls.Started(ctx, g.turn, g.contact, id)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: this conversation didn't start task %s at %s", a2a.ErrTaskNotFound, id, g.contact)
	}
	return nil
}

// checkMessage refuses a message to, a reference to or a push config for a
// task, or a message to a context, this conversation didn't start, and a
// reply to a pending approval.
func (g *guard) checkMessage(ctx context.Context, req *a2a.SendMessageRequest) error {
	if req.Message == nil {
		return nil
	}
	for _, id := range req.Message.ReferenceTasks {
		if err := g.checkTask(ctx, id); err != nil {
			return err
		}
	}
	if pc := configPush(req); pc != nil && pc.TaskID != "" {
		if err := g.checkTask(ctx, pc.TaskID); err != nil {
			return err
		}
	}
	switch m := req.Message; {
	case m.TaskID != "":
		if err := g.checkTask(ctx, m.TaskID); err != nil {
			return err
		}
	case m.ContextID != "":
		ok, err := g.proxy.calls.InContext(ctx, g.turn, g.contact, m.ContextID)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%w: this conversation has no tasks in context %s of %s", a2a.ErrUnauthorized, m.ContextID, g.contact)
		}
	}
	return g.checkAnswer(ctx, req)
}

func configPush(req *a2a.SendMessageRequest) *a2a.PushConfig {
	if req.Config == nil {
		return nil
	}
	return req.Config.PushConfig
}

// checkAnswer rejects a reply to a pending approval, which the Followup
// takes instead.
func (g *guard) checkAnswer(ctx context.Context, req *a2a.SendMessageRequest) error {
	if req.Message.TaskID == "" || g.proxy.followup == nil {
		return nil
	}
	task, err := g.RequestHandler.GetTask(ctx, &a2a.GetTaskRequest{ID: req.Message.TaskID})
	if errors.Is(err, a2a.ErrTaskNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("checking task %s at %s: %w", req.Message.TaskID, g.contact, err)
	}
	if task.Status.State != a2a.TaskStateInputRequired {
		return nil
	}
	if r := permission(task); r != nil {
		g.follow(ctx, task)
		return fmt.Errorf("%w: this is an approval for the user, and the server asks them for it; don't answer it. End your turn: the result comes back into this conversation", a2a.ErrUnauthorized)
	}
	return nil
}

// permission returns the approval of a tool call that task waits for, or
// nil.
func permission(task *a2a.Task) *approval.Request {
	r := approval.FromStatus(task.Status)
	if r == nil || r.Type != approval.TypePermission {
		return nil
	}
	return r
}

// follow gives the Followup a task that the agent won't see finish: one
// waiting for an approval, or one it didn't wait for. A task waiting for
// other input stays with the agent, which answers it.
func (g *guard) follow(ctx context.Context, task *a2a.Task) {
	if g.proxy.followup == nil {
		return
	}
	state := task.Status.State
	if state.Terminal() || state == a2a.TaskStateAuthRequired {
		return
	}
	o := Outstanding{Contact: g.contact, Task: task}
	if state == a2a.TaskStateInputRequired {
		if o.Approval = permission(task); o.Approval == nil {
			return
		}
	}
	g.proxy.followup.Follow(ctx, g.turn, o)
}
