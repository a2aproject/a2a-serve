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

package a2arun_test

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/core/a2aserve"
	"github.com/a2aproject/a2a-serve/core/approval"
	"github.com/a2aproject/a2a-serve/core/fsys"
	"github.com/a2aproject/a2a-serve/internal/a2arun/serve"
	"github.com/a2aproject/a2a-serve/internal/testutil"
)

// startWorkspace runs a2a-run with fakeacp on the local sandbox and the
// workspace args, and returns a client of its endpoint.
func startWorkspace(t *testing.T, args ...string) (*running, *a2aclient.Client) {
	t.Helper()
	sock := filepath.Join(testutil.ShortTempDir(t), "a2a.sock")
	r := start(t, append([]string{"--sandbox", "local", "--command", fakeACP, "--listen", "unix://" + sock}, args...)...)
	if !r.ready.Ready {
		t.Fatalf("ready line = %+v, want ready; stderr:\n%s", r.ready, r.stderr)
	}
	c, err := a2aclient.NewFromEndpoints(context.Background(), []*a2a.AgentInterface{a2a.NewAgentInterface("http://a2a/", a2a.TransportProtocolJSONRPC)}, a2aclient.WithJSONRPCTransport(unixClient(sock)))
	if err != nil {
		t.Fatalf("a2aclient.NewFromEndpoints() error = %v", err)
	}
	return r, c
}

// sendWant sends msg and returns the task, which must be in state want.
func sendWant(t *testing.T, c *a2aclient.Client, msg *a2a.Message, want a2a.TaskState) *a2a.Task {
	t.Helper()
	res, err := c.SendMessage(context.Background(), &a2a.SendMessageRequest{Message: msg})
	if err != nil {
		t.Fatalf("Client.SendMessage() error = %v", err)
	}
	task, ok := res.(*a2a.Task)
	if !ok {
		t.Fatalf("Client.SendMessage() = %T, want a task", res)
	}
	if task.Status.State != want {
		t.Fatalf("Client.SendMessage() state = %v, want %v; status %q", task.Status.State, want, statusText(task))
	}
	return task
}

func textIn(contextID, text string) *a2a.Message {
	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(text))
	msg.ContextID = contextID
	return msg
}

// allow answers the permission request of task with allow once.
func allow(t *testing.T, task *a2a.Task) *a2a.Message {
	t.Helper()
	r := approval.FromStatus(task.Status)
	if r == nil {
		t.Fatalf("approval.FromStatus() = nil, want the request of task %s", task.ID)
	}
	msg, err := approval.Answer(task, r, approval.OptionOnce)
	if err != nil {
		t.Fatalf("approval.Answer() error = %v", err)
	}
	return msg
}

// changeset is the changes artifact of a task without fakeacp's own
// session files.
type changeset struct {
	Files   map[string]string
	Removed []string
}

func changesOf(t *testing.T, task *a2a.Task) *changeset {
	t.Helper()
	var got *changeset
	for _, art := range task.Artifacts {
		if art.Name != serve.ChangesArtifact {
			continue
		}
		if got != nil {
			t.Fatalf("task %s has two %s artifacts", task.ID, serve.ChangesArtifact)
		}
		got = &changeset{Files: map[string]string{}}
		for _, p := range art.Parts {
			if data, ok := p.Data().(map[string]any); ok {
				for _, r := range data["removed"].([]any) {
					got.Removed = append(got.Removed, r.(string))
				}
				continue
			}
			if !strings.HasPrefix(p.Filename, ".fakeacp/") {
				got.Files[p.Filename] = string(p.Raw())
			}
		}
	}
	return got
}

func TestMemoryWorkspaceReturnsTheChangesOfEachTurn(t *testing.T) {
	t.Parallel()
	r, c := startWorkspace(t, "--workspace", "memory")
	if r.ready.Workspace != serve.WorkspaceMemory || r.ready.WorkspaceSync != serve.SyncTurn {
		t.Fatalf("ready line = %+v, want workspace memory with turn sync", r.ready)
	}

	written := sendWant(t, c, textIn("context-a", "write hello.txt"), a2a.TaskStateCompleted)
	renamed := sendWant(t, c, textIn("context-a", "rename hello.txt bye.txt"), a2a.TaskStateCompleted)

	want := &changeset{Files: map[string]string{"hello.txt": "written by fakeacp\n"}}
	if diff := cmp.Diff(want, changesOf(t, written)); diff != "" {
		t.Fatalf("changes of write wrong result (-want +got) diff = %s", diff)
	}
	want = &changeset{Files: map[string]string{"bye.txt": "written by fakeacp\n"}, Removed: []string{"hello.txt"}}
	if diff := cmp.Diff(want, changesOf(t, renamed)); diff != "" {
		t.Fatalf("changes of rename wrong result (-want +got) diff = %s", diff)
	}
}

func TestTurnSyncKeepsAContextsChangesFromOthersUntilItsTurnEnds(t *testing.T) {
	t.Parallel()
	_, c := startWorkspace(t, "--workspace", "memory")

	waiting := sendWant(t, c, textIn("context-a", "writeask hello.txt"), a2a.TaskStateInputRequired)
	before := testutil.AllArtifactText(sendWant(t, c, textIn("context-b", "read hello.txt"), a2a.TaskStateCompleted))
	sendWant(t, c, allow(t, waiting), a2a.TaskStateCompleted)
	after := testutil.AllArtifactText(sendWant(t, c, textIn("context-b", "read hello.txt"), a2a.TaskStateCompleted))

	if !strings.HasPrefix(before, "read error:") {
		t.Fatalf("read hello.txt in context-b during the turn of context-a = %q, want a read error", before)
	}
	if after != "written by fakeacp\n" {
		t.Fatalf("read hello.txt in context-b after the turn of context-a = %q, want the file of context-a", after)
	}
}

func TestDirWorkspaceWithTurnSyncChangesTheDirectoryAfterTheTurn(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	r, c := startWorkspace(t, "--workspace", "dir:"+dir)
	if diff := cmp.Diff(serve.Ready{Ready: true, A2A: r.ready.A2A, Sandbox: "local", Workspace: "dir", WorkspaceSync: "turn"}, r.ready); diff != "" {
		t.Fatalf("ready line wrong result (-want +got) diff = %s", diff)
	}

	waiting := sendWant(t, c, textIn("context-a", "writeask hello.txt"), a2a.TaskStateInputRequired)
	during, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	done := sendWant(t, c, allow(t, waiting), a2a.TaskStateCompleted)

	if len(during) > 0 {
		t.Fatalf("os.ReadDir(workspace) during the turn = %v, want it empty", during)
	}
	got, err := os.ReadFile(filepath.Join(dir, "hello.txt"))
	if err != nil || string(got) != "written by fakeacp\n" {
		t.Fatalf("os.ReadFile(hello.txt) after the turn = %q, %v, want the agent's file", got, err)
	}
	want := &changeset{Files: map[string]string{"hello.txt": "written by fakeacp\n"}}
	if diff := cmp.Diff(want, changesOf(t, done)); diff != "" {
		t.Fatalf("changes of the turn wrong result (-want +got) diff = %s", diff)
	}
}

func TestDirWorkspaceWithLiveSyncChangesTheDirectoryInPlace(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	r, c := startWorkspace(t, "--workspace", "dir:"+dir, "--workspace-sync", "live")
	if r.ready.WorkspaceSync != serve.SyncLive {
		t.Fatalf("ready line = %+v, want live sync", r.ready)
	}

	waiting := sendWant(t, c, textIn("context-a", "writeask hello.txt"), a2a.TaskStateInputRequired)
	got, err := os.ReadFile(filepath.Join(dir, "hello.txt"))
	done := sendWant(t, c, allow(t, waiting), a2a.TaskStateCompleted)

	if err != nil || string(got) != "written by fakeacp\n" {
		t.Fatalf("os.ReadFile(hello.txt) during the turn = %q, %v, want the agent's file", got, err)
	}
	if changes := changesOf(t, done); changes != nil {
		t.Fatalf("changes of a live turn = %+v, want no changes artifact", changes)
	}
}

// serveTree serves tree with the workspace protocol on a Unix socket and
// returns its URL.
func serveTree(t *testing.T, tree fsys.FS) string {
	t.Helper()
	ln, err := net.Listen("unix", filepath.Join(testutil.ShortTempDir(t), "fs.sock"))
	if err != nil {
		t.Fatalf("net.Listen() error = %v", err)
	}
	srv := httptest.NewUnstartedServer(fsys.Handler(tree))
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)
	return "unix://" + ln.Addr().String()
}

// readTree returns the contents of the file p of tree, or the error.
func readTree(t *testing.T, tree fsys.FS, p string) (string, error) {
	t.Helper()
	rc, err := tree.Open(context.Background(), p)
	if err != nil {
		return "", err
	}
	data, err := io.ReadAll(rc)
	if cerr := rc.Close(); err == nil {
		err = cerr
	}
	return string(data), err
}

func TestURLWorkspaceWithTurnSyncChangesTheHostsTreeAfterTheTurn(t *testing.T) {
	t.Parallel()
	tree := fsys.Memory()
	if _, err := tree.Write(context.Background(), "host.txt", strings.NewReader("from the host\n"), 0o644, ""); err != nil {
		t.Fatal(err)
	}
	r, c := startWorkspace(t, "--workspace", serveTree(t, tree))
	if diff := cmp.Diff(serve.Ready{Ready: true, A2A: r.ready.A2A, Sandbox: "local", Workspace: "url", WorkspaceSync: "turn"}, r.ready); diff != "" {
		t.Fatalf("ready line wrong result (-want +got) diff = %s", diff)
	}

	read := sendWant(t, c, textIn("context-a", "read host.txt"), a2a.TaskStateCompleted)
	waiting := sendWant(t, c, textIn("context-a", "writeask hello.txt"), a2a.TaskStateInputRequired)
	_, duringErr := readTree(t, tree, "hello.txt")
	done := sendWant(t, c, allow(t, waiting), a2a.TaskStateCompleted)

	if got := testutil.AllArtifactText(read); got != "from the host\n" {
		t.Fatalf("read host.txt = %q, want the host's file", got)
	}
	if !errors.Is(duringErr, fs.ErrNotExist) {
		t.Fatalf("reading hello.txt of the host's tree during the turn error = %v, want %v", duringErr, fs.ErrNotExist)
	}
	if got, err := readTree(t, tree, "hello.txt"); err != nil || got != "written by fakeacp\n" {
		t.Fatalf("reading hello.txt of the host's tree after the turn = %q, %v, want the agent's file", got, err)
	}
	want := &changeset{Files: map[string]string{"hello.txt": "written by fakeacp\n"}}
	if diff := cmp.Diff(want, changesOf(t, done)); diff != "" {
		t.Fatalf("changes of the turn wrong result (-want +got) diff = %s", diff)
	}
}

func TestURLWorkspaceConflictFailsTheTask(t *testing.T) {
	t.Parallel()
	tree := fsys.Memory()
	_, c := startWorkspace(t, "--workspace", serveTree(t, tree))

	waiting := sendWant(t, c, textIn("context-a", "writeask hello.txt"), a2a.TaskStateInputRequired)
	if _, err := tree.Write(context.Background(), "hello.txt", strings.NewReader("the host's\n"), 0o644, ""); err != nil {
		t.Fatal(err)
	}
	failed := sendWant(t, c, allow(t, waiting), a2a.TaskStateFailed)

	if msg := failed.Status.Message; msg == nil || !cmp.Equal([]any{"hello.txt"}, msg.Metadata[a2aserve.ConflictsKey]) {
		t.Fatalf("failed task status message = %+v, want %s with hello.txt", msg, a2aserve.ConflictsKey)
	}
	if got, err := readTree(t, tree, "hello.txt"); err != nil || got != "the host's\n" {
		t.Fatalf("reading hello.txt of the host's tree after the conflict = %q, %v, want the host's file", got, err)
	}
}

func TestURLWorkspaceThatStopsAnsweringFailsTheTurnWithoutItsPath(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.StripPrefix("/secret-value", fsys.Handler(fsys.Memory())))
	t.Cleanup(srv.Close)
	_, c := startWorkspace(t, "--workspace", srv.URL+"/secret-value/")

	waiting := sendWant(t, c, textIn("context-a", "writeask hello.txt"), a2a.TaskStateInputRequired)
	srv.Close()
	failed := sendWant(t, c, allow(t, waiting), a2a.TaskStateFailed)

	text := statusText(failed)
	if !strings.Contains(text, "The workspace at "+srv.URL+" didn't answer") || strings.Contains(text, "secret-value") {
		t.Fatalf("status of a turn whose workspace stopped answering = %q, want it to name %s without the path", text, srv.URL)
	}
}

func TestTurnSyncConflictFailsTheTask(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	_, c := startWorkspace(t, "--workspace", "dir:"+dir)

	waiting := sendWant(t, c, textIn("context-a", "writeask hello.txt"), a2a.TaskStateInputRequired)
	if err := os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("the host's\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	failed := sendWant(t, c, allow(t, waiting), a2a.TaskStateFailed)

	msg := failed.Status.Message
	if msg == nil {
		t.Fatalf("failed task status message = nil, want the conflicts")
	}
	if diff := cmp.Diff([]any{"hello.txt"}, msg.Metadata[a2aserve.ConflictsKey]); diff != "" {
		t.Fatalf("status metadata %s wrong result (-want +got) diff = %s", a2aserve.ConflictsKey, diff)
	}
	if text := msg.Parts[0].Text(); !strings.Contains(text, "kept its own version and dropped the agent's: hello.txt") {
		t.Fatalf("status message = %q, want it to name the conflict", text)
	}
	got, err := os.ReadFile(filepath.Join(dir, "hello.txt"))
	if err != nil || string(got) != "the host's\n" {
		t.Fatalf("os.ReadFile(hello.txt) after the conflict = %q, %v, want the host's file", got, err)
	}
}

func TestCanceledTaskGetsTheChangesOfItsTurn(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	_, c := startWorkspace(t, "--workspace", "memory")
	waiting := sendWant(t, c, textIn("context-a", "writeask hello.txt"), a2a.TaskStateInputRequired)

	if _, err := c.CancelTask(ctx, &a2a.CancelTaskRequest{ID: waiting.ID}); err != nil {
		t.Fatalf("Client.CancelTask() error = %v", err)
	}

	want := &changeset{Files: map[string]string{"hello.txt": "written by fakeacp\n"}}
	deadline := time.Now().Add(10 * time.Second)
	for {
		task, err := c.GetTask(ctx, &a2a.GetTaskRequest{ID: waiting.ID})
		if err != nil {
			t.Fatalf("Client.GetTask() error = %v", err)
		}
		if task.Status.State != a2a.TaskStateCanceled {
			t.Fatalf("Client.GetTask() state = %v, want %v", task.Status.State, a2a.TaskStateCanceled)
		}
		got := changesOf(t, task)
		if got != nil {
			if diff := cmp.Diff(want, got); diff != "" {
				t.Fatalf("changes of the canceled task wrong result (-want +got) diff = %s", diff)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("canceled task %s has no %s artifact after 10s", task.ID, serve.ChangesArtifact)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// statusText is the text of the status message of task.
func statusText(task *a2a.Task) string {
	if task.Status.Message == nil {
		return ""
	}
	var out []string
	for _, p := range task.Status.Message.Parts {
		out = append(out, p.Text())
	}
	return strings.Join(out, "")
}
