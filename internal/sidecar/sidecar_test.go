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

package sidecar_test

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

	"github.com/a2aproject/a2a-serve/internal/a2atext"
	"github.com/a2aproject/a2a-serve/internal/agentrun"
	"github.com/a2aproject/a2a-serve/internal/sandboxrt"
	"github.com/a2aproject/a2a-serve/internal/sidecar"
	"github.com/a2aproject/a2a-serve/internal/sidecar/serve"
	"github.com/a2aproject/a2a-serve/internal/testutil"
)

var (
	// fakeACP is the fake ACP agent of the server's tests.
	fakeACP        string
	sidecarTestDir string
)

func TestMain(m *testing.M) {
	dir, err := testutil.MkdirShortTemp()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	sidecarTestDir = dir
	fakeACP = filepath.Join(dir, "fakeacp")
	build := exec.Command("go", "build", "-o", fakeACP, "../agentsrv/testdata/fakeacp")
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "building fakeacp:", err)
		if rerr := os.RemoveAll(dir); rerr != nil {
			fmt.Fprintln(os.Stderr, rerr)
		}
		os.Exit(1)
	}
	code := m.Run()
	if fakeacpImage != "" {
		if out, err := exec.Command("docker", "rmi", "-f", fakeacpImage).CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "docker rmi %s error = %v: %s\n", fakeacpImage, err, out)
		}
	}
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

// running is a2a-serve-sidecar running in the test.
type running struct {
	ready  serve.Ready
	stdout *bufio.Reader
	stderr *syncBuffer
	cancel context.CancelFunc
	// code is the exit code, set before done closes.
	code int
	done chan struct{}
}

// start runs a2a-serve-sidecar with args and reads its ready line.
func start(t *testing.T, args ...string) *running {
	t.Helper()
	return startWithStdin(t, "", args...)
}

// startWithStdin runs a2a-serve-sidecar with stdin and args and reads its ready
// line.
func startWithStdin(t *testing.T, stdin string, args ...string) *running {
	t.Helper()
	pr, pw := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	r := &running{stdout: bufio.NewReader(pr), stderr: &syncBuffer{}, cancel: cancel, done: make(chan struct{})}
	var in io.Reader
	if stdin != "" {
		in = strings.NewReader(stdin)
	}
	go func() {
		defer close(r.done)
		r.code = sidecar.Main(ctx, args, sidecar.IO{Stdin: in, Stdout: pw, Stderr: r.stderr, StdoutIsTerminal: true, Getenv: func(string) string { return "" }, Version: "test"})
		if err := pw.Close(); err != nil {
			t.Errorf("PipeWriter.Close() error = %v", err)
		}
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-r.done:
		case <-time.After(time.Minute):
			t.Errorf("sidecar.Main() didn't return after its context ended")
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

// stop sends a2a-serve-sidecar the signal to stop and returns its exit code and what
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
		t.Fatalf("sidecar.Main() = %d, then stdout %q, want 0 and nothing after the ready line; stderr:\n%s", code, rest, r.stderr)
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
	if r.ready.Token == "" {
		t.Fatalf("ready line token = %q, want a run-scoped bearer token for TCP", r.ready.Token)
	}

	hc := &http.Client{Transport: &http.Transport{}}
	t.Cleanup(hc.CloseIdleConnections)

	for _, tc := range []struct {
		name string
		auth string
	}{
		{name: "missing auth", auth: ""},
		{name: "wrong token", auth: "Bearer wrong-token"},
		{name: "wrong scheme", auth: "Basic " + r.ready.Token},
	} {
		req, err := http.NewRequest(http.MethodGet, r.ready.A2A+"/.well-known/agent-card.json", nil)
		if err != nil {
			t.Fatal(err)
		}
		if tc.auth != "" {
			req.Header.Set("Authorization", tc.auth)
		}
		resp, err := hc.Do(req)
		if err != nil {
			t.Fatalf("GET agent card (%s) error = %v", tc.name, err)
		}
		if err := resp.Body.Close(); err != nil {
			t.Error(err)
		}
		if resp.StatusCode != http.StatusUnauthorized || resp.Header.Get("WWW-Authenticate") != "Bearer" {
			t.Fatalf("GET agent card (%s) = %d with WWW-Authenticate %q, want 401 Bearer", tc.name, resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
		}
	}

	req, err := http.NewRequest(http.MethodGet, r.ready.A2A+"/.well-known/agent-card.json", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+r.ready.Token)
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatalf("http.Get(agent card) error = %v", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Error(err)
		}
	}()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET agent card with token status = %d, want 200", resp.StatusCode)
	}
	var card a2a.AgentCard
	if err := json.NewDecoder(resp.Body).Decode(&card); err != nil {
		t.Fatalf("decoding the agent card error = %v", err)
	}

	want := []*a2a.AgentInterface{a2a.NewAgentInterface(r.ready.A2A, a2a.TransportProtocolJSONRPC), a2a.NewAgentInterface(r.ready.A2A, a2a.TransportProtocolHTTPJSON)}
	if diff := cmp.Diff(want, card.SupportedInterfaces); diff != "" {
		t.Fatalf("agent card interfaces wrong result (-want +got) diff = %s", diff)
	}

	unauthPost, err := hc.Post(r.ready.A2A+"/", "application/json", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"message/send"}`))
	if err != nil {
		t.Fatalf("POST / without token error = %v", err)
	}
	if err := unauthPost.Body.Close(); err != nil {
		t.Error(err)
	}
	if unauthPost.StatusCode != http.StatusUnauthorized {
		t.Fatalf("POST / without token status = %d, want 401", unauthPost.StatusCode)
	}

	authClient := &http.Client{Transport: bearerRoundTripper{base: &http.Transport{}, token: r.ready.Token}}
	t.Cleanup(authClient.CloseIdleConnections)
	c, err := a2aclient.NewFromEndpoints(context.Background(), []*a2a.AgentInterface{a2a.NewAgentInterface(r.ready.A2A, a2a.TransportProtocolJSONRPC)}, a2aclient.WithJSONRPCTransport(authClient))
	if err != nil {
		t.Fatalf("a2aclient.NewFromEndpoints() error = %v", err)
	}
	if got := send(t, c, "tcp-ctx", "pid"); !strings.HasPrefix(got, "pid ") {
		t.Fatalf("Client.SendMessage(pid) over authenticated TCP = %q, want a pid", got)
	}
}

type bearerRoundTripper struct {
	base  http.RoundTripper
	token string
}

func (b bearerRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return b.base.RoundTrip(r)
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
		{name: "argument", args: []string{"opencode"}, want: `a2a-serve-sidecar takes no arguments, got "opencode"`},
		{name: "info on a terminal", args: []string{"info"}, want: `a2a-serve-sidecar takes no arguments, got "info"`},
		{name: "egress with local", args: []string{"--agent", "opencode", "--workspace", workspace, "--sandbox", "local", "--egress", "allowlist"}, want: "--egress needs --sandbox docker"},
		{name: "invalid max processes", args: []string{"--agent", "opencode", "--workspace", workspace, "--max-processes", "x"}, want: `invalid argument "x" for "--max-processes"`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var stdout, stderr bytes.Buffer

			code := sidecar.Main(context.Background(), tc.args, sidecar.IO{Stdout: &stdout, Stderr: &stderr, StdoutIsTerminal: true})

			if code != 2 {
				t.Fatalf("sidecar.Main(%q) = %d, want 2", tc.args, code)
			}
			var ready serve.Ready
			if err := json.Unmarshal(stdout.Bytes(), &ready); err != nil || ready.Ready || !strings.Contains(ready.Error, tc.want) {
				t.Fatalf("sidecar.Main(%q) stdout = %q, want a ready line with ready false and an error with %q", tc.args, stdout.String(), tc.want)
			}
			if !strings.Contains(stderr.String(), tc.want) {
				t.Fatalf("sidecar.Main(%q) stderr = %q, want %q", tc.args, stderr.String(), tc.want)
			}
		})
	}
}

func TestMainFailsWhenTheAgentDoesNotStart(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	args := []string{"--sandbox", "local", "--command", filepath.Join(t.TempDir(), "missing"), "--workspace", "dir:" + t.TempDir()}

	code := sidecar.Main(context.Background(), args, sidecar.IO{Stdout: &stdout, Stderr: &stderr})

	var ready serve.Ready
	if err := json.Unmarshal(stdout.Bytes(), &ready); err != nil || code != 1 || ready.Ready || ready.Error == "" {
		t.Fatalf("sidecar.Main() = %d with stdout %q, want 1 and a ready line with ready false and an error", code, stdout.String())
	}
}

func TestMainPrintsPluginInfo(t *testing.T) {
	t.Parallel()
	var stdout bytes.Buffer

	code := sidecar.Main(context.Background(), []string{"info"}, sidecar.IO{Stdout: &stdout, Stderr: io.Discard, Version: "1.2.3"})

	want := `{"name":"serve-sidecar","version":"1.2.3","description":"Serve a coding agent in a sandbox over A2A"}` + "\n"
	if code != 0 || stdout.String() != want {
		t.Fatalf("sidecar.Main(info) = %d with stdout %q, want 0 and %q", code, stdout.String(), want)
	}
}

func TestAppArmorProfileCommand(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		args       []string
		wantCode   int
		wantStdout string
	}{
		{name: "prints the profile", args: []string{"apparmor-profile"}, wantStdout: sandboxrt.LiveAppArmor()},
		{name: "refuses arguments", args: []string{"apparmor-profile", "extra"}, wantCode: 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var stdout, stderr bytes.Buffer

			code := sidecar.Main(context.Background(), tc.args, sidecar.IO{Stdout: &stdout, Stderr: &stderr, StdoutIsTerminal: true})

			if code != tc.wantCode || stdout.String() != tc.wantStdout {
				t.Fatalf("sidecar.Main(%q) = %d with stdout %q, want %d and %q; stderr: %s", tc.args, code, stdout.String(), tc.wantCode, tc.wantStdout, stderr.String())
			}
		})
	}
}

func TestStopFailsTheTasksOfWaitingCallers(t *testing.T) {
	t.Parallel()
	sock := filepath.Join(testutil.ShortTempDir(t), "a2a.sock")
	r := start(t, "--sandbox", "local", "--command", fakeACP, "--workspace", "dir:"+t.TempDir(), "--listen", "unix://"+sock)
	c, err := a2aclient.NewFromEndpoints(context.Background(), []*a2a.AgentInterface{a2a.NewAgentInterface("http://a2a/", a2a.TransportProtocolJSONRPC)}, a2aclient.WithJSONRPCTransport(unixClient(sock)))
	if err != nil {
		t.Fatalf("a2aclient.NewFromEndpoints() error = %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	stall := func(contextID string) *a2a.SendMessageRequest {
		msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("stall"))
		msg.ContextID = contextID
		return &a2a.SendMessageRequest{Message: msg}
	}
	type result struct {
		status *a2a.TaskStatus
		err    error
	}
	blocking := make(chan result, 1)
	go func() {
		res, err := c.SendMessage(ctx, stall("blocking"))
		if err != nil {
			blocking <- result{err: err}
			return
		}
		task, ok := res.(*a2a.Task)
		if !ok {
			blocking <- result{err: fmt.Errorf("got %T, want a task", res)}
			return
		}
		blocking <- result{status: &task.Status}
	}()
	working := make(chan struct{})
	streaming := make(chan result, 1)
	go func() {
		var last result
		var closeWorking sync.Once
		for ev, err := range c.SendStreamingMessage(ctx, stall("streaming")) {
			if err != nil {
				last = result{err: err}
				break
			}
			if s, ok := ev.(*a2a.TaskStatusUpdateEvent); ok {
				last = result{status: &s.Status}
				if s.Status.State == a2a.TaskStateWorking {
					closeWorking.Do(func() { close(working) })
				}
			}
		}
		streaming <- last
	}()
	select {
	case <-working:
	case <-ctx.Done():
		t.Fatal("Client.SendStreamingMessage(stall) sent no working status")
	}
	waitWorking(t, c, 2)

	if code, _ := r.stop(t); code != 0 {
		t.Fatalf("sidecar.Main() = %d, want 0; stderr:\n%s", code, r.stderr)
	}

	for name, ch := range map[string]chan result{"Client.SendMessage(stall)": blocking, "Client.SendStreamingMessage(stall)": streaming} {
		got := <-ch
		if got.err != nil {
			t.Fatalf("%s error = %v, want a failed task", name, got.err)
		}
		if got.status.State != a2a.TaskStateFailed || a2atext.Message(got.status.Message) != agentrun.StoppedMessage {
			t.Fatalf("%s last status = %v %q, want %v %q", name, got.status.State, a2atext.Message(got.status.Message), a2a.TaskStateFailed, agentrun.StoppedMessage)
		}
	}
}

// waitWorking waits until n tasks are working.
func waitWorking(t *testing.T, c *a2aclient.Client, n int) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		resp, err := c.ListTasks(context.Background(), &a2a.ListTasksRequest{Status: a2a.TaskStateWorking})
		if err != nil {
			t.Fatalf("Client.ListTasks() error = %v", err)
		}
		if len(resp.Tasks) >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("Client.ListTasks() = %d working tasks, want %d", len(resp.Tasks), n)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
