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

package lifecycle

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/agenterr"
	"github.com/a2aproject/a2a-serve/internal/controlapi"
)

// envHungServer makes the test binary act as a server of the agent
// directory it names that holds the lock, ignores SIGTERM and never
// answers.
const envHungServer = "A2A_TEST_HUNG_SERVER"

func TestMain(m *testing.M) {
	if root := os.Getenv(envHungServer); root != "" {
		runHungServer(root)
		return
	}
	os.Exit(m.Run())
}

func runHungServer(root string) {
	signal.Ignore(syscall.SIGTERM)
	dir := agentdir.Dir{Root: root}
	if _, err := dir.Lock(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := os.WriteFile(dir.PIDPath(), []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println("ready")
	time.Sleep(time.Minute)
	os.Exit(1)
}

// hungServer starts a server of dir that ignores SIGTERM.
func hungServer(t *testing.T, dir agentdir.Dir) *fakeProcess {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe)
	cmd.Env = append(os.Environ(), envHungServer+"="+dir.Root)
	cmd.Stderr = os.Stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting the hung server: %v", err)
	}
	p := &fakeProcess{pid: cmd.Process.Pid, done: make(chan struct{})}
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || line != "ready\n" {
		t.Fatalf("the hung server said %q, error = %v", line, err)
	}
	go func() {
		defer close(p.done)
		var ee *exec.ExitError
		if err := cmd.Wait(); err != nil && !errors.As(err, &ee) {
			t.Errorf("waiting for the hung server: %v", err)
		}
	}()
	t.Cleanup(func() {
		if err := cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			t.Errorf("killing the hung server: %v", err)
		}
		<-p.done
	})
	return p
}

// hangingControl accepts control calls and never answers them.
type hangingControl struct{}

func (hangingControl) ServeHTTP(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }

func TestStopKillsServerThatDoesNotAnswer(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("unix sockets are not used on this platform")
	}
	dir := newAgent(t)
	serveControl(t, dir, hangingControl{})
	proc := hungServer(t, dir)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	got, err := Stop(ctx, dir, controlapi.NewClient(dir, controlapi.WithTimeout(100*time.Millisecond)), StopOptions{Wait: WaitStopped, KillAfter: 100 * time.Millisecond, Services: fakeServices{}})
	if err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	if !got.Running || !got.Unresponsive {
		t.Fatalf("Stop() = %+v, want a running server that didn't answer", got)
	}
	if !proc.exited() {
		t.Fatalf("Stop() did not kill the server")
	}
}

func TestStopErrorOfServerThatDoesNotAnswer(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("unix sockets are not used on this platform")
	}
	dir := newAgent(t)
	serveControl(t, dir, hangingControl{})

	_, err := Stop(t.Context(), dir, controlapi.NewClient(dir, controlapi.WithTimeout(100*time.Millisecond)), StopOptions{Services: fakeServices{}})
	if !errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), "http://") {
		t.Fatalf("Stop() error = %v, want a timeout without the internal URL", err)
	}
}

func TestRestartOfServerThatDoesNotAnswerPointsToStop(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("unix sockets are not used on this platform")
	}
	dir := newAgent(t)
	serveControl(t, dir, hangingControl{})
	hungServer(t, dir)

	err := Restart(t.Context(), dir, controlapi.NewClient(dir, controlapi.WithTimeout(100*time.Millisecond)), fakeServices{}, false, io.Discard)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Restart() error = %v, want a timeout", err)
	}
	if got, want := agenterr.HintText(err), "the server doesn't answer; stop it with `a2a-serve stop`, then start it with `a2a-serve restart`"; got != want {
		t.Fatalf("agenterr.HintText() = %q, want %q", got, want)
	}
}

func TestUnanswered(t *testing.T) {
	t.Parallel()
	timeout := fmt.Errorf("status: %w", context.DeadlineExceeded)
	tests := []struct {
		name     string
		locked   bool
		err      error
		wantHint string
	}{
		{name: "server holds the directory", locked: true, err: timeout, wantHint: "the server doesn't answer; stop it with `a2a-serve stop`, then start it with `a2a-serve restart`"},
		{name: "no server", err: timeout},
		{name: "other error", locked: true, err: errors.New("broken")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := newAgent(t)
			if tc.locked {
				release, err := dir.Lock()
				if err != nil {
					t.Fatalf("Lock() error = %v", err)
				}
				t.Cleanup(func() {
					if err := release(); err != nil {
						t.Errorf("release() error = %v", err)
					}
				})
			}

			got := Unanswered(dir, tc.err)

			if !errors.Is(got, tc.err) {
				t.Fatalf("Unanswered() = %v, want it to wrap %v", got, tc.err)
			}
			if hint := agenterr.HintText(got); hint != tc.wantHint {
				t.Fatalf("agenterr.HintText(Unanswered()) = %q, want %q", hint, tc.wantHint)
			}
		})
	}
}
