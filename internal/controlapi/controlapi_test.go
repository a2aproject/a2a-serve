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
	"syscall"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/agenterr"
	"github.com/a2aproject/a2a-serve/internal/clierr"
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
		{name: "preview revoke", call: func(ctx context.Context, c *Client) error { _, err := c.PreviewRevoke(ctx, "a/b"); return err }, wantMethod: http.MethodGet, wantPath: "/pair/a%2Fb/removal"},
		{name: "sign out", call: func(ctx context.Context, c *Client) error {
			_, err := c.SignOut(ctx, "anna@example.com")
			return err
		}, wantMethod: http.MethodDelete, wantPath: "/signins/anna@example.com"},
		{name: "delete contact", call: func(ctx context.Context, c *Client) error { return c.DeleteContact(ctx, "coder") }, wantMethod: http.MethodDelete, wantPath: "/contacts/coder"},
		{name: "delete messenger", call: func(ctx context.Context, c *Client) error { return c.DeleteMessenger(ctx, "telegram") }, wantMethod: http.MethodDelete, wantPath: "/messengers/telegram"},
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
					if _, err := w.Write([]byte(`{"error":"unknown messenger"}`)); err != nil {
						t.Errorf("Write() error = %v", err)
					}
				}))
			},
			wantErr: "unknown messenger",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := NewClient(tc.dir(t)).DeleteMessenger(t.Context(), "telegram")
			if err == nil {
				t.Fatalf("Client.DeleteMessenger() error = nil, want an error")
			}
			if got := errors.Is(err, agenterr.ErrNotRunning); got != tc.wantNotRunning {
				t.Fatalf("Client.DeleteMessenger() error = %v, want ErrNotRunning %v", err, tc.wantNotRunning)
			}
			if tc.wantErr != "" && err.Error() != tc.wantErr {
				t.Fatalf("Client.DeleteMessenger() error = %v, want %v", err, tc.wantErr)
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
			err:     fmt.Errorf("removing: %w", &agenterr.NotFoundError{Kind: "messenger", Name: "telegram"}),
			wantErr: &agenterr.NotFoundError{Kind: "messenger", Name: "telegram"},
		},
		{
			name:    "invalid",
			err:     agenterr.Invalidf("unknown messenger irc"),
			wantErr: agenterr.Invalidf("unknown messenger irc"),
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

			err := NewClient(dir).DeleteMessenger(t.Context(), "telegram")

			var ce *clierr.Error
			if errors.As(err, &ce) {
				err = ce.Unwrap()
			}
			if diff := cmp.Diff(tc.wantErr, err, cmp.Comparer(func(a, b error) bool { return fmt.Sprintf("%T %v", a, a) == fmt.Sprintf("%T %v", b, b) })); diff != "" {
				t.Fatalf("Client.DeleteMessenger() wrong error (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestClientErrorClass(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		err      error
		wantCode string
		wantExit int
		wantHint string
	}{
		{
			name:     "unreachable URL",
			err:      fmt.Errorf("can't reach http://127.0.0.1:1: %w", &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}),
			wantCode: clierr.CodeIO,
			wantExit: 3,
			wantHint: clierr.Classify(&net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}).Hint,
		},
		{
			name:     "busy",
			err:      &agenterr.BusyError{Name: "coder"},
			wantCode: clierr.CodeBusy,
			wantExit: 8,
		},
		{
			name:     "not found",
			err:      &agenterr.NotFoundError{Kind: "contact", Name: "grok"},
			wantCode: clierr.CodeUsage,
			wantExit: 2,
			wantHint: clierr.UsageHint,
		},
		{
			name:     "other",
			err:      errors.New("database is locked"),
			wantCode: clierr.CodeInternal,
			wantExit: 1,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := serveSocket(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { WriteError(w, tc.err) }))

			_, err := NewClient(dir).AddContact(t.Context(), ContactRequest{Name: "grok"})

			got := clierr.Classify(err)
			want := &clierr.Error{Code: tc.wantCode, Message: tc.err.Error(), Hint: tc.wantHint, Exit: tc.wantExit}
			if diff := cmp.Diff(want, got, cmpopts.IgnoreUnexported(clierr.Error{})); diff != "" {
				t.Fatalf("Client.AddContact() wrong error class (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestClientErrorWithoutClass(t *testing.T) {
	t.Parallel()
	dir := serveSocket(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		if _, err := w.Write([]byte(`{"error":"can't reach http://127.0.0.1:1"}`)); err != nil {
			t.Errorf("Write() error = %v", err)
		}
	}))

	_, err := NewClient(dir).AddContact(t.Context(), ContactRequest{Name: "grok"})

	if got := clierr.Classify(err); got.Code != clierr.CodeInternal || got.Message != "can't reach http://127.0.0.1:1" {
		t.Fatalf("Client.AddContact() error = %s %q, want %s from a server without error classes", got.Code, got.Message, clierr.CodeInternal)
	}
}

func TestClientRequestErrorKeepsHint(t *testing.T) {
	t.Parallel()
	err := agenterr.WithHint(agenterr.Conflictf("anna is already paired"), agenterr.HintPairRm, "anna")
	dir := serveSocket(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { WriteError(w, err) }))

	got := clierr.Classify(NewClient(dir).DeleteMessenger(t.Context(), "telegram"))

	want := &clierr.Error{Code: clierr.CodeUsage, Message: "anna is already paired", Hint: "pick another name, or run `a2a-serve pair rm anna` first", Exit: 2}
	if diff := cmp.Diff(want, got, cmpopts.IgnoreUnexported(clierr.Error{})); diff != "" {
		t.Fatalf("Client.DeleteMessenger() wrong error (-want +got) diff = %s", diff)
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
