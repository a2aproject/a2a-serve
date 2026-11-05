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
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

type fakeHandler struct {
	a2asrv.RequestHandler
	task *a2a.Task
}

func (f *fakeHandler) SendMessage(context.Context, *a2a.SendMessageRequest) (a2a.SendMessageResult, error) {
	return f.task, nil
}

func (f *fakeHandler) SendStreamingMessage(context.Context, *a2a.SendMessageRequest) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) { yield(f.task, nil) }
}

func (f *fakeHandler) CancelTask(context.Context, *a2a.CancelTaskRequest) (*a2a.Task, error) {
	return f.task, nil
}

func TestBindHandlerRefusesTurnsWhileBuilding(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	req := &a2a.SendMessageRequest{Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("hi"))}
	var errs []error

	bindHandler(func(s *handlerSender) a2asrv.RequestHandler {
		_, err := s.SendMessage(ctx, req)
		errs = append(errs, err)
		for _, err := range s.SendStreamingMessage(ctx, req) {
			errs = append(errs, err)
		}
		_, err = s.CancelTask(ctx, &a2a.CancelTaskRequest{ID: "t1"})
		errs = append(errs, err)
		return &fakeHandler{}
	})

	if diff := cmp.Diff([]error{errNotReady, errNotReady, errNotReady}, errs, cmpopts.EquateErrors()); diff != "" {
		t.Fatalf("handlerSender while building wrong errors (-want +got) diff = %s", diff)
	}
}

func TestBindHandler(t *testing.T) {
	t.Parallel()
	task := &a2a.Task{ID: "t1", ContextID: "c1"}
	h := &fakeHandler{task: task}
	s, got := bindHandler(func(*handlerSender) a2asrv.RequestHandler { return h })
	if got != a2asrv.RequestHandler(h) {
		t.Fatalf("bindHandler() handler = %v, want the built one", got)
	}
	ctx := t.Context()
	req := &a2a.SendMessageRequest{Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("hi"))}

	res, err := s.SendMessage(ctx, req)
	if err != nil {
		t.Fatalf("handlerSender.SendMessage() error = %v", err)
	}
	if diff := cmp.Diff(a2a.SendMessageResult(task), res); diff != "" {
		t.Fatalf("handlerSender.SendMessage() wrong result (-want +got) diff = %s", diff)
	}
	var events []a2a.Event
	for ev, err := range s.SendStreamingMessage(ctx, req) {
		if err != nil {
			t.Fatalf("handlerSender.SendStreamingMessage() error = %v", err)
		}
		events = append(events, ev)
	}
	if diff := cmp.Diff([]a2a.Event{task}, events); diff != "" {
		t.Fatalf("handlerSender.SendStreamingMessage() wrong result (-want +got) diff = %s", diff)
	}
	canceled, err := s.CancelTask(ctx, &a2a.CancelTaskRequest{ID: "t1"})
	if err != nil {
		t.Fatalf("handlerSender.CancelTask() error = %v", err)
	}
	if diff := cmp.Diff(task, canceled); diff != "" {
		t.Fatalf("handlerSender.CancelTask() wrong result (-want +got) diff = %s", diff)
	}
}
