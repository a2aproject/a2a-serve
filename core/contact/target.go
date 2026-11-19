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
	"context"
	"errors"
	"fmt"
	"iter"
	"maps"
	"strconv"
	"sync"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/a2aproject/a2a-serve/internal/a2aproxy"
)

// handlerFor returns the A2A handler of the target for calls in t: a remote
// contact gets the hop count of t and its params on each request.
func (tg Target) handlerFor(t Turn) a2asrv.RequestHandler {
	if tg.client == nil && !tg.remote {
		return tg.handler
	}
	params := a2aclient.ServiceParams{HeaderHops: {strconv.Itoa(t.Hops + 1)}}
	maps.Copy(params, tg.params)
	attach := func(ctx context.Context) context.Context { return a2aclient.AttachServiceParams(ctx, params) }
	if tg.remote {
		return &a2aproxy.ContextHandler{Handler: tg.handler, Context: attach}
	}
	return &a2aproxy.ClientHandler{Client: tg.client, Context: attach}
}

// withMetadata returns the metadata of a request with the turn's added.
func withMetadata(t Turn, m map[string]any) map[string]any {
	if len(t.Metadata) == 0 {
		return m
	}
	out := maps.Clone(m)
	if out == nil {
		out = map[string]any{}
	}
	maps.Copy(out, t.Metadata)
	return out
}

func (tg Target) cancel(ctx context.Context, t Turn, id a2a.TaskID) error {
	_, err := tg.handlerFor(t).CancelTask(ctx, &a2a.CancelTaskRequest{ID: id, Metadata: withMetadata(t, nil)})
	return err
}

// Dial sends msg to the contact of target for the turn t, such as the answer
// to an approval, with the hop count and metadata of t, and returns without
// waiting for the task to end.
func Dial(ctx context.Context, t Turn, target Target, msg *a2a.Message) (a2a.SendMessageResult, error) {
	return target.handlerFor(t).SendMessage(ctx, &a2a.SendMessageRequest{
		Message:  msg,
		Config:   &a2a.SendMessageConfig{ReturnImmediately: true},
		Metadata: withMetadata(t, nil),
	})
}

// cancelCallsTimeout bounds how long canceling the calls of a turn takes.
const cancelCallsTimeout = 10 * time.Second

// cancelCalls cancels calls at once with cancel, and returns the ones it
// canceled. A task that already ended, or that the agent no longer has,
// counts as canceled.
func cancelCalls(ctx context.Context, t Turn, calls []Call, cancel func(context.Context, Call) error) ([]Call, error) {
	ctx, stop := context.WithTimeout(ctx, cancelCallsTimeout)
	defer stop()
	errs := make([]error, len(calls))
	var wg sync.WaitGroup
	for i, c := range calls {
		wg.Go(func() {
			err := cancel(ctx, c)
			if err != nil && !errors.Is(err, a2a.ErrTaskNotCancelable) && !errors.Is(err, a2a.ErrTaskNotFound) {
				errs[i] = fmt.Errorf("canceling task %s at %s: %w", c.TaskID, c.Contact, err)
			}
		})
	}
	wg.Wait()
	var canceled []Call
	for i, c := range calls {
		if errs[i] == nil {
			canceled = append(canceled, c)
		}
	}
	return canceled, errors.Join(errs...)
}

// refusingHandler fails every A2A method with err.
type refusingHandler struct {
	err error
}

var _ a2asrv.RequestHandler = refusingHandler{}

func (h refusingHandler) GetTask(context.Context, *a2a.GetTaskRequest) (*a2a.Task, error) {
	return nil, h.err
}

func (h refusingHandler) ListTasks(context.Context, *a2a.ListTasksRequest) (*a2a.ListTasksResponse, error) {
	return nil, h.err
}

func (h refusingHandler) CancelTask(context.Context, *a2a.CancelTaskRequest) (*a2a.Task, error) {
	return nil, h.err
}

func (h refusingHandler) SendMessage(context.Context, *a2a.SendMessageRequest) (a2a.SendMessageResult, error) {
	return nil, h.err
}

func (h refusingHandler) SubscribeToTask(context.Context, *a2a.SubscribeToTaskRequest) iter.Seq2[a2a.Event, error] {
	return h.stream
}

func (h refusingHandler) SendStreamingMessage(context.Context, *a2a.SendMessageRequest) iter.Seq2[a2a.Event, error] {
	return h.stream
}

func (h refusingHandler) GetTaskPushConfig(context.Context, *a2a.GetTaskPushConfigRequest) (*a2a.PushConfig, error) {
	return nil, h.err
}

func (h refusingHandler) ListTaskPushConfigs(context.Context, *a2a.ListTaskPushConfigRequest) (*a2a.ListTaskPushConfigResponse, error) {
	return nil, h.err
}

func (h refusingHandler) CreateTaskPushConfig(context.Context, *a2a.PushConfig) (*a2a.PushConfig, error) {
	return nil, h.err
}

func (h refusingHandler) DeleteTaskPushConfig(context.Context, *a2a.DeleteTaskPushConfigRequest) error {
	return h.err
}

func (h refusingHandler) GetExtendedAgentCard(context.Context, *a2a.GetExtendedAgentCardRequest) (*a2a.AgentCard, error) {
	return nil, h.err
}

func (h refusingHandler) stream(yield func(a2a.Event, error) bool) {
	yield(nil, h.err)
}
