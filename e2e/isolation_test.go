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
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/go-cmp/cmp"
)

func TestPairedUserCantReachOwnerTasks(t *testing.T) {
	t.Parallel()
	owner, anna := newUser(t), newUser(t)
	coder := owner.startAgent("coder")
	anna.pairJoin(coder.url, coder.invite("anna"))
	own := anna.mustSend(coder.url, "hello")
	task := owner.mustSend(coder.url, "ask")
	id := string(task.ID)

	var listed []a2a.TaskID
	for _, lt := range anna.mustListTasks(coder.url) {
		listed = append(listed, lt.ID)
	}
	if diff := cmp.Diff([]a2a.TaskID{own.ID}, listed); diff != "" {
		t.Fatalf("a2a task list by anna wrong result (-want +got) diff = %s", diff)
	}
	if _, err := anna.getTask(coder.url, id); err == nil || !strings.Contains(err.Error(), "task not found") {
		t.Fatalf("a2a task get of the owner's task by anna error = %v, want task not found", err)
	}
	if _, err := anna.send(coder.url, "--task-id", id, "once"); err == nil || !strings.Contains(err.Error(), "task not found") {
		t.Fatalf("a2a send --task-id of the owner's approval by anna error = %v, want task not found", err)
	}
	if _, err := anna.send(coder.url, "--task-id", "01a1b049-0000-7000-8000-000000000000", "once"); err == nil || !strings.Contains(err.Error(), "task not found") {
		t.Fatalf("a2a send --task-id of an unknown task by anna error = %v, want task not found", err)
	}
	if got := owner.mustGetTask(coder.url, id); got.Status.State != a2a.TaskStateInputRequired {
		t.Fatalf("a2a task get after anna's answer state = %v, want %v", got.Status.State, a2a.TaskStateInputRequired)
	}

	got := resultOf(owner.mustSend(coder.url, "--task-id", id, "once"))
	want := taskResult{State: a2a.TaskStateCompleted, Text: "Pushed (allow)."}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("a2a send --task-id by the owner wrong result (-want +got) diff = %s", diff)
	}
}

func TestPairedUserRunsInOwnAgentProcess(t *testing.T) {
	t.Parallel()
	dependsOnSandbox(t)
	owner, anna := newUser(t), newUser(t)
	coder := owner.startAgent("coder")
	anna.pairJoin(coder.url, coder.invite("anna"))

	ownerPID := resultOf(owner.mustSend(coder.url, "pid")).Text
	annaPID := resultOf(anna.mustSend(coder.url, "pid")).Text
	if ownerPID == annaPID {
		t.Fatalf("a2a send pid by the owner and anna = %q, want different agent processes", ownerPID)
	}
	if got := resultOf(anna.mustSend(coder.url, "pid")).Text; got != annaPID {
		t.Fatalf("a2a send pid in anna's second conversation = %q, want her process %q", got, annaPID)
	}
}

func TestConversationHandleWorksOnlyInItsTurn(t *testing.T) {
	t.Parallel()
	dependsOnSandbox(t)
	owner := newUser(t)
	coder, reviewer := owner.startAgent("coder"), owner.startAgent("reviewer")
	coder.serve("contact", "add", "reviewer", "--url", reviewer.url, "--code", reviewer.invite("coder"))

	first := owner.mustSend(coder.url, "acall reviewer ask")
	reviewerTask := calledTaskID(t, first)
	firstURL, socket := coder.agentsOf(owner.mustSend(coder.url, "--context-id", first.ContextID, "agents"))
	if _, err := owner.send(firstURL+"/reviewer", "--unix-socket", socket, "hello"); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("a2a send through the agents URL after its turn error = %v, want 403", err)
	}

	second := owner.mustSend(coder.url, "agents")
	secondURL, _ := coder.agentsOf(second)
	waiting := owner.mustSend(coder.url, "--context-id", second.ContextID, "ask")
	if got := resultOf(owner.mustSend(secondURL+"/reviewer", "--unix-socket", socket, "hello")); got.Text != "Hello from fakeacp." {
		t.Fatalf("a2a send through the agents URL during its turn = %+v, want the reviewer's answer", got)
	}
	if _, err := owner.getTask(secondURL+"/reviewer", reviewerTask, "--unix-socket", socket); err == nil || !strings.Contains(err.Error(), "this conversation didn't start task") {
		t.Fatalf("a2a task get of another conversation's task error = %v, want it refused", err)
	}
	if _, err := owner.getTask(firstURL+"/reviewer", reviewerTask, "--unix-socket", socket); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("a2a task get through the agents URL after its turn error = %v, want 403", err)
	}
	owner.mustSend(coder.url, "--task-id", string(waiting.ID), "once")
}

func TestAgentsURLRefusalReachesTheAgent(t *testing.T) {
	t.Parallel()
	dependsOnSandbox(t)
	owner := newUser(t)
	coder := owner.startAgent("coder")
	turn := owner.mustSend(coder.url, "agents")
	agentsURL, socket := coder.agentsOf(turn)
	waiting := owner.mustSend(coder.url, "--context-id", turn.ContextID, "ask")

	_, err := owner.exec(owner.home, "a2a", "send", "-a", agentsURL+"/nosuch", "--unix-socket", socket, "hi")
	want := "stderr: Error: failed to send message: \"nosuch\" is not an agent you can call; the [a2a agents] line lists them\n"
	if err == nil || !strings.HasSuffix(err.Error(), want) {
		t.Fatalf("a2a send to an unknown agent error = %v, want it to end with %q", err, want)
	}
	owner.mustSend(coder.url, "--task-id", string(waiting.ID), "once")
}

var calledTaskRE = regexp.MustCompile(`tool result: task (\S+) `)

// calledTaskID returns the ID of the task a call or acall prompt started.
func calledTaskID(t *testing.T, task *a2a.Task) string {
	t.Helper()
	text := resultOf(task).Text
	m := calledTaskRE.FindStringSubmatch(text)
	if m == nil {
		t.Fatalf("call result = %q, want a called task", text)
	}
	return m[1]
}

// agentsOf returns the agents URL of the answer to the agents prompt, and the
// host path of the socket that reaches it from the agent process.
func (a *agent) agentsOf(task *a2a.Task) (url, socket string) {
	t := a.u.t
	t.Helper()
	text := resultOf(task).Text
	fields := strings.Fields(text)
	if len(fields) != 3 {
		t.Fatalf("a2a send agents = %q, want the agents URL, the socket and the hostname", text)
	}
	url, socket, host := fields[0], fields[1], fields[2]
	if sandbox != "docker" {
		return url, socket
	}
	// The hostname of a container is its ID, and its socket directory is a
	// mount of the process's own directory on the host.
	format := fmt.Sprintf(`{{range .Mounts}}{{if eq .Destination %q}}{{.Source}}{{end}}{{end}}`, filepath.Dir(socket))
	out, err := exec.Command("docker", "inspect", "--format", format, host).Output()
	if err != nil {
		t.Fatalf("docker inspect %s error = %v", host, err)
	}
	src := strings.TrimSpace(string(out))
	if src == "" {
		t.Fatalf("docker inspect %s has no mount at %s", host, filepath.Dir(socket))
	}
	hostSocket := filepath.Join(src, filepath.Base(socket))
	if len(hostSocket) < 100 {
		return url, hostSocket
	}
	// A relay's socket is in a Docker volume, whose path can be too long
	// for a Unix socket address; a short symlink leads to it.
	link := filepath.Join(a.u.home, "run", "a2a.sock")
	if err := os.Remove(link); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if err := os.Symlink(hostSocket, link); err != nil {
		t.Fatal(err)
	}
	return url, link
}
