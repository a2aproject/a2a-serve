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
	"iter"
	"sync/atomic"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
)

// errNotReady is returned by a [handlerSender] while its handler is being built.
var errNotReady = errors.New("the agent is not ready")

// handlerSender sends turns to the agent's A2A handler. The handler runs
// components that send it turns, such as answers to the agent's questions,
// so [bindHandler] builds the handler together with its sender.
type handlerSender struct {
	h atomic.Pointer[a2asrv.RequestHandler]
}

// bindHandler builds the agent's A2A handler with build, which gets the sender of
// the turns that the handler's components send to it, and returns both.
// The sender refuses turns with [errNotReady] until build returns.
func bindHandler(build func(*handlerSender) a2asrv.RequestHandler) (*handlerSender, a2asrv.RequestHandler) {
	s := &handlerSender{}
	h := build(s)
	s.h.Store(&h)
	return s, h
}

func (s *handlerSender) handler() (a2asrv.RequestHandler, error) {
	h := s.h.Load()
	if h == nil {
		return nil, errNotReady
	}
	return *h, nil
}

// SendMessage sends a message to the agent.
func (s *handlerSender) SendMessage(ctx context.Context, req *a2a.SendMessageRequest) (a2a.SendMessageResult, error) {
	h, err := s.handler()
	if err != nil {
		return nil, err
	}
	return h.SendMessage(ctx, req)
}

// SendStreamingMessage sends a message to the agent and streams the events
// of its task.
func (s *handlerSender) SendStreamingMessage(ctx context.Context, req *a2a.SendMessageRequest) iter.Seq2[a2a.Event, error] {
	h, err := s.handler()
	if err != nil {
		return func(yield func(a2a.Event, error) bool) { yield(nil, err) }
	}
	return h.SendStreamingMessage(ctx, req)
}

// CancelTask cancels a task of the agent.
func (s *handlerSender) CancelTask(ctx context.Context, req *a2a.CancelTaskRequest) (*a2a.Task, error) {
	h, err := s.handler()
	if err != nil {
		return nil, err
	}
	return h.CancelTask(ctx, req)
}
