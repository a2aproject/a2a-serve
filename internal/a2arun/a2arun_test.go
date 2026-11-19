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
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/a2arun"
	"github.com/a2aproject/a2a-serve/internal/a2arun/serve"
	"github.com/a2aproject/a2a-serve/internal/testutil"
)

// fakeACP is the fake ACP agent of the server's tests.
var fakeACP string

func TestMain(m *testing.M) {
	dir, err := testutil.MkdirShortTemp()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fakeACP = filepath.Join(dir, "fakeacp")
	build := exec.Command("go", "build", "-o", fakeACP, "../agentsrv/testdata/fakeacp")
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "building fakeacp:", err)
		os.Exit(1)
	}
	code := m.Run()
	if err := os.RemoveAll(dir); err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
	os.Exit(code)
}

// syncBuffer is a buffer that the server and the test use at once.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// running is a2a-run running in the test.
type running struct {
	ready  serve.Ready
	stdout *bufio.Reader
	stderr *syncBuffer
	cancel context.CancelFunc
	// code is the exit code, set before done closes.
	code int
	done chan struct{}
}

// start runs a2a-run with args and reads its ready line.
func start(t *testing.T, args ...string) *running {
	t.Helper()
	pr, pw := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	r := &running{stdout: bufio.NewReader(pr), stderr: &syncBuffer{}, cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(r.done)
		r.code = a2arun.Main(ctx, args, a2arun.IO{Stdout: pw, Stderr: r.stderr, StdoutIsTerminal: true, Getenv: func(string) string { return "" }, Version: "test"})
		if err := pw.Close(); err != nil {
			t.Errorf("PipeWriter.Close() error = %v", err)
		}
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-r.done:
		case <-time.After(time.Minute):
			t.Errorf("a2arun.Main() didn't return after its context ended")
		}
	})
	line, err := r.stdout.ReadString('\n')
	if err != nil {
		t.Fatalf("reading the ready line error = %v; stderr:\n%s", err, r.stderr)
	}
	if err := json.Unmarshal([]byte(line), &r.ready); err != nil {
		t.Fatalf("json.Unmarshal(%q) error = %v", line, err)
	}
	return r
}

// stop sends a2a-run the signal to stop and returns its exit code and what
// it printed on stdout after the ready line.
func (r *running) stop(t *testing.T) (int, string) {
	t.Helper()
	r.cancel()
	rest, err := io.ReadAll(r.stdout)
	if err != nil {
		t.Fatalf("reading stdout error = %v", err)
	}
	<-r.done
	return r.code, string(rest)
}

// unixClient returns an HTTP client whose requests go to the socket sock.
func unixClient(sock string) *http.Client {
	return &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", sock)
	}}}
}

// send sends text in the context contextID and returns the text of the
// result.
func send(t *testing.T, c *a2aclient.Client, contextID, text string) string {
	t.Helper()
	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(text))
	msg.ContextID = contextID
	res, err := c.SendMessage(context.Background(), &a2a.SendMessageRequest{Message: msg})
	if err != nil {
		t.Fatalf("Client.SendMessage(%q) error = %v", text, err)
	}
	task, ok := res.(*a2a.Task)
	if !ok {
		t.Fatalf("Client.SendMessage(%q) = %T, want a task", text, res)
	}
	if task.Status.State != a2a.TaskStateCompleted {
		t.Fatalf("Client.SendMessage(%q) state = %v, want %v", text, task.Status.State, a2a.TaskStateCompleted)
	}
	return testutil.AllArtifactText(task)
}

func TestServesEachContextInAProcessOfItsOwn(t *testing.T) {
	t.Parallel()
	sock := filepath.Join(testutil.ShortTempDir(t), "a2a.sock")
	r := start(t, "--sandbox", "local", "--command", fakeACP, "--workspace", "dir:"+t.TempDir(), "--listen", "unix://"+sock)
	if diff := cmp.Diff(serve.Ready{Ready: true, A2A: "unix://" + sock, Sandbox: "local", Workspace: "dir", WorkspaceSync: "turn"}, r.ready); diff != "" {
		t.Fatalf("ready line wrong result (-want +got) diff = %s", diff)
	}
	hc := unixClient(sock)
	c, err := a2aclient.NewFromEndpoints(context.Background(), []*a2a.AgentInterface{a2a.NewAgentInterface("http://a2a/", a2a.TransportProtocolJSONRPC)}, a2aclient.WithJSONRPCTransport(hc))
	if err != nil {
		t.Fatalf("a2aclient.NewFromEndpoints() error = %v", err)
	}

	first := send(t, c, "context-a", "pid")
	again := send(t, c, "context-a", "pid")
	other := send(t, c, "context-b", "pid")

	if !strings.HasPrefix(first, "pid ") || first != again {
		t.Fatalf("Client.SendMessage(pid) in context-a = %q, then %q, want one process", first, again)
	}
	if other == first {
		t.Fatalf("Client.SendMessage(pid) in context-b = %q, want another process than context-a's", other)
	}
	if code, rest := r.stop(t); code != 0 || rest != "" {
		t.Fatalf("a2arun.Main() = %d, then stdout %q, want 0 and nothing after the ready line; stderr:\n%s", code, rest, r.stderr)
	}
	if _, err := os.Stat(sock); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("os.Stat(%s) after the stop error = %v, want %v", sock, err, fs.ErrNotExist)
	}
}

func TestServesTheAgentCardOverTCP(t *testing.T) {
	t.Parallel()
	r := start(t, "--sandbox", "local", "--command", fakeACP, "--workspace", "dir:"+t.TempDir())
	base, ok := strings.CutPrefix(r.ready.A2A, "http://127.0.0.1:")
	if !ok || base == "0" {
		t.Fatalf("ready line a2a = %q, want http://127.0.0.1 with the bound port", r.ready.A2A)
	}

	hc := &http.Client{Transport: &http.Transport{}}
	t.Cleanup(hc.CloseIdleConnections)

	resp, err := hc.Get(r.ready.A2A + "/.well-known/agent-card.json")
	if err != nil {
		t.Fatalf("http.Get(agent card) error = %v", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Error(err)
		}
	}()
	var card a2a.AgentCard
	if err := json.NewDecoder(resp.Body).Decode(&card); err != nil {
		t.Fatalf("decoding the agent card error = %v", err)
	}

	want := []*a2a.AgentInterface{a2a.NewAgentInterface(r.ready.A2A, a2a.TransportProtocolJSONRPC), a2a.NewAgentInterface(r.ready.A2A, a2a.TransportProtocolHTTPJSON)}
	if diff := cmp.Diff(want, card.SupportedInterfaces); diff != "" {
		t.Fatalf("agent card interfaces wrong result (-want +got) diff = %s", diff)
	}
}

func TestMainReportsInvalidFlags(t *testing.T) {
	t.Parallel()
	workspace := "dir:" + t.TempDir()
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "unknown flag", args: []string{"--agent", "opencode", "--port", "1"}, want: "unknown flag: --port"},
		{name: "argument", args: []string{"opencode"}, want: `a2a-run takes no arguments, got "opencode"`},
		{name: "info on a terminal", args: []string{"info"}, want: `a2a-run takes no arguments, got "info"`},
		{name: "egress with local", args: []string{"--agent", "opencode", "--workspace", workspace, "--sandbox", "local", "--egress", "allowlist"}, want: "--egress need --sandbox docker"},
		{name: "invalid max processes", args: []string{"--agent", "opencode", "--workspace", workspace, "--max-processes", "x"}, want: `invalid argument "x" for "--max-processes"`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var stdout, stderr bytes.Buffer

			code := a2arun.Main(context.Background(), tc.args, a2arun.IO{Stdout: &stdout, Stderr: &stderr, StdoutIsTerminal: true})

			if code != 2 {
				t.Fatalf("a2arun.Main(%q) = %d, want 2", tc.args, code)
			}
			var ready serve.Ready
			if err := json.Unmarshal(stdout.Bytes(), &ready); err != nil || ready.Ready || !strings.Contains(ready.Error, tc.want) {
				t.Fatalf("a2arun.Main(%q) stdout = %q, want a ready line with ready false and an error with %q", tc.args, stdout.String(), tc.want)
			}
			if !strings.Contains(stderr.String(), tc.want) {
				t.Fatalf("a2arun.Main(%q) stderr = %q, want %q", tc.args, stderr.String(), tc.want)
			}
		})
	}
}

func TestMainFailsWhenTheAgentDoesNotStart(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	args := []string{"--sandbox", "local", "--command", filepath.Join(t.TempDir(), "missing"), "--workspace", "dir:" + t.TempDir()}

	code := a2arun.Main(context.Background(), args, a2arun.IO{Stdout: &stdout, Stderr: &stderr})

	var ready serve.Ready
	if err := json.Unmarshal(stdout.Bytes(), &ready); err != nil || code != 1 || ready.Ready || ready.Error == "" {
		t.Fatalf("a2arun.Main() = %d with stdout %q, want 1 and a ready line with ready false and an error", code, stdout.String())
	}
}

func TestMainPrintsPluginInfo(t *testing.T) {
	t.Parallel()
	var stdout bytes.Buffer

	code := a2arun.Main(context.Background(), []string{"info"}, a2arun.IO{Stdout: &stdout, Stderr: io.Discard, Version: "1.2.3"})

	want := `{"name":"run","version":"1.2.3","description":"Serve a coding agent in a sandbox over A2A"}` + "\n"
	if code != 0 || stdout.String() != want {
		t.Fatalf("a2arun.Main(info) = %d with stdout %q, want 0 and %q", code, stdout.String(), want)
	}
}
