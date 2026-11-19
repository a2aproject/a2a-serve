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

package a2aserve

import (
	"context"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aevent"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"
)

// racingStore stores the question of a task once, as the handler does, at
// the first update after a Get, so that update loses.
type racingStore struct {
	*taskstore.InMemory
	question *a2a.TaskStatusUpdateEvent
}

func (s *racingStore) Update(ctx context.Context, req *taskstore.UpdateRequest) (taskstore.TaskVersion, error) {
	if q := s.question; q != nil {
		s.question = nil
		stored, err := s.Get(ctx, q.TaskID)
		if err != nil {
			return taskstore.TaskVersionMissing, err
		}
		task := *stored.Task
		task.Status = q.Status
		if _, err := s.InMemory.Update(ctx, &taskstore.UpdateRequest{Task: &task, Event: q, PrevTask: stored.Task, PrevVersion: stored.Version}); err != nil {
			return taskstore.TaskVersionMissing, err
		}
	}
	return s.InMemory.Update(ctx, req)
}

func TestAbandonStoresTheEndOverAConcurrentQuestion(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	task := &a2a.Task{ID: a2a.NewTaskID(), ContextID: "c", Status: a2a.TaskStatus{State: a2a.TaskStateWorking}}
	store := &racingStore{
		InMemory: taskstore.NewInMemory(nil),
		question: &a2a.TaskStatusUpdateEvent{TaskID: task.ID, ContextID: "c", Status: a2a.TaskStatus{State: a2a.TaskStateInputRequired}},
	}
	if _, err := store.Create(ctx, task); err != nil {
		t.Fatalf("Store.Create() error = %v", err)
	}
	failed := &a2a.TaskStatusUpdateEvent{TaskID: task.ID, ContextID: "c", Status: a2a.TaskStatus{State: a2a.TaskStateFailed}}

	err := abandon(store, taskstore.NewFullUpdateMaterializer(a2aevent.ApplyShallowUpdate))(ctx, failed)

	if err != nil {
		t.Fatalf("abandon() error = %v, want nil", err)
	}
	stored, err := store.Get(ctx, task.ID)
	if err != nil {
		t.Fatalf("Store.Get() error = %v", err)
	}
	if got := stored.Task.Status.State; got != a2a.TaskStateFailed {
		t.Fatalf("Store.Get() state = %v, want %v", got, a2a.TaskStateFailed)
	}
}
