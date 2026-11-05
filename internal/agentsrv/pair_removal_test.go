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

	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/controlapi"
)

// streamSlow starts a task that runs until it is stopped, and returns once
// it runs. The channel gets the task's last state when its stream ends.
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
	text := resultText(task)
	pid, err := strconv.Atoi(text[strings.LastIndex(text, "/")+1:])
	if err != nil {
		t.Fatalf("parsing the pid in %q: %v", resultText(task), err)
	}
	return pid
}

func processRuns(pid int) bool {
	return !errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
}

func TestPairRmStopsTheUsersAgentAndDeletesItsHome(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{})
	ctx := context.Background()
	anna := ts.client(ts.pair("anna", auth.RoleAuthenticated))
	pid := pidOf(t, send(t, anna, userText("pid", "", "")))
	home := ts.dir.UserHomeDir("pair:anna")
	marker := filepath.Join(home, "history")
	if err := os.WriteFile(marker, []byte("anna's conversations"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	ended := streamSlow(t, anna)

	if _, err := controlapi.NewClient(ts.dir).Revoke(ctx, "anna"); err != nil {
		t.Fatalf("Revoke() error = %v", err)
	}

	wantEnded(t, ended, a2a.TaskStateCanceled)
	waitFor(t, "anna's agent process to stop", func() bool { return !processRuns(pid) })
	if _, err := os.Stat(home); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Stat() of anna's agent home error = %v, want %v", err, os.ErrNotExist)
	}
	newAnna := ts.client(ts.pair("anna", auth.RoleAuthenticated))
	if got := pidOf(t, send(t, newAnna, userText("pid", "", ""))); got == pid {
		t.Fatalf("the new anna ran in process %d, want a new process", got)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Stat() of the old anna's file in the new anna's home error = %v, want %v", err, os.ErrNotExist)
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

	_, err := ts.srv.startUserAgent(context.Background(), userAgentKeyPrefix+auth.PairPrincipal("gone"), io.Discard)

	if err == nil {
		t.Fatalf("startUserAgent() of a removed pairing error = nil, want an error")
	}
	if _, err := os.Stat(ts.dir.UserHomeDir(auth.PairPrincipal("gone"))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Stat() of the removed pairing's agent home error = %v, want %v", err, os.ErrNotExist)
	}
}
