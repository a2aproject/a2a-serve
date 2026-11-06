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

package cli

import (
	"errors"
	"net"
	"net/http"
	"os"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/controlapi"
	"github.com/a2aproject/a2a-serve/internal/jsonhttp"
)

// fakeControl answers the control socket of a running agent named coder.
type fakeControl struct {
	mu    sync.Mutex
	calls []string
}

func (f *fakeControl) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)
	f.mu.Unlock()
	switch r.URL.Path {
	case "/status":
		jsonhttp.Write(w, http.StatusOK, controlapi.Status{Name: "coder", Harness: "opencode", PID: 42, Started: time.Now(), Local: "http://127.0.0.1:4100"})
	case "/config":
		jsonhttp.Write(w, http.StatusOK, agentdir.Config{Name: "coder", Harness: "opencode"})
	default:
		jsonhttp.Write(w, http.StatusOK, struct{}{})
	}
}

func (f *fakeControl) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

func serveFakeControl(t *testing.T, d agentdir.Dir) *fakeControl {
	t.Helper()
	ln, err := net.Listen("unix", d.SocketPath())
	if err != nil {
		t.Fatalf("net.Listen() error = %v", err)
	}
	f := &fakeControl{}
	srv := &http.Server{Handler: f, ReadHeaderTimeout: time.Second}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("Serve() error = %v", err)
		}
	}()
	t.Cleanup(func() {
		if err := srv.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})
	return f
}

// breakConfig replaces config.yaml of d with content, or deletes it for
// nil, leaving the state directory of a server that ran there.
func breakConfig(t *testing.T, d agentdir.Dir, content []byte) {
	t.Helper()
	if err := d.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := os.WriteFile(d.LockPath(), nil, 0o600); err != nil {
		t.Fatalf("writing the lock file: %v", err)
	}
	if content == nil {
		if err := os.Remove(d.ConfigPath()); err != nil {
			t.Fatalf("removing config.yaml: %v", err)
		}
		return
	}
	if err := os.WriteFile(d.ConfigPath(), content, 0o644); err != nil {
		t.Fatalf("writing config.yaml: %v", err)
	}
}

var brokenConfigs = []struct {
	name    string
	content []byte
	wantErr string
}{
	{name: "invalid setting", content: []byte("name: coder\nagent: opencode\nsandbox: bogus\n"), wantErr: "config.yaml: sandbox has \"bogus\": want docker or local: fix it with `a2a-serve config unset sandbox`"},
	{name: "invalid YAML", content: []byte("name: [coder\n"), wantErr: "config.yaml: yaml: line 1"},
	{name: "empty", content: []byte{}, wantErr: "no agent"},
	{name: "deleted", wantErr: "config.yaml does not exist"},
}

func TestStopWithBrokenConfig(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("unix sockets are not used on this platform")
	}
	for _, tc := range brokenConfigs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := newTestAgent(t, &agentdir.Config{Name: "coder", Harness: "opencode"})
			breakConfig(t, d, tc.content)
			f := serveFakeControl(t, d)

			out, err := runCMD(t, "stop", "--dir", d.Root)
			if err != nil {
				t.Fatalf("a2a-serve stop error = %v", err)
			}
			if !strings.HasPrefix(out, "✓ stopped coder\n") || !strings.Contains(out, "! the config is invalid: ") || !strings.Contains(out, tc.wantErr) {
				t.Fatalf("a2a-serve stop = %q, want the agent stopped and a note with %q", out, tc.wantErr)
			}
			if diff := cmp.Diff([]string{"GET /status", "POST /stop"}, f.Calls()); diff != "" {
				t.Fatalf("a2a-serve stop wrong calls (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestStopStoppedAgentWithBrokenConfig(t *testing.T) {
	t.Parallel()
	for _, tc := range brokenConfigs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := newTestAgent(t, &agentdir.Config{Name: "coder", Harness: "opencode"})
			breakConfig(t, d, tc.content)

			out, err := runCMD(t, "stop", "--dir", d.Root)
			if err != nil {
				t.Fatalf("a2a-serve stop error = %v", err)
			}
			if !strings.Contains(out, " is not running\n") || !strings.Contains(out, tc.wantErr) {
				t.Fatalf("a2a-serve stop = %q, want not running and a note with %q", out, tc.wantErr)
			}
		})
	}
}

func TestStatusWithBrokenConfig(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("unix sockets are not used on this platform")
	}
	for _, tc := range brokenConfigs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := newTestAgent(t, &agentdir.Config{Name: "coder", Harness: "opencode"})
			breakConfig(t, d, tc.content)
			serveFakeControl(t, d)

			out, err := runCMD(t, "status", "--dir", d.Root)
			if err != nil {
				t.Fatalf("a2a-serve status error = %v", err)
			}
			if !strings.HasPrefix(out, "coder (opencode) running for ") || !strings.Contains(out, "! the config is invalid: ") || !strings.Contains(out, tc.wantErr) {
				t.Fatalf("a2a-serve status = %q, want the running agent and a note with %q", out, tc.wantErr)
			}
		})
	}
}

func TestStatusStoppedAgentWithBrokenConfig(t *testing.T) {
	t.Parallel()
	for _, tc := range brokenConfigs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := newTestAgent(t, &agentdir.Config{Name: "coder", Harness: "opencode"})
			breakConfig(t, d, tc.content)

			out, err := runCMD(t, "status", "--dir", d.Root)
			if err != nil {
				t.Fatalf("a2a-serve status error = %v", err)
			}
			if !strings.Contains(out, " is not running") || !strings.Contains(out, tc.wantErr) {
				t.Fatalf("a2a-serve status = %q, want not running and a note with %q", out, tc.wantErr)
			}
		})
	}
}
