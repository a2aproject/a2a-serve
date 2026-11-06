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
	"fmt"
	"io"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/a2aproject/a2a-serve/internal/agentsrv/agentkey"
	"github.com/a2aproject/a2a-serve/internal/auth"
)

type nopCloser struct{}

func (nopCloser) Close() error { return nil }

func userKey(i int) string { return agentkey.User(fmt.Sprintf("pair:u%d", i)) }

// startUsers starts the agent processes of n users and returns them.
func startUsers(t *testing.T, u *userAgents, n int) []io.Closer {
	t.Helper()
	procs := make([]io.Closer, n)
	for i := range n {
		a := u.get(userKey(i))
		release, err := u.reserve(a)
		if err != nil {
			t.Fatalf("userAgents.reserve(%d) error = %v", i, err)
		}
		procs[i] = &nopCloser{}
		u.started(a, procs[i])
		release()
	}
	return procs
}

func TestUserAgentsReserveRefusesStartOverLimit(t *testing.T) {
	t.Parallel()
	u := &userAgents{}
	procs := startUsers(t, u, maxUserAgents-1)
	last := u.get(userKey(maxUserAgents - 1))
	release, err := u.reserve(last)
	if err != nil {
		t.Fatalf("userAgents.reserve() of the last free slot error = %v", err)
	}

	if _, err := u.reserve(u.get(userKey(maxUserAgents))); !errors.Is(err, errAgentsBusy) {
		t.Fatalf("userAgents.reserve() while the last slot starts error = %v, want %v", err, errAgentsBusy)
	}
	if !u.hasRoom(userKey(maxUserAgents - 1)) {
		t.Fatalf("userAgents.hasRoom() of the starting process = false, want true")
	}
	release()
	u.exited(u.get(userKey(0)), procs[0])
	if _, err := u.reserve(u.get(userKey(maxUserAgents))); err != nil {
		t.Fatalf("userAgents.reserve() after a process exited error = %v", err)
	}
}

func TestUserAgentsHasRoom(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		running int
		busy    int
		key     string
		want    bool
	}{
		{name: "free slot", running: maxUserAgents - 1, busy: maxUserAgents - 1, key: userKey(maxUserAgents), want: true},
		{name: "idle process to stop", running: maxUserAgents, busy: maxUserAgents - 1, key: userKey(maxUserAgents), want: true},
		{name: "all busy", running: maxUserAgents, busy: maxUserAgents, key: userKey(maxUserAgents)},
		{name: "own process runs", running: maxUserAgents, busy: maxUserAgents, key: userKey(0), want: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			u := &userAgents{}
			startUsers(t, u, tc.running)
			for i := range tc.busy {
				u.turn(userKey(i))
			}
			if got := u.hasRoom(tc.key); got != tc.want {
				t.Fatalf("userAgents.hasRoom(%q) = %v, want %v", tc.key, got, tc.want)
			}
		})
	}
}

func TestUserAgentsTurnEndFreesProcess(t *testing.T) {
	t.Parallel()
	u := &userAgents{}
	startUsers(t, u, maxUserAgents)
	ends := make([]func(), maxUserAgents)
	for i := range maxUserAgents {
		ends[i] = u.turn(userKey(i))
	}
	ends[0]()
	ends[0]()
	if !u.hasRoom(userKey(maxUserAgents)) {
		t.Fatalf("userAgents.hasRoom() after a turn ended = false, want true")
	}
	u.turn(userKey(0))
	if u.hasRoom(userKey(maxUserAgents)) {
		t.Fatalf("userAgents.hasRoom() after a new turn = true, want false")
	}
}

func TestNewTaskRefusedWhileAllUserAgentsAreBusy(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{})
	ctx := context.Background()
	first := ts.client(ts.pair("u0", auth.RoleAuthenticated))
	asked := send(t, first, userText("ask", "", ""))
	for i := 1; i < maxUserAgents; i++ {
		c := ts.client(ts.pair(fmt.Sprintf("u%d", i), auth.RoleAuthenticated))
		if task := send(t, c, userText("ask", "", "")); task.Status.State != a2a.TaskStateInputRequired {
			t.Fatalf("task state = %s, want %s", task.Status.State, a2a.TaskStateInputRequired)
		}
	}
	late := ts.client(ts.pair("late", auth.RoleAuthenticated))

	if _, err := late.SendMessage(ctx, &a2a.SendMessageRequest{Message: userText("hello", "", "")}); !errors.Is(err, a2a.ErrServerError) {
		t.Fatalf("SendMessage() while all agents are busy error = %v, want %v", err, a2a.ErrServerError)
	}
	res, err := late.ListTasks(ctx, &a2a.ListTasksRequest{})
	if err != nil {
		t.Fatalf("ListTasks() error = %v", err)
	}
	if len(res.Tasks) != 0 {
		t.Fatalf("ListTasks() = %d tasks, want none stored for a refused message", len(res.Tasks))
	}
	if _, err := first.CancelTask(ctx, &a2a.CancelTaskRequest{ID: asked.ID}); err != nil {
		t.Fatalf("CancelTask() error = %v", err)
	}
	waitState(t, first, asked.ID, a2a.TaskStateCanceled)
	if task := send(t, late, userText("hello", "", "")); task.Status.State != a2a.TaskStateCompleted {
		t.Fatalf("task state after a process became idle = %s, want %s", task.Status.State, a2a.TaskStateCompleted)
	}
}
