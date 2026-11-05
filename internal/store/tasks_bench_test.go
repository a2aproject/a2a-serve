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

package store

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"
)

func openBenchStore(b *testing.B) *Store {
	b.Helper()
	s, err := Open(filepath.Join(b.TempDir(), "server.db"))
	if err != nil {
		b.Fatalf("Open() error = %v", err)
	}
	b.Cleanup(func() {
		if err := s.Close(); err != nil {
			b.Errorf("Close() error = %v", err)
		}
	})
	return s
}

// streamChunks creates a working task which streamed n chunks of one
// artifact, as an agent streaming a token per chunk does.
func streamChunks(b *testing.B, ts *TaskStore, id string, n int) {
	b.Helper()
	ctx := asUser("owner")
	task := newTask(id, "c-"+id)
	task.History = []*a2a.Message{a2a.NewMessageForTask(a2a.MessageRoleUser, task, a2a.NewTextPart("write a long story"))}
	if _, err := ts.Create(ctx, task); err != nil {
		b.Fatalf("Create() error = %v", err)
	}
	for i := range n {
		ev := &a2a.TaskArtifactUpdateEvent{TaskID: task.ID, ContextID: task.ContextID, Append: i > 0,
			Artifact: &a2a.Artifact{ID: "story", Parts: []*a2a.Part{a2a.NewTextPart(fmt.Sprintf("token%d ", i))}}}
		if _, err := ts.Update(ctx, &taskstore.UpdateRequest{Task: task, Event: ev}); err != nil {
			b.Fatalf("Update() error = %v", err)
		}
	}
}

func BenchmarkGetWorkingTask(b *testing.B) {
	for _, chunks := range []int{2000, 10000} {
		b.Run(fmt.Sprintf("chunks=%d", chunks), func(b *testing.B) {
			ts := openBenchStore(b).Tasks("", testViewer)
			streamChunks(b, ts, "t1", chunks)
			b.ReportAllocs()
			for b.Loop() {
				if _, err := ts.Get(asUser("owner"), "t1"); err != nil {
					b.Fatalf("Get() error = %v", err)
				}
			}
		})
	}
}

func BenchmarkListWorkingTasks(b *testing.B) {
	ts := openBenchStore(b).Tasks("", testViewer)
	for i := range 100 {
		streamChunks(b, ts, fmt.Sprintf("t%d", i), 200)
	}
	b.ReportAllocs()
	for b.Loop() {
		resp, err := ts.List(asUser("owner"), &a2a.ListTasksRequest{PageSize: 100})
		if err != nil {
			b.Fatalf("List() error = %v", err)
		}
		if len(resp.Tasks) != 100 {
			b.Fatalf("List() = %d tasks, want 100", len(resp.Tasks))
		}
	}
}

func BenchmarkAppendChunk(b *testing.B) {
	ts := openBenchStore(b).Tasks("", testViewer)
	streamChunks(b, ts, "t1", 1)
	task := newTask("t1", "c-t1")
	ev := &a2a.TaskArtifactUpdateEvent{TaskID: task.ID, ContextID: task.ContextID, Append: true,
		Artifact: &a2a.Artifact{ID: "story", Parts: []*a2a.Part{a2a.NewTextPart("token ")}}}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := ts.Update(asUser("owner"), &taskstore.UpdateRequest{Task: task, Event: ev}); err != nil {
			b.Fatalf("Update() error = %v", err)
		}
	}
}
