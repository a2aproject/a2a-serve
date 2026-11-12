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
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/go-cmp/cmp"

	sbx "github.com/a2aproject/a2a-serve/internal/sandbox"
	"github.com/a2aproject/a2a-serve/internal/testutil"
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
	if _, marked := sandboxTests.Load(topName(t)); sandbox == "docker" && !marked {
		t.Skip("doesn't depend on the sandbox; mark it with dependsOnSandbox to run it with E2E_SANDBOX=docker")
	}
	home := testutil.ShortTempDir(t)
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
	if dockerHost != "" {
		u.env = append(u.env, "DOCKER_HOST="+dockerHost)
	}
	if relayImage != "" {
		u.env = append(u.env, sbx.EnvForceRelay+"=1", sbx.EnvRelayImage+"="+relayImage)
	}
	u.setUp()
	return u
}

// setUp runs `doctor --fix` and requires the checks of the a2a CLI to pass,
// and the Docker check when the agents run in Docker.
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
	if sandbox == "docker" {
		want["docker"] = "ok"
	}
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
	return decodeTask(out)
}

func decodeTask(out string) (*a2a.Task, error) {
	var resp a2a.StreamResponse
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		return nil, fmt.Errorf("json.Unmarshal(%q): %w", out, err)
	}
	task, ok := resp.Event.(*a2a.Task)
	if !ok {
		return nil, fmt.Errorf("a2a returned %T, want a task", resp.Event)
	}
	return task, nil
}

// getTask gets a task with `a2a task get` and extra flags.
func (u *user) getTask(url, id string, flags ...string) (*a2a.Task, error) {
	out, err := u.exec(u.home, append([]string{"a2a", "task", "get", "-a", url, "-o", "json", id}, flags...)...)
	if err != nil {
		return nil, err
	}
	return decodeTask(out)
}

func (u *user) mustGetTask(url, id string) *a2a.Task {
	u.t.Helper()
	task, err := u.getTask(url, id)
	if err != nil {
		u.t.Fatal(err)
	}
	return task
}

// listTasks lists tasks with `a2a task list`.
func (u *user) listTasks(url string, args ...string) ([]*a2a.Task, error) {
	out, err := u.exec(u.home, append([]string{"a2a", "task", "list", "-a", url, "-o", "json"}, args...)...)
	if err != nil {
		return nil, err
	}
	var resp a2a.ListTasksResponse
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		return nil, fmt.Errorf("json.Unmarshal(%q): %w", out, err)
	}
	return resp.Tasks, nil
}

func (u *user) mustListTasks(url string, args ...string) []*a2a.Task {
	u.t.Helper()
	tasks, err := u.listTasks(url, args...)
	if err != nil {
		u.t.Fatal(err)
	}
	return tasks
}

func (u *user) mustSend(url string, args ...string) *a2a.Task {
	u.t.Helper()
	task, err := u.send(url, args...)
	if err != nil {
		u.t.Fatal(err)
	}
	return task
}

// pairJoin redeems a pairing code for a token to the agent at url.
func (u *user) pairJoin(url, code string) {
	u.t.Helper()
	u.run(u.home, "a2a", "serve", "pair", "join", url, code)
}

// agent is an agent of a user, running in the background.
type agent struct {
	u       *user
	name    string
	dir     string
	url     string
	stopped bool
}

// startAgent starts an `a2a-serve start acp` agent running fakeacp in the
// sandbox of the run, with extra start flags.
func (u *user) startAgent(name string, flags ...string) *agent {
	u.t.Helper()
	a := u.newAgent(name)
	a.start("acp", append(sandboxFlags(), flags...)...)
	return a
}

// newAgent creates the directory of an agent that isn't started yet.
func (u *user) newAgent(name string) *agent {
	u.t.Helper()
	dir := filepath.Join(u.home, name)
	if err := os.Mkdir(dir, 0o700); err != nil {
		u.t.Fatalf("os.Mkdir() error = %v", err)
	}
	return &agent{u: u, name: name, dir: dir}
}

// start runs `a2a-serve start <kind>` with flags in the background.
func (a *agent) start(kind string, flags ...string) {
	a.u.t.Helper()
	a.u.t.Cleanup(a.cleanup)
	a.serve(append([]string{"start", kind, "--name", a.name, "--detach"}, flags...)...)
	a.url = strings.TrimSpace(a.serve("status", "--url=local"))
}

func sandboxFlags() []string {
	if sandbox == "docker" {
		return []string{"--sandbox", "docker", "--image", fakeacpImage, "--command", "/usr/local/bin/fakeacp"}
	}
	return []string{"--sandbox", "local", "--command", filepath.Join(binDir, "fakeacp")}
}

// sandboxTests holds the names of the top-level tests marked with
// dependsOnSandbox.
var sandboxTests sync.Map

// dependsOnSandbox marks a test of what the sandbox changes: how the agent
// process starts, crashes and restarts, what it sees, and how it reaches the
// server. Only marked tests run with E2E_SANDBOX=docker; call it before
// newUser.
func dependsOnSandbox(t *testing.T) {
	sandboxTests.Store(topName(t), true)
}

func topName(t *testing.T) string {
	name, _, _ := strings.Cut(t.Name(), "/")
	return name
}

// requireDocker marks a test of the docker sandbox, and skips it when the
// agents run locally.
func requireDocker(t *testing.T) {
	t.Helper()
	dependsOnSandbox(t)
	if sandbox != "docker" {
		t.Skip("needs E2E_SANDBOX=docker")
	}
}

// serve runs `a2a-serve <args>` in the agent's directory.
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

// invite creates a pairing code with `a2a-serve pair invite`.
func (a *agent) invite(args ...string) string {
	a.u.t.Helper()
	var resp struct {
		Code string `json:"code"`
	}
	a.serveJSON(&resp, append([]string{"pair", "invite"}, args...)...)
	if resp.Code == "" {
		a.u.t.Fatalf("a2a-serve pair invite %v returned no code", args)
	}
	return resp.Code
}

// agentStatus is the output of `a2a-serve status -o json`.
type agentStatus struct {
	Running bool     `json:"running"`
	PID     int      `json:"pid"`
	Pending []string `json:"pending"`
}

func (a *agent) status() agentStatus {
	a.u.t.Helper()
	var status agentStatus
	a.serveJSON(&status, "status")
	return status
}

func (a *agent) running() bool {
	a.u.t.Helper()
	return a.status().Running
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

// eventually polls check until it returns true or the command timeout ends,
// and fails with the last description check returned.
func eventually(t *testing.T, what string, check func() (bool, string)) {
	t.Helper()
	eventuallyWithin(t, cmdTimeout, what, check)
}

// eventuallyWithin is eventually with a timeout of its own.
func eventuallyWithin(t *testing.T, timeout time.Duration, what string, check func() (bool, string)) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		ok, last := check()
		if ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: gave up, last %s", what, last)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

type taskResult struct {
	State a2a.TaskState
	Text  string
}

func resultOf(task *a2a.Task) taskResult {
	return taskResult{State: task.Status.State, Text: testutil.AllArtifactText(task)}
}
