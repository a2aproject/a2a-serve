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

// Package a2aproxy serves A2A requests by forwarding them to another agent.
package a2aproxy

import (
	"context"
	"iter"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
)

// ClientHandler serves A2A requests by forwarding them to an agent through
// Client. Context, if set, prepares the context of every forwarded request.
type ClientHandler struct {
	Client  *a2aclient.Client
	Context func(context.Context) context.Context
}

var _ a2asrv.RequestHandler = (*ClientHandler)(nil)

func (p *ClientHandler) ctx(ctx context.Context) context.Context {
	if p.Context != nil {
		return p.Context(ctx)
	}
	return ctx
}

// GetTask forwards the request to Client.
func (p *ClientHandler) GetTask(ctx context.Context, req *a2a.GetTaskRequest) (*a2a.Task, error) {
	return p.Client.GetTask(p.ctx(ctx), req)
}

// ListTasks forwards the request to Client.
func (p *ClientHandler) ListTasks(ctx context.Context, req *a2a.ListTasksRequest) (*a2a.ListTasksResponse, error) {
	return p.Client.ListTasks(p.ctx(ctx), req)
}

// CancelTask forwards the request to Client.
func (p *ClientHandler) CancelTask(ctx context.Context, req *a2a.CancelTaskRequest) (*a2a.Task, error) {
	return p.Client.CancelTask(p.ctx(ctx), req)
}

// SendMessage forwards the request to Client.
func (p *ClientHandler) SendMessage(ctx context.Context, req *a2a.SendMessageRequest) (a2a.SendMessageResult, error) {
	return p.Client.SendMessage(p.ctx(ctx), req)
}

// SubscribeToTask forwards the request to Client.
func (p *ClientHandler) SubscribeToTask(ctx context.Context, req *a2a.SubscribeToTaskRequest) iter.Seq2[a2a.Event, error] {
	return p.Client.SubscribeToTask(p.ctx(ctx), req)
}

// SendStreamingMessage forwards the request to Client.
func (p *ClientHandler) SendStreamingMessage(ctx context.Context, req *a2a.SendMessageRequest) iter.Seq2[a2a.Event, error] {
	return p.Client.SendStreamingMessage(p.ctx(ctx), req)
}

// GetTaskPushConfig forwards the request to Client.
func (p *ClientHandler) GetTaskPushConfig(ctx context.Context, req *a2a.GetTaskPushConfigRequest) (*a2a.PushConfig, error) {
	return p.Client.GetTaskPushConfig(p.ctx(ctx), req)
}

// ListTaskPushConfigs forwards the request to Client.
func (p *ClientHandler) ListTaskPushConfigs(ctx context.Context, req *a2a.ListTaskPushConfigRequest) (*a2a.ListTaskPushConfigResponse, error) {
	resp, err := p.Client.ListTaskPushConfigs(p.ctx(ctx), req)
	if err != nil {
		return nil, err
	}
	return &a2a.ListTaskPushConfigResponse{Configs: resp}, nil
}

// CreateTaskPushConfig forwards the request to Client.
func (p *ClientHandler) CreateTaskPushConfig(ctx context.Context, req *a2a.PushConfig) (*a2a.PushConfig, error) {
	return p.Client.CreateTaskPushConfig(p.ctx(ctx), req)
}

// DeleteTaskPushConfig forwards the request to Client.
func (p *ClientHandler) DeleteTaskPushConfig(ctx context.Context, req *a2a.DeleteTaskPushConfigRequest) error {
	return p.Client.DeleteTaskPushConfig(p.ctx(ctx), req)
}

// GetExtendedAgentCard forwards the request to Client.
func (p *ClientHandler) GetExtendedAgentCard(ctx context.Context, req *a2a.GetExtendedAgentCardRequest) (*a2a.AgentCard, error) {
	return p.Client.GetExtendedAgentCard(p.ctx(ctx), req)
}
