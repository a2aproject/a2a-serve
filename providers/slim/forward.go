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

package main

import (
	"context"
	"iter"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
)

// forwarder serves A2A requests by sending them on, with params, with a
// transport that open returns for each request. release is called once the
// request is done, after the last event of a stream.
type forwarder struct {
	open   func(ctx context.Context) (t a2aclient.Transport, release func(), err error)
	params a2aclient.ServiceParams
}

var _ a2asrv.RequestHandler = (*forwarder)(nil)

func unary[Req, Resp any](ctx context.Context, f *forwarder, req Req, call func(a2aclient.Transport, context.Context, a2aclient.ServiceParams, Req) (Resp, error)) (Resp, error) {
	t, release, err := f.open(ctx)
	if err != nil {
		var zero Resp
		return zero, err
	}
	defer release()
	return call(t, ctx, f.params, req)
}

func stream[Req any](ctx context.Context, f *forwarder, req Req, call func(a2aclient.Transport, context.Context, a2aclient.ServiceParams, Req) iter.Seq2[a2a.Event, error]) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		t, release, err := f.open(ctx)
		if err != nil {
			yield(nil, err)
			return
		}
		defer release()
		for ev, err := range call(t, ctx, f.params, req) {
			if !yield(ev, err) || err != nil {
				return
			}
		}
	}
}

func (f *forwarder) GetTask(ctx context.Context, req *a2a.GetTaskRequest) (*a2a.Task, error) {
	return unary(ctx, f, req, a2aclient.Transport.GetTask)
}

func (f *forwarder) ListTasks(ctx context.Context, req *a2a.ListTasksRequest) (*a2a.ListTasksResponse, error) {
	return unary(ctx, f, req, a2aclient.Transport.ListTasks)
}

func (f *forwarder) CancelTask(ctx context.Context, req *a2a.CancelTaskRequest) (*a2a.Task, error) {
	return unary(ctx, f, req, a2aclient.Transport.CancelTask)
}

func (f *forwarder) SendMessage(ctx context.Context, req *a2a.SendMessageRequest) (a2a.SendMessageResult, error) {
	return unary(ctx, f, req, a2aclient.Transport.SendMessage)
}

func (f *forwarder) SubscribeToTask(ctx context.Context, req *a2a.SubscribeToTaskRequest) iter.Seq2[a2a.Event, error] {
	return stream(ctx, f, req, a2aclient.Transport.SubscribeToTask)
}

func (f *forwarder) SendStreamingMessage(ctx context.Context, req *a2a.SendMessageRequest) iter.Seq2[a2a.Event, error] {
	return stream(ctx, f, req, a2aclient.Transport.SendStreamingMessage)
}

func (f *forwarder) GetTaskPushConfig(ctx context.Context, req *a2a.GetTaskPushConfigRequest) (*a2a.PushConfig, error) {
	return unary(ctx, f, req, a2aclient.Transport.GetTaskPushConfig)
}

func (f *forwarder) ListTaskPushConfigs(ctx context.Context, req *a2a.ListTaskPushConfigRequest) (*a2a.ListTaskPushConfigResponse, error) {
	configs, err := unary(ctx, f, req, a2aclient.Transport.ListTaskPushConfigs)
	if err != nil {
		return nil, err
	}
	return &a2a.ListTaskPushConfigResponse{Configs: configs}, nil
}

func (f *forwarder) CreateTaskPushConfig(ctx context.Context, req *a2a.PushConfig) (*a2a.PushConfig, error) {
	return unary(ctx, f, req, a2aclient.Transport.CreateTaskPushConfig)
}

func (f *forwarder) DeleteTaskPushConfig(ctx context.Context, req *a2a.DeleteTaskPushConfigRequest) error {
	_, err := unary(ctx, f, req, func(t a2aclient.Transport, ctx context.Context, p a2aclient.ServiceParams, req *a2a.DeleteTaskPushConfigRequest) (struct{}, error) {
		return struct{}{}, t.DeleteTaskPushConfig(ctx, p, req)
	})
	return err
}

func (f *forwarder) GetExtendedAgentCard(ctx context.Context, req *a2a.GetExtendedAgentCardRequest) (*a2a.AgentCard, error) {
	return unary(ctx, f, req, a2aclient.Transport.GetExtendedAgentCard)
}
