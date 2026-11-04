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

package controlapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/agenterr"
	"github.com/a2aproject/a2a-serve/internal/fsutil"
)

func newDir(t *testing.T) agentdir.Dir {
	t.Helper()
	dir, err := agentdir.Open(t.TempDir())
	if err != nil {
		t.Fatalf("agentdir.Open() error = %v", err)
	}
	return dir
}

func listenSocket(t *testing.T, dir agentdir.Dir) *net.UnixListener {
	t.Helper()
	path := dir.SocketPath()
	if err := fsutil.MkdirPrivate(filepath.Dir(path)); err != nil {
		t.Fatalf("fsutil.MkdirPrivate() error = %v", err)
	}
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatalf("net.ListenUnix() error = %v", err)
	}
	return ln
}

func serveSocket(t *testing.T, h http.Handler) agentdir.Dir {
	t.Helper()
	dir := newDir(t)
	ln := listenSocket(t, dir)
	srv := &http.Server{Handler: h, ReadHeaderTimeout: time.Second}
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
	return dir
}

func TestClientRequests(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		call       func(ctx context.Context, c *Client) error
		wantMethod string
		wantPath   string
		wantBody   string
	}{
		{name: "status", call: func(ctx context.Context, c *Client) error { _, err := c.Status(ctx); return err }, wantMethod: http.MethodGet, wantPath: "/status"},
		{name: "stop", call: func(ctx context.Context, c *Client) error { return c.Stop(ctx) }, wantMethod: http.MethodPost, wantPath: "/stop"},
		{name: "pair", call: func(ctx context.Context, c *Client) error {
			_, err := c.Pair(ctx, PairCodeRequest{Name: "anna", Owner: true})
			return err
		}, wantMethod: http.MethodPost, wantPath: "/pair", wantBody: `{"name":"anna","owner":true}`},
		{name: "revoke", call: func(ctx context.Context, c *Client) error { _, err := c.Revoke(ctx, "a/b"); return err }, wantMethod: http.MethodDelete, wantPath: "/pair/a%2Fb"},
		{name: "delete contact", call: func(ctx context.Context, c *Client) error { return c.DeleteContact(ctx, "coder") }, wantMethod: http.MethodDelete, wantPath: "/contacts/coder"},
		{name: "delete channel", call: func(ctx context.Context, c *Client) error { return c.DeleteChannel(ctx, "telegram") }, wantMethod: http.MethodDelete, wantPath: "/channels/telegram"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			type request struct{ Method, Path, Body string }
			got := make(chan request, 1)
			dir := serveSocket(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body json.RawMessage
				if r.Body != http.NoBody {
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Errorf("decoding the request body error = %v", err)
					}
				}
				got <- request{Method: r.Method, Path: r.URL.EscapedPath(), Body: string(body)}
				if _, err := w.Write([]byte("{}")); err != nil {
					t.Errorf("Write() error = %v", err)
				}
			}))
			if err := tc.call(t.Context(), NewClient(dir)); err != nil {
				t.Fatalf("Client call error = %v", err)
			}
			want := request{Method: tc.wantMethod, Path: tc.wantPath, Body: tc.wantBody}
			if diff := cmp.Diff(want, <-got); diff != "" {
				t.Fatalf("Client call wrong request (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestClientStatus(t *testing.T) {
	t.Parallel()
	want := &Status{Name: "coder", Harness: "opencode", PID: 42, Local: "http://127.0.0.1:1", Tasks: map[string]int{"completed": 2}}
	dir := serveSocket(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewEncoder(w).Encode(want); err != nil {
			t.Errorf("Encode() error = %v", err)
		}
	}))
	got, err := NewClient(dir).Status(t.Context())
	if err != nil {
		t.Fatalf("Client.Status() error = %v", err)
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("Client.Status() wrong result (-want +got) diff = %s", diff)
	}
}

func TestClientErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name           string
		dir            func(t *testing.T) agentdir.Dir
		wantNotRunning bool
		wantErr        string
	}{
		{
			name:           "no socket",
			dir:            newDir,
			wantNotRunning: true,
		},
		{
			name: "stale socket",
			dir: func(t *testing.T) agentdir.Dir {
				dir := newDir(t)
				ln := listenSocket(t, dir)
				ln.SetUnlinkOnClose(false)
				if err := ln.Close(); err != nil {
					t.Fatalf("Close() error = %v", err)
				}
				return dir
			},
			wantNotRunning: true,
		},
		{
			name: "connection reset by the server",
			dir: func(t *testing.T) agentdir.Dir {
				dir := newDir(t)
				ln := listenSocket(t, dir)
				t.Cleanup(func() {
					if err := ln.Close(); err != nil {
						t.Errorf("Close() error = %v", err)
					}
				})
				go func() {
					for {
						c, err := ln.AcceptUnix()
						if err != nil {
							return
						}
						// Closing with the request partly unread resets the connection.
						if _, err := c.Read(make([]byte, 1)); err != nil {
							t.Errorf("Read() error = %v", err)
						}
						if err := c.Close(); err != nil {
							t.Errorf("Close() error = %v", err)
						}
					}
				}()
				return dir
			},
		},
		{
			name: "error response",
			dir: func(t *testing.T) agentdir.Dir {
				return serveSocket(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusBadRequest)
					if _, err := w.Write([]byte(`{"error":"unknown channel"}`)); err != nil {
						t.Errorf("Write() error = %v", err)
					}
				}))
			},
			wantErr: "unknown channel",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := NewClient(tc.dir(t)).DeleteChannel(t.Context(), "telegram")
			if err == nil {
				t.Fatalf("Client.DeleteChannel() error = nil, want an error")
			}
			if got := errors.Is(err, ErrNotRunning); got != tc.wantNotRunning {
				t.Fatalf("Client.DeleteChannel() error = %v, want ErrNotRunning %v", err, tc.wantNotRunning)
			}
			if tc.wantErr != "" && err.Error() != tc.wantErr {
				t.Fatalf("Client.DeleteChannel() error = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestClientRequestErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		err     error
		wantErr error
	}{
		{
			name:    "not found",
			err:     fmt.Errorf("removing: %w", &agenterr.NotFoundError{Kind: "channel", Name: "telegram"}),
			wantErr: &agenterr.NotFoundError{Kind: "channel", Name: "telegram"},
		},
		{
			name:    "invalid",
			err:     agenterr.Invalidf("unknown channel irc"),
			wantErr: agenterr.Invalidf("unknown channel irc"),
		},
		{
			name:    "conflict",
			err:     agenterr.Conflictf("anna is already paired"),
			wantErr: agenterr.Conflictf("anna is already paired"),
		},
		{
			name:    "other",
			err:     errors.New("database is locked"),
			wantErr: errors.New("database is locked"),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := serveSocket(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { WriteError(w, tc.err) }))

			err := NewClient(dir).DeleteChannel(t.Context(), "telegram")

			if diff := cmp.Diff(tc.wantErr, err, cmp.Comparer(func(a, b error) bool { return fmt.Sprintf("%T %v", a, a) == fmt.Sprintf("%T %v", b, b) })); diff != "" {
				t.Fatalf("Client.DeleteChannel() wrong error (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestClientWithTimeout(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		timeout time.Duration
		wantErr bool
	}{
		{name: "call slower than the timeout", timeout: 50 * time.Millisecond, wantErr: true},
		{name: "zero keeps the default", timeout: 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := serveSocket(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				select {
				case <-time.After(300 * time.Millisecond):
				case <-r.Context().Done():
					return
				}
				w.Header().Set("Content-Type", "application/json")
				if _, err := w.Write([]byte(`{"name":"coder"}`)); err != nil {
					t.Errorf("Write() error = %v", err)
				}
			}))

			_, err := NewClient(dir, WithTimeout(tc.timeout)).Status(t.Context())

			if gotErr := err != nil; gotErr != tc.wantErr {
				t.Fatalf("Client.Status() error = %v, want error %t", err, tc.wantErr)
			}
		})
	}
}

func TestStatusURL(t *testing.T) {
	t.Parallel()
	all := &Status{Name: "coder", Local: "http://127.0.0.1:4000", Private: "http://coder.tail.ts.net", Public: "https://coder.example.com"}
	localOnly := &Status{Name: "coder", Local: "http://127.0.0.1:4000"}
	tests := []struct {
		name    string
		status  *Status
		scope   string
		want    string
		wantErr string
	}{
		{name: "best is public", status: all, want: "https://coder.example.com"},
		{name: "best is private without public", status: &Status{Local: "http://127.0.0.1:4000", Private: "https://coder.tail.ts.net"}, want: "https://coder.tail.ts.net"},
		{name: "best is local without networks", status: localOnly, want: "http://127.0.0.1:4000"},
		{name: "local", status: all, scope: ScopeLocal, want: "http://127.0.0.1:4000"},
		{name: "private", status: all, scope: ScopePrivate, want: "http://coder.tail.ts.net"},
		{name: "public", status: all, scope: ScopePublic, want: "https://coder.example.com"},
		{name: "no private URL", status: localOnly, scope: ScopePrivate, wantErr: "coder has no private URL; add one with `a2a-serve config set --private tailscale` (or a relay URL), then `a2a-serve restart`"},
		{name: "no public URL", status: localOnly, scope: ScopePublic, wantErr: "coder has no public URL; add one with `a2a-serve config set --public tailscale` (or a relay URL), then `a2a-serve restart`"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := tc.status.URL(tc.scope)
			if tc.wantErr != "" {
				if err == nil || err.Error() != tc.wantErr {
					t.Fatalf("Status.URL(%q) error = %v, want %q", tc.scope, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Status.URL(%q) error = %v, want nil", tc.scope, err)
			}
			if got != tc.want {
				t.Fatalf("Status.URL(%q) = %q, want %q", tc.scope, got, tc.want)
			}
		})
	}
}

func TestStatusInspectorURL(t *testing.T) {
	t.Parallel()
	const plainHTTP = "the inspector isn't served on http://192.168.1.5:4000, because its login link and session would cross the network unencrypted; run `a2a-serve inspect` without --private on the agent's machine"
	tests := []struct {
		name    string
		status  *Status
		private bool
		want    string
		wantErr string
	}{
		{name: "local", status: &Status{Local: "http://127.0.0.1:4000", Private: "http://192.168.1.5:4000"}, want: "http://127.0.0.1:4000"},
		{name: "tailnet", status: &Status{Local: "http://127.0.0.1:4000", Private: "https://coder.tail.ts.net"}, private: true, want: "https://coder.tail.ts.net"},
		{name: "plain HTTP", status: &Status{Local: "http://127.0.0.1:4000", Private: "http://192.168.1.5:4000"}, private: true, wantErr: plainHTTP},
		{name: "no private URL", status: &Status{Name: "coder", Local: "http://127.0.0.1:4000"}, private: true, wantErr: "coder has no private URL; add one with `a2a-serve config set --private tailscale` (or a relay URL), then `a2a-serve restart`"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := tc.status.InspectorURL(tc.private)
			if tc.wantErr != "" {
				if err == nil || err.Error() != tc.wantErr {
					t.Fatalf("Status.InspectorURL(%v) error = %v, want %q", tc.private, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Status.InspectorURL(%v) error = %v, want nil", tc.private, err)
			}
			if got != tc.want {
				t.Fatalf("Status.InspectorURL(%v) = %q, want %q", tc.private, got, tc.want)
			}
		})
	}
}

func TestLocalOnly(t *testing.T) {
	t.Parallel()
	tests := []struct {
		url  string
		want bool
	}{
		{url: "http://127.0.0.1:4000", want: true},
		{url: "http://localhost:4000/n/grok", want: true},
		{url: "http://[::1]:4000", want: true},
		{url: "http://coder.tail.ts.net", want: false},
		{url: "https://coder.example.com", want: false},
		{url: "http://192.168.1.10:4000", want: false},
	}
	for _, tc := range tests {
		t.Run(tc.url, func(t *testing.T) {
			t.Parallel()
			if got := LocalOnly(tc.url); got != tc.want {
				t.Fatalf("LocalOnly(%q) = %v, want %v", tc.url, got, tc.want)
			}
		})
	}
}
