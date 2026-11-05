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
	"fmt"
	"io"
	"io/fs"
	"maps"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/a2aproject/a2a-go/v2/a2aclient/agentcard"

	"github.com/a2aproject/a2a-serve/internal/a2atext"
	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/channel"
	"github.com/a2aproject/a2a-serve/internal/contacts"
	"github.com/a2aproject/a2a-serve/internal/controlapi"
	"github.com/a2aproject/a2a-serve/internal/credstore"
	"github.com/a2aproject/a2a-serve/internal/googleauth"
	"github.com/a2aproject/a2a-serve/internal/harness"
	"github.com/a2aproject/a2a-serve/internal/network"
	"github.com/a2aproject/a2a-serve/internal/pairapi"
	"github.com/a2aproject/a2a-serve/internal/store"
)

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
	ChannelRetryMin:          10 * time.Millisecond,
	FirstBackupDelay:         50 * time.Millisecond,
	AnonymousQuestionTimeout: 5 * time.Second,
	SendRetry:                time.Millisecond,
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
	ch   *fakeChannel
	done chan error
}

type serverOpts struct {
	name        string
	dir         string
	channel     *fakeChannel
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
		// Unix socket paths are short, so the agent lives in a short temp dir.
		d, err := os.MkdirTemp("", "a2a")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := os.RemoveAll(d); err != nil {
				t.Errorf("removing %s: %v", d, err)
			}
		})
		o.dir = d
	}
	dir, err := agentdir.Open(o.dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &agentdir.Config{Name: o.name, Harness: "fake", Command: []string{fakeACP}, Sandbox: harness.SandboxLocal, Port: freePort(t)}
	if dir.Exists() {
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
	ts := &testServer{t: t, dir: dir, ch: o.channel, done: make(chan error, 1)}
	ready := make(chan *Server, 1)
	opts := Options{Private: o.private, Public: o.public, NewRelay: o.newRelay, GoogleEndpoints: o.google, SharedGoogleClientID: o.sharedClientID, Dir: dir, Config: cfg, Secrets: o.secrets, Credentials: o.credentials, TelegramServerURL: o.telegramAPI, Now: o.now, Out: o.out, SetupProblems: o.setupProblems, Timings: timings, Ready: func(s *Server) { ready <- s }}
	if o.channel != nil {
		opts.Channels = []channel.Channel{o.channel}
	}
	go func() { ts.done <- Run(context.Background(), opts) }()
	select {
	case ts.srv = <-ready:
	case err := <-ts.done:
		t.Fatalf("Run() error = %v", err)
	case <-time.After(30 * time.Second):
		t.Fatal("server did not start")
	}
	t.Cleanup(ts.stop)
	return ts
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
	case <-time.After(30 * time.Second):
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
	resp, err := pairapi.Pair(ctx, http.DefaultClient, ts.localURL(), code.Code, pairapi.PairingPerson)
	if err != nil {
		ts.t.Fatalf("pairapi.Pair() error = %v", err)
	}
	return resp.Token
}

// client returns an A2A client authenticated with token.
func (ts *testServer) client(token string) *a2aclient.Client {
	ts.t.Helper()
	hc := contacts.TokenClient(http.DefaultClient, store.Contact{URL: ts.localURL(), Token: token})
	ctx := context.Background()
	card, err := agentcard.NewResolver(hc).Resolve(ctx, ts.localURL())
	if err != nil {
		ts.t.Fatalf("Resolve() error = %v", err)
	}
	c, err := a2aclient.NewFromCard(ctx, card, a2aclient.WithJSONRPCTransport(hc), a2aclient.WithRESTTransport(hc))
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
	if got := resultText(task); !strings.Contains(got, text) {
		t.Fatalf("task result = %q, want it to contain %q", got, text)
	}
}

// fakeChannel records what the router sends and lets tests send messages.
type fakeChannel struct {
	name    string
	buttons bool
	maxText int
	// failures is how many times Run fails before it connects.
	failures int
	// sendDelay is how long Send returns after the message is visible.
	sendDelay time.Duration

	mu    sync.Mutex
	in    chan<- channel.Inbound
	msgs  map[string]channel.Outbound
	order []string
	ready chan struct{}
}

func newFakeChannel(name string) *fakeChannel {
	return &fakeChannel{name: name, buttons: true, msgs: map[string]channel.Outbound{}, ready: make(chan struct{})}
}

func (f *fakeChannel) Name() string { return f.name }

func (f *fakeChannel) Caps() channel.Caps {
	maxText := f.maxText
	if maxText == 0 {
		maxText = 4000
	}
	return channel.Caps{Edit: true, Buttons: f.buttons, MaxText: maxText, Stream: true}
}

func (f *fakeChannel) Describe() string { return "@fake_bot" }

func (f *fakeChannel) LinkHint(code string) string { return "/start " + code }

func (f *fakeChannel) Run(ctx context.Context, in chan<- channel.Inbound, _ func(error)) error {
	f.mu.Lock()
	if f.failures > 0 {
		f.failures--
		f.mu.Unlock()
		return errors.New("messenger unreachable")
	}
	f.in = in
	f.mu.Unlock()
	close(f.ready)
	<-ctx.Done()
	return nil
}

func (f *fakeChannel) Send(_ context.Context, m channel.Outbound) (channel.Ref, error) {
	f.mu.Lock()
	msgSeq.Lock()
	msgSeq.n++
	id := strconv.Itoa(msgSeq.n)
	msgSeq.Unlock()
	f.msgs[id] = m
	f.order = append(f.order, id)
	f.mu.Unlock()
	time.Sleep(f.sendDelay)
	return channel.Ref{Conv: m.Conv, ID: id}, nil
}

func (f *fakeChannel) Edit(_ context.Context, ref channel.Ref, m channel.Outbound) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.msgs[ref.ID] = m
	return nil
}

var msgSeq struct {
	sync.Mutex
	n int
}

// write delivers a message from identity in conversation conv.
func (f *fakeChannel) write(identity, conv, text string) {
	f.deliver(f.message(identity, conv, text))
}

func (f *fakeChannel) message(identity, conv, text string) channel.Inbound {
	msgSeq.Lock()
	msgSeq.n++
	id := strconv.Itoa(msgSeq.n)
	msgSeq.Unlock()
	return channel.Inbound{Channel: f.name, MessageID: "m" + id, Identity: identity, Name: identity, Conv: channel.ConvKey(conv), Direct: true, Text: text, Authentic: true}
}

func (f *fakeChannel) deliver(in channel.Inbound) {
	<-f.ready
	f.in <- in
}

// waitReady waits until the router runs the channel.
func (f *fakeChannel) waitReady(t *testing.T) {
	t.Helper()
	select {
	case <-f.ready:
	case <-time.After(10 * time.Second):
		t.Fatal("channel did not start")
	}
}

// click presses option of a question message.
func (f *fakeChannel) click(identity, conv string, ref string, q *channel.Question, option int) {
	<-f.ready
	msgSeq.Lock()
	msgSeq.n++
	id := strconv.Itoa(msgSeq.n)
	msgSeq.Unlock()
	f.in <- channel.Inbound{Channel: f.name, MessageID: "cb" + id, Identity: identity, Conv: channel.ConvKey(conv), Direct: true,
		Action: &channel.Action{QuestionID: q.ID, Option: option}, Authentic: true, ReplyTo: channel.Ref{Conv: channel.ConvKey(conv), ID: ref}}
}

// waitFor waits until a message in conv satisfies pred and returns its ID.
func (f *fakeChannel) waitFor(t *testing.T, conv string, pred func(channel.Outbound) bool) (string, channel.Outbound) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		for _, id := range f.order {
			m := f.msgs[id]
			if string(m.Conv) == conv && pred(m) {
				f.mu.Unlock()
				return id, m
			}
		}
		f.mu.Unlock()
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no matching message in %s; messages:\n%s", conv, f.dump())
	return "", channel.Outbound{}
}

func (f *fakeChannel) dump() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var all []string
	for _, id := range slices.Sorted(maps.Keys(f.msgs)) {
		all = append(all, fmt.Sprintf("%s[%s]: %q q=%v", id, f.msgs[id].Conv, f.msgs[id].Text, f.msgs[id].Question))
	}
	return strings.Join(all, "\n")
}

// waitEdited waits until the message with ID id contains text. The message
// may have been sent by an earlier channel of the same agent.
func (f *fakeChannel) waitEdited(t *testing.T, id, text string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		f.mu.Lock()
		got := f.msgs[id].Text
		f.mu.Unlock()
		if strings.Contains(got, text) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("message %s = %q, want it to contain %q; messages:\n%s", id, got, text, f.dump())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// questions counts the messages with buttons.
func (f *fakeChannel) questions() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, id := range f.order {
		if f.msgs[id].Question != nil {
			n++
		}
	}
	return n
}

func (f *fakeChannel) transcript() []channel.Outbound {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]channel.Outbound, len(f.order))
	for i, id := range f.order {
		out[i] = f.msgs[id]
	}
	return out
}

func hasText(s string) func(channel.Outbound) bool {
	return func(m channel.Outbound) bool { return strings.Contains(m.Text, s) }
}

func hasQuestion(m channel.Outbound) bool { return m.Question != nil }
