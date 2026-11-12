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

package meshbackend

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/a2aproject/a2a-serve/internal/relay"
	"github.com/a2aproject/a2a-serve/internal/relay/control"
	"github.com/a2aproject/a2a-serve/internal/relay/meshadmin"
	"github.com/a2aproject/a2a-serve/internal/relaydir"
	"github.com/a2aproject/a2a-serve/internal/testutil"
)

// fakeMeshEnv makes the test binary act as agentmesh-one. Its value is how
// long the fake runs before it exits on its own, "" to run until SIGTERM, or
// one of the modes below.
const fakeMeshEnv = "A2A_SERVE_TEST_FAKE_AGENTMESH"

const (
	// fakeMeshPortTaken makes the first fake hand its port to a holder
	// process and exit, as if another process took the port first.
	fakeMeshPortTaken = "port-taken"
	fakeMeshHoldPort  = "hold-port"
	// fakeMeshServing is created in the data directory by a fake that serves.
	fakeMeshServing = "serving"
)

func TestMain(m *testing.M) {
	if v, ok := os.LookupEnv(fakeMeshEnv); ok {
		os.Exit(runFakeMesh(v))
	}
	os.Exit(m.Run())
}

func runFakeMesh(exitAfter string) int {
	if exitAfter == fakeMeshHoldPort {
		time.Sleep(5 * time.Second)
		return 0
	}
	i := slices.Index(os.Args, "--port")
	if i < 0 || i+1 >= len(os.Args) {
		return 2
	}
	ln, err := net.Listen("tcp", "127.0.0.1:"+os.Args[i+1])
	if err != nil {
		return 2
	}
	if exitAfter == fakeMeshPortTaken {
		const marker = "port-taken"
		if _, err := os.Stat(marker); errors.Is(err, os.ErrNotExist) {
			if err := os.WriteFile(marker, nil, 0o600); err != nil {
				return 1
			}
			if err := holdPort(ln.(*net.TCPListener)); err != nil {
				return 1
			}
			return 2
		}
	}
	if err := os.WriteFile(fakeMeshServing, nil, 0o600); err != nil {
		return 1
	}
	go func() {
		if err := http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/admin/") {
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

// holdPort passes ln to a process that keeps it open for a while.
func holdPort(ln *net.TCPListener) error {
	f, err := ln.File()
	if err != nil {
		return err
	}
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), fakeMeshEnv+"="+fakeMeshHoldPort)
	cmd.ExtraFiles = []*os.File{f}
	return errors.Join(cmd.Start(), f.Close())
}

// startFake starts the backend over the fake agentmesh-one in a new relay
// directory.
func startFake(t *testing.T, exitAfter string) (relaydir.Dir, relay.Running, error) {
	t.Helper()
	t.Setenv(fakeMeshEnv, exitAfter)
	d := relaydir.Dir{Root: testutil.ShortTempDir(t)}
	if _, err := d.SaveSettings(relaydir.Settings{URL: "http://127.0.0.1:8443", Listen: "127.0.0.1:8443"}); err != nil {
		t.Fatalf("SaveSettings() error = %v", err)
	}
	conf, err := d.Resolve()
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	b := New(Options{Binary: os.Args[0], Log: io.Discard, Now: time.Now})
	r, err := b.Start(t.Context(), relay.Start{
		Dir: d, Config: conf, URL: conf.URL,
		Status: func() control.Status { return control.Status{} },
		Admits: func(control.Member) bool { return false },
	})
	if err == nil {
		t.Cleanup(func() {
			if err := r.Stop(); err != nil {
				t.Errorf("Running.Stop() error = %v", err)
			}
		})
	}
	return d, r, err
}

func TestStartReportsWhenAgentMeshExits(t *testing.T) {
	_, r, err := startFake(t, "1s")
	if err != nil {
		t.Fatalf("Backend.Start() error = %v", err)
	}

	select {
	case <-r.Exited():
	case <-time.After(30 * time.Second):
		t.Fatal("Running.Exited() didn't close after agentmesh-one exited")
	}

	if err := r.Err(); err == nil || !strings.Contains(err.Error(), "agentmesh-one exited") {
		t.Fatalf("Running.Err() = %v, want agentmesh-one exited", err)
	}
}

func TestStartPicksAnotherPortWhenAgentMeshLosesIt(t *testing.T) {
	d, r, err := startFake(t, fakeMeshPortTaken)
	if err != nil {
		t.Fatalf("Backend.Start() error = %v", err)
	}

	if _, err := os.Stat(filepath.Join(d.MeshDir(), fakeMeshServing)); err != nil {
		t.Fatalf("Stat() of the serving marker error = %v, want agentmesh-one serving on another port", err)
	}
	if !strings.HasPrefix(r.Upstream().String(), "http://127.0.0.1:") {
		t.Fatalf("Running.Upstream() = %v, want a loopback URL", r.Upstream())
	}
}

func TestStopStopsAgentMesh(t *testing.T) {
	_, r, err := startFake(t, "")
	if err != nil {
		t.Fatalf("Backend.Start() error = %v", err)
	}

	if err := r.Stop(); err != nil {
		t.Fatalf("Running.Stop() error = %v", err)
	}

	select {
	case <-r.Exited():
	default:
		t.Fatal("Running.Exited() is open after Stop")
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

func TestWaitReadyReportsAnExitWithoutProbingAgain(t *testing.T) {
	t.Parallel()
	var probes atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		probes.Add(1)
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)
	mesh := &meshProcess{exited: make(chan struct{}), err: errors.New("exit status 2")}
	close(mesh.exited)

	err := waitReady(context.Background(), meshadmin.New(srv.URL, "", srv.Client()), mesh, 1)

	if err == nil || !strings.Contains(err.Error(), "exited while starting") {
		t.Fatalf("waitReady() error = %v, want agentmesh-one exited while starting", err)
	}
	if got := probes.Load(); got != 1 {
		t.Fatalf("waitReady() sent %d readiness probes after agentmesh-one exited, want 1", got)
	}
}
