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

package relay

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-serve/internal/relay/control"
	"github.com/a2aproject/a2a-serve/internal/relay/front"
	"github.com/a2aproject/a2a-serve/internal/relaydir"
	"github.com/a2aproject/a2a-serve/internal/testutil"
)

// fakeBackend starts once release is closed, and serves "backend" at its
// upstream.
type fakeBackend struct {
	release  chan struct{}
	upstream *httptest.Server
	running  *fakeRunning
}

func newFakeBackend(t *testing.T) *fakeBackend {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.WriteString(w, "backend"); err != nil {
			t.Errorf("writing the upstream's answer: %v", err)
		}
	}))
	t.Cleanup(srv.Close)
	return &fakeBackend{release: make(chan struct{}), upstream: srv, running: &fakeRunning{exited: make(chan struct{})}}
}

func (*fakeBackend) Name() string    { return "Fake" }
func (*fakeBackend) Version() string { return "v1" }
func (*fakeBackend) Prepare(context.Context, relaydir.Dir, *relaydir.Resolved, time.Time) (Prepared, error) {
	return Prepared{}, nil
}
func (*fakeBackend) Info(a2aServe string, signIn bool) any {
	return map[string]string{"a2a_serve": a2aServe}
}
func (*fakeBackend) Routes() front.Routes { return noRoutes{} }

func (b *fakeBackend) Start(ctx context.Context, s Start) (Running, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-b.release:
	}
	u, err := url.Parse(b.upstream.URL)
	if err != nil {
		return nil, err
	}
	b.running.upstream, b.running.status = u, s.Status
	return b.running, nil
}

type fakeRunning struct {
	upstream *url.URL
	status   func() control.Status
	exited   chan struct{}
	exitOnce sync.Once
	stopped  bool
}

func (r *fakeRunning) Upstream() *url.URL       { return r.upstream }
func (r *fakeRunning) Service() control.Service { return fakeService{r.status} }
func (r *fakeRunning) Reconcile(context.Context) ([]control.Member, []control.Member, error) {
	return nil, nil, nil
}
func (r *fakeRunning) Exited() <-chan struct{} { return r.exited }
func (r *fakeRunning) Err() error              { return errors.New("the fake backend exited") }
func (r *fakeRunning) Stop() error {
	r.stopped = true
	r.exit()
	return nil
}
func (r *fakeRunning) exit() { r.exitOnce.Do(func() { close(r.exited) }) }

type fakeService struct{ status func() control.Status }

func (s fakeService) Status(context.Context) (*control.Status, error) {
	st := s.status()
	return &st, nil
}
func (fakeService) Invite(context.Context, control.InviteRequest) (*control.Invite, error) {
	return nil, errors.New("not implemented")
}
func (fakeService) Members(context.Context, bool) ([]control.Member, error) { return nil, nil }
func (fakeService) Remove(context.Context, control.RemoveRequest) (*control.RemoveResponse, error) {
	return nil, errors.New("not implemented")
}

type noRoutes struct{}

func (noRoutes) Blocked(string, string) bool     { return false }
func (noRoutes) Enrollment(string) bool          { return false }
func (noRoutes) Ingress(string) (string, string) { return "", "" }

// runFake runs a relay over b and returns its directory, its URL, its
// control client, and the result of Run.
func runFake(t *testing.T, b Backend) (relaydir.Dir, string, *control.Client, context.CancelFunc, <-chan error) {
	t.Helper()
	d := relaydir.Dir{Root: testutil.ShortTempDir(t)}
	port := freeLoopbackPort(t)
	relayURL := fmt.Sprintf("http://127.0.0.1:%d", port)
	if _, err := d.SaveSettings(relaydir.Settings{URL: relayURL, Listen: fmt.Sprintf("127.0.0.1:%d", port)}); err != nil {
		t.Fatalf("SaveSettings() error = %v", err)
	}
	release, err := d.Lock()
	if err != nil {
		t.Fatalf("Lock() error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		err := New(Options{Dir: d, Backend: b, A2AServe: "test", Out: io.Discard, TunnelLog: io.Discard, Now: time.Now}).Run(ctx)
		done <- errors.Join(err, release())
	}()
	t.Cleanup(cancel)
	return d, relayURL, control.NewSocketClient(d.SocketPath(), 5*time.Second), cancel, done
}

func freeLoopbackPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	return port
}

// waitStatus waits until the relay's control API answers Status, and
// returns its error.
func waitStatus(t *testing.T, c *control.Client, done <-chan error) error {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		select {
		case err := <-done:
			t.Fatalf("Relay.Run() error = %v before the relay ran", err)
		default:
		}
		_, err := c.Status(context.Background())
		if err == nil || !errors.Is(err, control.ErrNotRunning) {
			return err
		}
		if time.Now().After(deadline) {
			t.Fatal("the relay didn't answer on its control socket")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func waitStarted(t *testing.T, c *control.Client, done <-chan error) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for waitStatus(t, c, done) != nil {
		if time.Now().After(deadline) {
			t.Fatal("the relay's backend didn't start")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func wait(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(30 * time.Second):
		t.Fatal("Relay.Run() didn't return")
		return nil
	}
}

func TestRunRefusesTheControlAPIUntilTheBackendStarts(t *testing.T) {
	b := newFakeBackend(t)
	_, _, c, _, done := runFake(t, b)

	err := waitStatus(t, c, done)

	if err == nil || !strings.Contains(err.Error(), "the relay is starting") {
		t.Fatalf("Client.Status() before the backend started error = %v, want the relay is starting", err)
	}
	close(b.release)
	waitStarted(t, c, done)
}

func TestRunProxiesToTheBackend(t *testing.T) {
	b := newFakeBackend(t)
	close(b.release)
	_, relayURL, c, _, done := runFake(t, b)
	waitStarted(t, c, done)

	resp, err := http.Get(relayURL + "/anything")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	if err := errors.Join(err, resp.Body.Close()); err != nil {
		t.Fatal(err)
	}

	if string(body) != "backend" {
		t.Fatalf("Get() of a backend route = %q, want %q", body, "backend")
	}
}

func TestRunReturnsWhenTheBackendExits(t *testing.T) {
	b := newFakeBackend(t)
	close(b.release)
	_, _, c, _, done := runFake(t, b)
	waitStarted(t, c, done)

	b.running.exit()

	if err := wait(t, done); err == nil || !strings.Contains(err.Error(), "the fake backend exited") {
		t.Fatalf("Relay.Run() error = %v, want the fake backend exited", err)
	}
}

func TestRunStops(t *testing.T) {
	b := newFakeBackend(t)
	close(b.release)
	d, _, c, cancel, done := runFake(t, b)
	waitStarted(t, c, done)

	cancel()

	if err := wait(t, done); err != nil {
		t.Fatalf("Relay.Run() error = %v", err)
	}
	if !b.running.stopped {
		t.Fatal("Relay.Run() returned without stopping the backend")
	}
	if _, err := os.Stat(d.SocketPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Stat() of the control socket after the relay stopped error = %v, want %v", err, os.ErrNotExist)
	}
	release, err := d.Lock()
	if err != nil {
		t.Fatalf("Lock() after the relay stopped error = %v", err)
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
}

func TestRunStopsWhileStarting(t *testing.T) {
	d, _, _, cancel, done := runFake(t, newFakeBackend(t))

	cancel()

	if err := wait(t, done); err != nil {
		t.Fatalf("Relay.Run() error = %v", err)
	}
	if _, err := os.Stat(d.SocketPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Stat() of the control socket after the relay stopped error = %v, want %v", err, os.ErrNotExist)
	}
}

func TestRunRestartsWhenAsked(t *testing.T) {
	b := newFakeBackend(t)
	close(b.release)
	_, _, c, _, done := runFake(t, b)
	waitStarted(t, c, done)

	if err := c.Restart(context.Background()); err != nil {
		t.Fatalf("Client.Restart() error = %v", err)
	}

	if err := wait(t, done); !errors.Is(err, ErrRestart) {
		t.Fatalf("Relay.Run() error = %v, want %v", err, ErrRestart)
	}
}
