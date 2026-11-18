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
	"strings"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/a2aproject/a2a-serve/internal/a2atext"
	"github.com/a2aproject/a2a-serve/internal/access"
	"github.com/a2aproject/a2a-serve/internal/agentprofile"
	"github.com/a2aproject/a2a-serve/internal/auth"
)

func TestMembersRunInTheirOwnProcesses(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{})
	owner := ts.client(ts.pair("me", auth.RoleOwner))
	anna := ts.client(ts.pair("anna", auth.RoleMember))
	ben := ts.client(ts.pair("ben", auth.RoleMember))

	annaTask := send(t, anna, userText("pid", "", ""))
	ownerPID, annaPID, benPID := a2atext.Result(send(t, owner, userText("pid", "", ""))), a2atext.Result(annaTask), a2atext.Result(send(t, ben, userText("pid", "", "")))
	if ownerPID == annaPID || ownerPID == benPID || annaPID == benPID {
		t.Fatalf("owner, anna and ben ran in %q, %q and %q, want separate processes", ownerPID, annaPID, benPID)
	}
	if got := a2atext.Result(send(t, anna, userText("pid", "", ""))); got != annaPID {
		t.Fatalf("anna's second conversation ran in %q, want %q", got, annaPID)
	}
	if got := a2atext.Result(send(t, owner, userText("pid", "", annaTask.ContextID))); got != annaPID {
		t.Fatalf("owner message in anna's conversation ran in %q, want %q", got, annaPID)
	}
}

func TestOwnerTurnInUserProcessActsAsTheUser(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{})
	owner := ts.client(ts.pair("me", auth.RoleOwner))
	anna := ts.client(ts.pair("anna", auth.RoleMember))
	annaTask := send(t, anna, userText("hello", "", ""))

	task := send(t, owner, userText(`call schedules add mine --cron @daily --prompt "x"`, "", annaTask.ContextID))
	wantResult(t, task, a2a.TaskStateCompleted, "Scheduled mine")
	got, err := ts.srv.store.Schedule(context.Background(), "mine")
	if err != nil {
		t.Fatalf("Schedule(mine) error = %v", err)
	}
	if got.UserID != "pair:anna" {
		t.Fatalf("Schedule(mine) user = %q, want %q", got.UserID, "pair:anna")
	}
}

func TestCapToProcessUser(t *testing.T) {
	t.Parallel()
	owner := auth.Owner()
	anna := auth.Principal{ID: "pair:anna", Role: auth.RoleMember}
	ben := auth.Principal{ID: "pair:ben", Role: auth.RoleMember}
	anon := auth.Principal{ID: auth.GuestID, Role: auth.RoleGuest}
	tests := []struct {
		name    string
		turn    auth.Principal
		process auth.Principal
		want    auth.Principal
	}{
		{name: "same user", turn: anna, process: anna, want: anna},
		{name: "owner in a user's process", turn: owner, process: anna, want: anna},
		{name: "another user in a user's process", turn: ben, process: anna, want: anna},
		{name: "guest in a user's process", turn: anon, process: anna, want: anon},
		{name: "owner in the guest process", turn: owner, process: anon, want: anon},
		{name: "user in a process without a role", turn: anna, process: auth.Principal{ID: "pair:gone"}, want: auth.Principal{ID: "pair:gone"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := capToProcessUser(tc.turn, tc.process); got != tc.want {
				t.Fatalf("capToProcessUser() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestAgentsLineOncePerSession(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{})
	owner := ts.client(ts.pair("me", auth.RoleOwner))

	first := send(t, owner, userText("line", "", ""))
	wantResult(t, first, a2a.TaskStateCompleted, "with agents line")
	wantResult(t, send(t, owner, userText("line", "", first.ContextID)), a2a.TaskStateCompleted, "without agents line")
	wantResult(t, send(t, owner, userText("call schedules list", "", first.ContextID)), a2a.TaskStateCompleted, "No schedules.")
}

func TestAgentsLineAgainAfterManyTokens(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{})
	ts.srv.claims.linedMu.Lock()
	ts.srv.claims.lineEvery = 10
	ts.srv.claims.linedMu.Unlock()
	owner := ts.client(ts.pair("me", auth.RoleOwner))

	first := send(t, owner, userText("line", "", ""))
	wantResult(t, first, a2a.TaskStateCompleted, "with agents line")
	wantResult(t, send(t, owner, userText("line "+strings.Repeat("x", 40), "", first.ContextID)), a2a.TaskStateCompleted, "without agents line")
	wantResult(t, send(t, owner, userText("line", "", first.ContextID)), a2a.TaskStateCompleted, "with agents line")
	wantResult(t, send(t, owner, userText("line", "", first.ContextID)), a2a.TaskStateCompleted, "without agents line")
}

func TestLocalSandboxWarning(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		sandbox  string
		access   access.Description
		wantWarn bool
	}{
		{name: "docker with others", sandbox: agentprofile.SandboxDocker, access: access.Description{Pairings: 1}},
		{name: "local for you only", sandbox: agentprofile.SandboxLocal, access: access.Description{Pairings: 1, OwnerPairings: 1}},
		{name: "local with a paired user", sandbox: agentprofile.SandboxLocal, access: access.Description{Pairings: 2, OwnerPairings: 1}, wantWarn: true},
		{name: "local open to others", sandbox: agentprofile.SandboxLocal, access: access.Description{Others: []string{"anyone on the public URL or in a direct chat"}}, wantWarn: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := localSandboxWarning(tc.sandbox, &tc.access); (got != "") != tc.wantWarn {
				t.Fatalf("localSandboxWarning() = %q, want a warning %v", got, tc.wantWarn)
			}
		})
	}
}
func TestNewTaskRefusedWhileAllUserAgentsAreBusy(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{})
	ctx := context.Background()
	first := ts.client(ts.pair("u0", auth.RoleMember))
	asked := send(t, first, userText("ask", "", ""))
	for i := 1; i < maxUserAgents; i++ {
		c := ts.client(ts.pair(fmt.Sprintf("u%d", i), auth.RoleMember))
		if task := send(t, c, userText("ask", "", "")); task.Status.State != a2a.TaskStateInputRequired {
			t.Fatalf("task state = %s, want %s", task.Status.State, a2a.TaskStateInputRequired)
		}
	}
	late := ts.client(ts.pair("late", auth.RoleMember))

	if _, err := late.SendMessage(ctx, &a2a.SendMessageRequest{Message: userText("hello", "", "")}); !errors.Is(err, a2a.ErrServerError) {
		t.Fatalf("SendMessage() while all agents are busy error = %v, want %v", err, a2a.ErrServerError)
	}
	list, err := late.ListTasks(ctx, &a2a.ListTasksRequest{})
	if err != nil {
		t.Fatalf("ListTasks() error = %v", err)
	}
	if len(list.Tasks) != 0 {
		t.Fatalf("ListTasks() = %d tasks, want none stored for a refused message", len(list.Tasks))
	}
	if _, err := first.CancelTask(ctx, &a2a.CancelTaskRequest{ID: asked.ID}); err != nil {
		t.Fatalf("CancelTask() error = %v", err)
	}
	var res a2a.SendMessageResult
	waitFor(t, "a message accepted after a process became idle", func() bool {
		res, err = late.SendMessage(ctx, &a2a.SendMessageRequest{Message: userText("hello", "", "")})
		return err == nil
	})
	if task, ok := res.(*a2a.Task); !ok || task.Status.State != a2a.TaskStateCompleted {
		t.Fatalf("SendMessage() after a process became idle = %v, want a completed task", res)
	}
}
