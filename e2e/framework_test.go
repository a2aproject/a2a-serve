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

//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-serve/internal/testutil"
	"github.com/google/go-cmp/cmp"
)

const cmdTimeout = time.Minute

// user is a person on a machine of their own: a HOME with the a2a CLI
// configured by `a2a-serve doctor --fix`.
type user struct {
	t    *testing.T
	home string
	env  []string
}

func newUser(t *testing.T) *user {
	t.Helper()
	home, err := os.MkdirTemp("", "a2a-e2e-home-")
	if err != nil {
		t.Fatalf("os.MkdirTemp() error = %v", err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(home); err != nil {
			t.Errorf("os.RemoveAll(%q) error = %v", home, err)
		}
	})
	runtimeDir := filepath.Join(home, "run")
	if err := os.Mkdir(runtimeDir, 0o700); err != nil {
		t.Fatalf("os.Mkdir() error = %v", err)
	}
	u := &user{t: t, home: home, env: []string{
		"HOME=" + home,
		"XDG_RUNTIME_DIR=" + runtimeDir,
		"PATH=" + binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"TMPDIR=" + os.TempDir(),
	}}
	u.setUp()
	return u
}

// setUp runs `doctor --fix` and requires the checks of the a2a CLI to pass;
// the Docker check may fail, as the agents run with --sandbox local.
func (u *user) setUp() {
	u.t.Helper()
	out, err := u.exec(u.home, "a2a-serve", "doctor", "--fix", "-o", "json")
	var checks []struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	if jsonErr := json.Unmarshal([]byte(out), &checks); jsonErr != nil {
		u.t.Fatalf("a2a-serve doctor --fix error = %v, output %q: %v", err, out, jsonErr)
	}
	got := map[string]string{}
	for _, c := range checks {
		got[c.ID] = c.Status
	}
	want := map[string]string{"a2a": "ok", "plugins": "ok", "a2a-serve": "ok", "credential-helper": "ok"}
	for id := range got {
		if _, ok := want[id]; !ok {
			delete(got, id)
		}
	}
	if diff := cmp.Diff(want, got); diff != "" {
		u.t.Fatalf("a2a-serve doctor --fix wrong result (-want +got) diff = %s", diff)
	}
}

// exec runs a binary of binDir as the user and returns its stdout.
func (u *user) exec(dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), cmdTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, filepath.Join(binDir, args[0]), args[1:]...)
	cmd.Dir = dir
	cmd.Env = u.env
	// A detached server inherits no pipes, but don't hang on one which does.
	cmd.WaitDelay = time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("%s: %w\nstdout: %s\nstderr: %s", strings.Join(args, " "), err, stdout.String(), stderr.String())
	}
	return stdout.String(), nil
}

func (u *user) run(dir string, args ...string) string {
	u.t.Helper()
	out, err := u.exec(dir, args...)
	if err != nil {
		u.t.Fatal(err)
	}
	return out
}

func (u *user) runJSON(dir string, v any, args ...string) {
	u.t.Helper()
	out := u.run(dir, append(args, "-o", "json")...)
	if err := json.Unmarshal([]byte(out), v); err != nil {
		u.t.Fatalf("json.Unmarshal(%q) error = %v", out, err)
	}
}

// send sends a message with `a2a send` and returns the task it ends with.
func (u *user) send(url string, args ...string) (*a2a.Task, error) {
	out, err := u.exec(u.home, append([]string{"a2a", "send", "-a", url, "-o", "json"}, args...)...)
	if err != nil {
		return nil, err
	}
	var resp a2a.StreamResponse
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		return nil, fmt.Errorf("json.Unmarshal(%q): %w", out, err)
	}
	task, ok := resp.Event.(*a2a.Task)
	if !ok {
		return nil, fmt.Errorf("a2a send returned %T, want a task", resp.Event)
	}
	return task, nil
}

func (u *user) mustSend(url string, args ...string) *a2a.Task {
	u.t.Helper()
	task, err := u.send(url, args...)
	if err != nil {
		u.t.Fatal(err)
	}
	return task
}

// tokensAdd redeems a pairing code for a token to the agent at url.
func (u *user) tokensAdd(url, code string) {
	u.t.Helper()
	u.run(u.home, "a2a", "serve", "tokens", "add", url, code)
}

// agent is an `a2a serve run acp` agent of a user, running fakeacp in the
// background without a sandbox.
type agent struct {
	u       *user
	name    string
	dir     string
	url     string
	stopped bool
}

func (u *user) startAgent(name string) *agent {
	u.t.Helper()
	dir := filepath.Join(u.home, name)
	if err := os.Mkdir(dir, 0o700); err != nil {
		u.t.Fatalf("os.Mkdir() error = %v", err)
	}
	a := &agent{u: u, name: name, dir: dir}
	u.t.Cleanup(a.cleanup)
	a.serve("run", "acp", "--name", name, "--sandbox", "local", "--command", filepath.Join(binDir, "fakeacp"), "--detach")
	a.url = strings.TrimSpace(a.serve("url", "--local"))
	return a
}

// serve runs `a2a serve <args>` in the agent's directory.
func (a *agent) serve(args ...string) string {
	a.u.t.Helper()
	return a.u.run(a.dir, append([]string{"a2a", "serve"}, args...)...)
}

func (a *agent) serveJSON(v any, args ...string) {
	a.u.t.Helper()
	a.u.runJSON(a.dir, v, append([]string{"a2a", "serve"}, args...)...)
}

func (a *agent) stop() {
	a.u.t.Helper()
	a.serve("stop")
	a.stopped = true
}

// invite creates a pairing code with `a2a serve pair invite`.
func (a *agent) invite(args ...string) string {
	a.u.t.Helper()
	var resp struct {
		Code string `json:"code"`
	}
	a.serveJSON(&resp, append([]string{"pair", "invite"}, args...)...)
	if resp.Code == "" {
		a.u.t.Fatalf("a2a serve pair invite %v returned no code", args)
	}
	return resp.Code
}

func (a *agent) running() bool {
	a.u.t.Helper()
	var status struct {
		Running bool `json:"running"`
	}
	a.serveJSON(&status, "status")
	return status.Running
}

func (a *agent) cleanup() {
	t := a.u.t
	if t.Failed() {
		logs, err := a.u.exec(a.dir, "a2a", "serve", "logs")
		t.Logf("%s logs (error %v):\n%s", a.name, err, logs)
	}
	if a.stopped {
		return
	}
	if _, err := a.u.exec(a.dir, "a2a", "serve", "stop"); err != nil {
		t.Errorf("stop %s: %v", a.name, err)
	}
}

type taskResult struct {
	State a2a.TaskState
	Text  string
}

func resultOf(task *a2a.Task) taskResult {
	return taskResult{State: task.Status.State, Text: testutil.AllArtifactText(task)}
}
