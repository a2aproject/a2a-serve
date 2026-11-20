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

package serve

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"
	"github.com/google/go-cmp/cmp"
)

func artifactNamed(name string, size int) *a2a.Artifact {
	return &a2a.Artifact{ID: a2a.ArtifactID(name), Name: name, Parts: a2a.ContentParts{a2a.NewTextPart(strings.Repeat("x", size))}}
}

// artifactsOf returns the names of the artifacts of the stored task id.
func artifactsOf(t *testing.T, m *memory, id a2a.TaskID) []string {
	t.Helper()
	got, err := m.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("memory.Get(%s) error = %v", id, err)
	}
	var names []string
	for _, a := range got.Task.Artifacts {
		names = append(names, a.Name)
	}
	return names
}

func TestMemoryAddsAnArtifactToItsTaskOnceTheTaskEnds(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		// endFirst ends the task before the artifact arrives.
		endFirst bool
	}{
		{name: "artifact before the end"},
		{name: "artifact after the end", endFirst: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			m := newMemory(defaultMemoryLimits, time.Now)
			task := &a2a.Task{ID: "t1", ContextID: "c", Status: a2a.TaskStatus{State: a2a.TaskStateWorking}}
			if _, err := m.Create(ctx, task); err != nil {
				t.Fatalf("memory.Create() error = %v", err)
			}
			canceled := &a2a.Task{ID: "t1", ContextID: "c", Status: a2a.TaskStatus{State: a2a.TaskStateCanceled}}

			if tc.endFirst {
				if _, err := m.Update(ctx, &taskstore.UpdateRequest{Task: canceled}); err != nil {
					t.Fatalf("memory.Update() error = %v", err)
				}
			}
			m.attachArtifact("t1", artifactNamed("changes", 1))
			if got := artifactsOf(t, m, "t1"); !tc.endFirst && got != nil {
				t.Fatalf("artifacts of a running task = %v, want none until it ends", got)
			}
			if !tc.endFirst {
				if _, err := m.Update(ctx, &taskstore.UpdateRequest{Task: canceled}); err != nil {
					t.Fatalf("memory.Update() error = %v", err)
				}
			}

			if diff := cmp.Diff([]string{"changes"}, artifactsOf(t, m, "t1")); diff != "" {
				t.Fatalf("artifacts of the ended task wrong result (-want +got) diff = %s", diff)
			}
			if art := m.takeArtifact("t1"); art != nil {
				t.Fatalf("memory.takeArtifact() after the end = %v, want nil", art)
			}
		})
	}
}

func TestMemoryBoundsTheArtifactsThatWait(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	limits := defaultMemoryLimits
	limits.WaitingArtifactBytes = 250
	m := newMemory(limits, time.Now)
	for _, id := range []a2a.TaskID{"t1", "t2", "t3"} {
		if _, err := m.Create(ctx, &a2a.Task{ID: id, ContextID: "c", Status: a2a.TaskStatus{State: a2a.TaskStateWorking}}); err != nil {
			t.Fatalf("memory.Create() error = %v", err)
		}
		m.attachArtifact(id, artifactNamed(string(id), 100))
	}

	var waiting []a2a.TaskID
	for _, id := range []a2a.TaskID{"t1", "t2", "t3"} {
		if m.takeArtifact(id) != nil {
			waiting = append(waiting, id)
		}
	}

	if diff := cmp.Diff([]a2a.TaskID{"t3"}, waiting); diff != "" {
		t.Fatalf("waiting artifacts wrong result (-want +got) diff = %s", diff)
	}
	if m.waitingBytes != 0 {
		t.Fatalf("memory.waitingBytes after every take = %d, want 0", m.waitingBytes)
	}
}
