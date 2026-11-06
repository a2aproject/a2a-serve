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
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/a2aproject/a2a-serve/internal/relay/control"
	"github.com/a2aproject/a2a-serve/internal/relaydir"
)

// fakeMeshEnv makes the test binary act as agentmesh-one. Its value is how
// long the fake runs before it exits on its own, or "" to run until SIGTERM.
const fakeMeshEnv = "A2A_SERVE_TEST_FAKE_AGENTMESH"

func TestMain(m *testing.M) {
	if v, ok := os.LookupEnv(fakeMeshEnv); ok {
		os.Exit(runFakeMesh(v))
	}
	os.Exit(m.Run())
}

func runFakeMesh(exitAfter string) int {
	i := slices.Index(os.Args, "--port")
	if i < 0 || i+1 >= len(os.Args) {
		return 2
	}
	ln, err := net.Listen("tcp", "127.0.0.1:"+os.Args[i+1])
	if err != nil {
		return 2
	}
	go func() {
		if err := http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/admin/status" {
				if _, err := io.WriteString(w, "{}"); err != nil {
					return
				}
			}
		})); err != nil {
			return
		}
	}()
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM)
	var after <-chan time.Time
	if d, err := time.ParseDuration(exitAfter); err == nil {
		after = time.After(d)
	}
	select {
	case <-stop:
		return 0
	case <-after:
		return 3
	}
}

// runFake runs a relay over the fake agentmesh-one and returns its
// directory, its control client, and the result of Run.
func runFake(t *testing.T, exitAfter string) (relaydir.Dir, *control.Client, context.CancelFunc, <-chan error) {
	t.Helper()
	t.Setenv(fakeMeshEnv, exitAfter)
	dir, err := os.MkdirTemp("", "relay")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Error(err)
		}
	})
	d := relaydir.Dir{Root: dir}
	port, err := freePort()
	if err != nil {
		t.Fatalf("freePort() error = %v", err)
	}
	if _, err := d.SaveSettings(relaydir.Settings{URL: fmt.Sprintf("http://127.0.0.1:%d", port), Listen: fmt.Sprintf("127.0.0.1:%d", port)}); err != nil {
		t.Fatalf("SaveSettings() error = %v", err)
	}
	release, err := d.Lock()
	if err != nil {
		t.Fatalf("Lock() error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		err := New(Options{Dir: d, Binary: os.Args[0], A2AServe: "test", Out: io.Discard, MeshLog: io.Discard, Now: time.Now}).Run(ctx)
		done <- errors.Join(err, release())
	}()
	t.Cleanup(cancel)
	return d, control.NewSocketClient(d.SocketPath(), 5*time.Second), cancel, done
}

func waitRunning(t *testing.T, c *control.Client, done <-chan error) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		select {
		case err := <-done:
			t.Fatalf("Relay.Run() error = %v before the relay ran", err)
		default:
		}
		if _, err := c.Members(context.Background(), false); err == nil || !errors.Is(err, control.ErrNotRunning) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the relay didn't answer on its control socket")
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

func TestRunReturnsWhenAgentMeshExits(t *testing.T) {
	_, c, _, done := runFake(t, "1s")
	waitRunning(t, c, done)

	err := wait(t, done)

	if err == nil || !strings.Contains(err.Error(), "agentmesh-one exited") {
		t.Fatalf("Relay.Run() error = %v, want agentmesh-one exited", err)
	}
}

func TestRunStops(t *testing.T) {
	d, c, cancel, done := runFake(t, "")
	waitRunning(t, c, done)

	cancel()

	if err := wait(t, done); err != nil {
		t.Fatalf("Relay.Run() error = %v", err)
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
	d, _, cancel, done := runFake(t, "")

	cancel()

	if err := wait(t, done); err != nil {
		t.Fatalf("Relay.Run() error = %v", err)
	}
	if _, err := os.Stat(d.SocketPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Stat() of the control socket after the relay stopped error = %v, want %v", err, os.ErrNotExist)
	}
}

func TestRunRestartsWhenAsked(t *testing.T) {
	_, c, _, done := runFake(t, "")
	waitRunning(t, c, done)

	if err := c.Restart(context.Background()); err != nil {
		t.Fatalf("Client.Restart() error = %v", err)
	}

	if err := wait(t, done); !errors.Is(err, ErrRestart) {
		t.Fatalf("Relay.Run() error = %v, want %v", err, ErrRestart)
	}
}

func TestFreePortIsBelowTheEphemeralRange(t *testing.T) {
	t.Parallel()
	port, err := freePort()
	if err != nil {
		t.Fatalf("freePort() error = %v", err)
	}
	if port < freePortMin || port >= freePortMax {
		t.Fatalf("freePort() = %d, want in [%d, %d)", port, freePortMin, freePortMax)
	}
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatalf("Listen() on the port from freePort() error = %v", err)
	}
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
}
