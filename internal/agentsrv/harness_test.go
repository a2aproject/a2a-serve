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
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/a2aproject/a2a-go/v2/a2aclient/agentcard"

	"github.com/a2aproject/a2a-serve/internal/a2atext"
	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/agentsrv/agentproxy"
	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/contacts"
	"github.com/a2aproject/a2a-serve/internal/controlapi"
	"github.com/a2aproject/a2a-serve/internal/credstore"
	"github.com/a2aproject/a2a-serve/internal/googleauth"
	"github.com/a2aproject/a2a-serve/internal/harness"
	"github.com/a2aproject/a2a-serve/internal/jsonhttp"
	"github.com/a2aproject/a2a-serve/internal/messenger"
	"github.com/a2aproject/a2a-serve/internal/messenger/messengertest"
	"github.com/a2aproject/a2a-serve/internal/network"
	"github.com/a2aproject/a2a-serve/internal/pairapi"
	"github.com/a2aproject/a2a-serve/internal/store"
)

// testHTTP has a transport of its own: httptest.Server.Close closes the idle
// connections of http.DefaultTransport, which breaks requests that parallel
// tests send through it at the same time.
var testHTTP = &http.Client{Transport: http.DefaultTransport.(*http.Transport).Clone()}

// postHeaders sends the headers of a POST to rawURL with a body of size
// bytes, without the body, and returns the response. It is for bodies that
// the server refuses by their length: a client that writes the body can
// find the connection closed under the write instead of the response.
func postHeaders(t *testing.T, rawURL string, header http.Header, size int) *http.Response {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("tcp", u.Host)
	if err != nil {
		t.Fatalf("dialing %s: %v", u.Host, err)
	}
	t.Cleanup(func() {
		if err := conn.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			t.Errorf("closing the connection: %v", err)
		}
	})
	if err := conn.SetDeadline(time.Now().Add(waitTimeout)); err != nil {
		t.Fatal(err)
	}
	req := &http.Request{Method: http.MethodPost, URL: u, Header: header.Clone()}
	var head strings.Builder
	fmt.Fprintf(&head, "POST %s HTTP/1.1\r\nHost: %s\r\nContent-Length: %d\r\n", u.RequestURI(), u.Host, size)
	if err := header.Write(&head); err != nil {
		t.Fatal(err)
	}
	head.WriteString("\r\n")
	if _, err := io.WriteString(conn, head.String()); err != nil {
		t.Fatalf("writing the request headers: %v", err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), req)
	if err != nil {
		t.Fatalf("reading the response: %v", err)
	}
	return resp
}

var fakeACP string

// migratedDB is a server.db with every migration applied. Migrations take
// most of the CPU time of a fresh store, so tests copy it instead.
var migratedDB []byte

var testTimings = Timings{
	WatchInterval:            50 * time.Millisecond,
	WatchMaxInterval:         200 * time.Millisecond,
	EditInterval:             50 * time.Millisecond,
	ScheduleMinWait:          10 * time.Millisecond,
	WebhookRetries:           []time.Duration{0, 100 * time.Millisecond},
	MessengerRetryMin:        10 * time.Millisecond,
	FirstBackupDelay:         50 * time.Millisecond,
	AnonymousQuestionTimeout: 5 * time.Second,
	SendRetry:                time.Millisecond,
	StateCheckInterval:       50 * time.Millisecond,
}

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "fakeacp")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fakeACP = filepath.Join(dir, "fakeacp")
	build := exec.Command("go", "build", "-o", fakeACP, "./testdata/fakeacp")
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "building fakeacp:", err)
		os.Exit(1)
	}
	if migratedDB, err = migrateDB(filepath.Join(dir, "server.db")); err != nil {
		fmt.Fprintln(os.Stderr, "migrating the template database:", err)
		os.Exit(1)
	}
	code := m.Run()
	if err := os.RemoveAll(dir); err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
	os.Exit(code)
}

func migrateDB(path string) ([]byte, error) {
	st, err := store.Open(path)
	if err != nil {
		return nil, err
	}
	if err := st.Close(); err != nil {
		return nil, err
	}
	return os.ReadFile(path)
}

// seedDB writes migratedDB to path unless a database is already there.
func seedDB(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(migratedDB); err != nil {
		t.Fatal(errors.Join(err, f.Close()))
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// testServer is a server running fakeacp in a temporary directory.
type testServer struct {
	t    *testing.T
	dir  agentdir.Dir
	srv  *Server
	ch   *messengertest.Messenger
	done chan error
}

type serverOpts struct {
	name        string
	dir         string
	messenger   *messengertest.Messenger
	credentials *credstore.Store
	telegramAPI string
	secrets     agentdir.Secrets
	// configure changes the config before the server starts.
	configure func(*agentdir.Config)
	// timings changes the test timings before the server starts.
	timings func(*Timings)
	google  *googleauth.Endpoints
	// sharedClientID is the client ID of the shared Google app.
	sharedClientID string
	private        network.Provider
	public         network.Provider
	newRelay       func(network.RelayConfig) (network.Provider, error)
	now            func() time.Time
	out            io.Writer
	setupProblems  func(context.Context) []string
}

// startServer starts a server and stops it when the test ends.
func startServer(t *testing.T, o serverOpts) *testServer {
	t.Helper()
	if o.name == "" {
		o.name = "coder"
	}
	if o.dir == "" {
		o.dir = shortTempDir(t)
	}
	dir, err := agentdir.Open(o.dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &agentdir.Config{Name: o.name, Harness: "fake", Command: []string{fakeACP}, Sandbox: harness.SandboxLocal, Port: freePort(t)}
	fresh := !dir.Exists()
	if !fresh {
		if cfg, err = dir.LoadConfig(); err != nil {
			t.Fatal(err)
		}
	}
	if o.configure != nil {
		o.configure(cfg)
	}
	if o.secrets == nil {
		o.secrets = agentdir.Secrets{}
	}
	if err := dir.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	seedDB(t, dir.DBPath())
	timings := testTimings
	if o.timings != nil {
		o.timings(&timings)
	}
	// The agent processes' stderr explains why one exited.
	agentStderr := &lockedBuffer{}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("stderr of the agent processes:\n%s", agentStderr)
		}
	})
	ts := &testServer{t: t, dir: dir, ch: o.messenger, done: make(chan error, 1)}
	ready := make(chan *Server, 1)
	opts := Options{Private: o.private, Public: o.public, NewRelay: o.newRelay, GoogleEndpoints: o.google, SharedGoogleClientID: o.sharedClientID, Dir: dir, Config: cfg, Secrets: o.secrets, Credentials: o.credentials, TelegramServerURL: o.telegramAPI, Now: o.now, HarnessLog: agentStderr, Out: o.out, SetupProblems: o.setupProblems, Timings: timings, Ready: func(s *Server) { ready <- s }}
	if o.messenger != nil {
		opts.Messengers = []messenger.Messenger{o.messenger}
	}
	// freePort only knows the ports of this process, so another test
	// process can take the port of a fresh agent before it listens.
	for attempt := 1; ; attempt++ {
		go func() { ts.done <- Run(context.Background(), opts) }()
		select {
		case ts.srv = <-ready:
			t.Cleanup(ts.stop)
			return ts
		case err := <-ts.done:
			if !fresh || attempt == 3 || !errors.Is(err, syscall.EADDRINUSE) {
				t.Fatalf("Run() error = %v", err)
			}
			cfg.Port = freePort(t)
			if err := dir.SaveConfig(cfg); err != nil {
				t.Fatal(err)
			}
		case <-time.After(30 * time.Second):
			t.Fatal("server did not start")
		}
	}
}

// leftFiles lists the files under dir, which a writer that outlived the
// server left there.
// shortTempDir returns a temp dir for an agent, removed when the test ends.
// Unix socket paths are short, so the agent lives in a short temp dir.
func shortTempDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("", "a2a")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(d); err != nil {
			t.Errorf("removing %s: %v; left: %v", d, err, leftFiles(d))
		}
	})
	return d
}

// copyAgent copies the agent directory src to dst with the modes of its
// files, as a restore or a move does.
func copyAgent(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		target := filepath.Join(dst, strings.TrimPrefix(path, src))
		if d.IsDir() {
			if err := os.MkdirAll(target, 0o700); err != nil {
				return err
			}
			return os.Chmod(target, info.Mode().Perm())
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, info.Mode().Perm())
	})
	if err != nil {
		t.Fatalf("copying %s to %s: %v", src, dst, err)
	}
}

func leftFiles(dir string) []string {
	var left []string
	if err := filepath.WalkDir(dir, func(path string, _ fs.DirEntry, err error) error {
		left = append(left, path)
		return err
	}); err != nil {
		left = append(left, err.Error())
	}
	return left
}

// Agents keep their port across restarts. Ports are picked below the
// ephemeral range, so that a parallel test listening on port 0 can't take
// the port of a stopped agent before it restarts.
const (
	minTestPort = 20000
	maxTestPort = 32000
)

var nextTestPort = struct {
	sync.Mutex
	n int
}{n: rand.IntN(maxTestPort - minTestPort)}

// freePort returns a free port for a new agent, never the same one twice
// in this process.
func freePort(t *testing.T) int {
	t.Helper()
	nextTestPort.Lock()
	defer nextTestPort.Unlock()
	for range maxTestPort - minTestPort {
		port := minTestPort + nextTestPort.n
		nextTestPort.n = (nextTestPort.n + 1) % (maxTestPort - minTestPort)
		ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
		if err != nil {
			continue
		}
		if err := ln.Close(); err != nil {
			t.Fatalf("closing the listener on port %d: %v", port, err)
		}
		return port
	}
	t.Fatalf("no free port in [%d, %d)", minTestPort, maxTestPort)
	return 0
}

func (ts *testServer) stop() {
	if ts.srv == nil {
		return
	}
	ts.srv.Stop()
	select {
	case err := <-ts.done:
		if err != nil {
			ts.t.Errorf("Run() error = %v", err)
		}
	case <-time.After(2 * closeTimeout):
		ts.t.Errorf("server did not stop")
	}
	ts.srv = nil
}

func (ts *testServer) localURL() string {
	local, _, _ := ts.srv.URLs()
	return local
}

func (ts *testServer) publicURL() string {
	_, _, public := ts.srv.URLs()
	return public
}

// pair mints a code over the control socket and exchanges it for a token.
func (ts *testServer) pair(name, role string) string {
	ts.t.Helper()
	ctx := context.Background()
	code, err := controlapi.NewClient(ts.dir).Pair(ctx, controlapi.PairCodeRequest{Name: name, Owner: role == auth.RoleOwner})
	if err != nil {
		ts.t.Fatalf("Pair() error = %v", err)
	}
	resp, err := pairapi.Pair(ctx, testHTTP, ts.localURL(), code.Code, pairapi.PairingPerson)
	if err != nil {
		ts.t.Fatalf("pairapi.Pair() error = %v", err)
	}
	return resp.Token
}

// client returns an A2A client authenticated with token.
func (ts *testServer) client(token string) *a2aclient.Client {
	ts.t.Helper()
	return ts.clientOver(token)
}

// clientOver returns an A2A client authenticated with token that picks the
// first of transports the agent card offers, or the card's order when none is given.
func (ts *testServer) clientOver(token string, transports ...a2a.TransportProtocol) *a2aclient.Client {
	ts.t.Helper()
	hc := contacts.TokenClient(testHTTP, store.Contact{URL: ts.localURL(), Token: token})
	ctx := context.Background()
	card, err := agentcard.NewResolver(hc).Resolve(ctx, ts.localURL())
	if err != nil {
		ts.t.Fatalf("Resolve() error = %v", err)
	}
	c, err := a2aclient.NewFromCard(ctx, card, a2aclient.WithConfig(a2aclient.Config{PreferredTransports: transports}), a2aclient.WithJSONRPCTransport(hc), a2aclient.WithRESTTransport(hc))
	if err != nil {
		ts.t.Fatalf("NewFromCard() error = %v", err)
	}
	ts.t.Cleanup(func() {
		if err := c.Destroy(); err != nil {
			ts.t.Errorf("Destroy() error = %v", err)
		}
	})
	return c
}

func userText(text, taskID, contextID string) *a2a.Message {
	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(text))
	msg.TaskID, msg.ContextID = a2a.TaskID(taskID), contextID
	return msg
}

func send(t *testing.T, c *a2aclient.Client, msg *a2a.Message) *a2a.Task {
	t.Helper()
	res, err := c.SendMessage(context.Background(), &a2a.SendMessageRequest{Message: msg})
	if err != nil {
		t.Fatalf("SendMessage() error = %v", err)
	}
	task, ok := res.(*a2a.Task)
	if !ok {
		t.Fatalf("SendMessage() = %T, want *a2a.Task", res)
	}
	return task
}

func sendAsync(t *testing.T, c *a2aclient.Client, msg *a2a.Message) *a2a.Task {
	t.Helper()
	res, err := c.SendMessage(context.Background(), &a2a.SendMessageRequest{Message: msg, Config: &a2a.SendMessageConfig{ReturnImmediately: true}})
	if err != nil {
		t.Fatalf("SendMessage() error = %v", err)
	}
	return res.(*a2a.Task)
}

// waitState polls a task until it reaches state.
func waitState(t *testing.T, c *a2aclient.Client, id a2a.TaskID, state a2a.TaskState) *a2a.Task {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		task, err := c.GetTask(context.Background(), &a2a.GetTaskRequest{ID: id})
		if err != nil {
			t.Fatalf("GetTask() error = %v", err)
		}
		if task.Status.State == state {
			return task
		}
		if time.Now().After(deadline) {
			t.Fatalf("task %s state = %s, want %s", id, task.Status.State, state)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func wantResult(t *testing.T, task *a2a.Task, state a2a.TaskState, text string) {
	t.Helper()
	if task.Status.State != state {
		t.Fatalf("task state = %s (%s), want %s", task.Status.State, a2atext.Message(task.Status.Message), state)
	}
	if got := a2atext.Result(task); !strings.Contains(got, text) {
		t.Fatalf("task result = %q, want it to contain %q", got, text)
	}
}

// waitTimeout bounds how long tests wait for something to happen.
const waitTimeout = 10 * time.Second

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(waitTimeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// harnessKeyCaller takes requests with key, or every request for no key,
// as calls from the owner's agent process.
func harnessKeyCaller(key string) agentproxy.Caller {
	return func(r *http.Request) (string, bool) {
		if key == "" {
			return "", true
		}
		tok, ok := auth.BearerToken(r.Header.Get("Authorization"))
		return "", ok && tok == key
	}
}

type proxyResponse struct {
	Status int
	Error  string
	Body   []byte
}

func getURL(t *testing.T, u, authorization string) proxyResponse {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, u, nil)
	if err != nil {
		t.Fatalf("http.NewRequestWithContext() error = %v", err)
	}
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	resp, err := testHTTP.Do(req)
	if err != nil {
		t.Fatalf("GET %s error = %v", u, err)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading the response body error = %v", err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatalf("closing the response body error = %v", err)
	}
	out := proxyResponse{Status: resp.StatusCode, Body: body}
	var apiErr jsonhttp.APIError
	if json.Unmarshal(body, &apiErr) == nil {
		out.Error = apiErr.Error
	}
	return out
}
