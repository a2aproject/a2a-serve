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
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/a2atext"
	"github.com/a2aproject/a2a-serve/internal/agentkey"
	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/controlapi"
)

// streamSlow starts a task that runs until it is stopped, and returns once
// it runs. The messenger gets the task's last state when its stream ends.
func streamSlow(t *testing.T, c *a2aclient.Client) <-chan a2a.TaskState {
	t.Helper()
	working := make(chan struct{})
	ended := make(chan a2a.TaskState, 1)
	go func() {
		var state a2a.TaskState
		started := false
		for ev, err := range c.SendStreamingMessage(context.Background(), &a2a.SendMessageRequest{Message: userText("slow", "", "")}) {
			if err != nil {
				break
			}
			switch ev := ev.(type) {
			case *a2a.Task:
				state = ev.Status.State
			case *a2a.TaskStatusUpdateEvent:
				state = ev.Status.State
			case *a2a.TaskArtifactUpdateEvent:
				if !started {
					started = true
					close(working)
				}
			}
		}
		ended <- state
	}()
	select {
	case <-working:
	case <-time.After(waitTimeout):
		t.Fatal("timed out waiting for the slow task to run")
	}
	return ended
}

func wantEnded(t *testing.T, ended <-chan a2a.TaskState, want a2a.TaskState) {
	t.Helper()
	select {
	case got := <-ended:
		if got != want {
			t.Fatalf("task state at the end of its stream = %s, want %s", got, want)
		}
	case <-time.After(waitTimeout):
		t.Fatalf("task still runs after its pairing was removed, want %s", want)
	}
}

func pidOf(t *testing.T, task *a2a.Task) int {
	t.Helper()
	text := a2atext.Result(task)
	pid, err := strconv.Atoi(text[strings.LastIndex(text, "/")+1:])
	if err != nil {
		t.Fatalf("parsing the pid in %q: %v", a2atext.Result(task), err)
	}
	return pid
}

func processRuns(pid int) bool {
	return !errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
}

func harnessSocketDirs(t *testing.T, ts *testServer) []string {
	t.Helper()
	entries, err := os.ReadDir(ts.srv.harness.dir)
	if err != nil {
		t.Fatalf("ReadDir() error = %v", err)
	}
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name()
	}
	return names
}

func TestPairRmPreviewChangesNothing(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{})
	ctx := context.Background()
	anna := ts.client(ts.pair("anna", auth.RoleMember))
	pid := pidOf(t, send(t, anna, userText("pid", "", "")))
	home := ts.dir.UserHomeDir("pair:anna")

	got, err := controlapi.NewClient(ts.dir).PreviewRevoke(ctx, "anna")
	if err != nil {
		t.Fatalf("PreviewRevoke() error = %v", err)
	}

	if diff := cmp.Diff([]string{home}, got.AgentHomes); diff != "" {
		t.Fatalf("PreviewRevoke() agent homes wrong result (-want +got) diff = %s", diff)
	}
	if got.Tasks != 1 || got.Tokens != 1 {
		t.Fatalf("PreviewRevoke() = %+v, want 1 task and 1 token", got)
	}
	if !processRuns(pid) {
		t.Fatalf("anna's agent process %d stopped by PreviewRevoke(), want it running", pid)
	}
	if _, err := os.Stat(home); err != nil {
		t.Fatalf("Stat() of anna's agent home after PreviewRevoke() error = %v, want it kept", err)
	}
	if _, err := controlapi.NewClient(ts.dir).PreviewRevoke(ctx, "nobody"); err == nil {
		t.Fatalf("PreviewRevoke() of an unknown name error = nil, want error")
	}
}

func TestPairRmStopsTheUsersAgentAndDeletesItsHome(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{})
	ctx := context.Background()
	anna := ts.client(ts.pair("anna", auth.RoleMember))
	pid := pidOf(t, send(t, anna, userText("pid", "", "")))
	home := ts.dir.UserHomeDir("pair:anna")
	marker := filepath.Join(home, "history")
	if err := os.WriteFile(marker, []byte("anna's conversations"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	ended := streamSlow(t, anna)
	sockets := harnessSocketDirs(t, ts)

	if _, err := controlapi.NewClient(ts.dir).Revoke(ctx, "anna"); err != nil {
		t.Fatalf("Revoke() error = %v", err)
	}

	wantEnded(t, ended, a2a.TaskStateCanceled)
	waitFor(t, "anna's agent process to stop", func() bool { return !processRuns(pid) })
	if got := harnessSocketDirs(t, ts); len(got) != len(sockets)-1 {
		t.Fatalf("harness socket directories after removing anna = %v, want one fewer than %v", got, sockets)
	}
	if _, err := os.Stat(home); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Stat() of anna's agent home error = %v, want %v", err, os.ErrNotExist)
	}
	newAnna := ts.client(ts.pair("anna", auth.RoleMember))
	if got := pidOf(t, send(t, newAnna, userText("pid", "", ""))); got == pid {
		t.Fatalf("the new anna ran in process %d, want a new process", got)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Stat() of the old anna's file in the new anna's home error = %v, want %v", err, os.ErrNotExist)
	}
}

func TestPairRmDeletesTheOwnersTasksInTheUsersConversation(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{})
	ctx := context.Background()
	anna := ts.client(ts.pair("anna", auth.RoleMember))
	boss := ts.client(ts.pair("boss", auth.RoleOwner))
	started := send(t, anna, userText("pid", "", ""))
	joined := send(t, boss, userText("pid", "", started.ContextID))
	if got, want := pidOf(t, joined), pidOf(t, started); got != want {
		t.Fatalf("the owner's message in anna's conversation ran in process %d, want anna's process %d", got, want)
	}

	if _, err := controlapi.NewClient(ts.dir).Revoke(ctx, "anna"); err != nil {
		t.Fatalf("Revoke() error = %v", err)
	}

	if _, err := boss.GetTask(ctx, &a2a.GetTaskRequest{ID: joined.ID}); !errors.Is(err, a2a.ErrTaskNotFound) {
		t.Fatalf("GetTask() of the owner's task in anna's conversation error = %v, want %v", err, a2a.ErrTaskNotFound)
	}
	newAnna := ts.client(ts.pair("anna", auth.RoleMember))
	newPid := pidOf(t, send(t, newAnna, userText("pid", "", "")))
	if got := pidOf(t, send(t, boss, userText("pid", "", started.ContextID))); got == newPid {
		t.Fatalf("the owner's message in the old anna's conversation ran in the new anna's process %d", got)
	}
}

func TestPairRmCancelsTasksOfOwnerPairing(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{})
	boss := ts.client(ts.pair("boss", auth.RoleOwner))
	ended := streamSlow(t, boss)

	if _, err := controlapi.NewClient(ts.dir).Revoke(context.Background(), "boss"); err != nil {
		t.Fatalf("Revoke() error = %v", err)
	}

	wantEnded(t, ended, a2a.TaskStateCanceled)
}

func TestUserAgentOfRemovedPairingDoesNotStart(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{})

	_, err := ts.srv.startPoolAgent(context.Background(), &ts.srv.users, agentkey.User(auth.PairPrincipal("gone")), io.Discard)

	if err == nil {
		t.Fatalf("startPoolAgent() of a removed pairing error = nil, want an error")
	}
	if _, err := os.Stat(ts.dir.UserHomeDir(auth.PairPrincipal("gone"))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Stat() of the removed pairing's agent home error = %v, want %v", err, os.ErrNotExist)
	}
}
