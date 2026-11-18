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

package agentrun

import (
	"context"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-serve/internal/agentprofile"
	"github.com/a2aproject/a2a-serve/internal/workspaces"
)

func TestGrantMounts(t *testing.T) {
	t.Parallel()
	mounts := []agentprofile.Mount{{Src: "/src/app", Dst: "/src/app"}, {Src: "/docs", Dst: "/docs"}, {Src: "/data", Dst: "/data", ReadOnly: true}}
	tests := []struct {
		storage workspaces.Storage
		want    []agentprofile.Mount
	}{
		{storage: workspaces.StorageShared, want: mounts},
		{storage: workspaces.StorageReadOnly, want: []agentprofile.Mount{{Src: "/src/app", Dst: "/src/app", ReadOnly: true}, {Src: "/docs", Dst: "/docs", ReadOnly: true}, {Src: "/data", Dst: "/data", ReadOnly: true}}},
		{storage: workspaces.StorageOwn, want: []agentprofile.Mount{{Src: "/src/app", Dst: "/src/app"}, {Src: "/docs", Dst: "/docs", ReadOnly: true}, {Src: "/data", Dst: "/data", ReadOnly: true}}},
		{storage: workspaces.StorageEmpty, want: []agentprofile.Mount{{Src: "/src/app", Dst: "/src/app"}}},
	}
	for _, tt := range tests {
		t.Run(tt.storage, func(t *testing.T) {
			t.Parallel()
			if diff := cmp.Diff(tt.want, grantMounts(tt.storage, mounts)); diff != "" {
				t.Fatalf("grantMounts() wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}

// noTasks is a task store without unfinished tasks.
type noTasks struct{}

func (noTasks) UnfinishedIn(context.Context, string) ([]a2a.TaskID, error) { return nil, nil }

func (noTasks) Cancel(context.Context, a2a.TaskID) error { return nil }

func (noTasks) Abandon(context.Context, *a2a.TaskStatusUpdateEvent) error { return nil }

func TestTurnRightAfterTheLastTaskEndedRestartsTheProcess(t *testing.T) {
	t.Parallel()
	const key = "user:ann"
	want := Grant{Storage: workspaces.StorageShared, Secrets: []SecretGrant{{Name: "KEY", From: "KEY"}}}
	r := &Runner{
		cfg:   Config{Processes: fakeProcesses{key: {Pool: "users", Grant: want}}, Tasks: noTasks{}},
		pools: map[string]*procs{"users": {}},
	}
	p := r.pools["users"]
	proc := &countingCloser{}
	p.started(p.get(key), proc, Grant{Storage: workspaces.StorageShared})
	time.AfterFunc(50*time.Millisecond, p.turn(key))

	release, err := r.admitTurn(context.Background(), key, nil)

	if err != nil {
		t.Fatalf("Runner.admitTurn() while the turn of an ended task finishes error = %v, want nil", err)
	}
	release()
	if proc.closed != 1 {
		t.Fatalf("Runner.admitTurn() closed the process %d times, want 1", proc.closed)
	}
}
