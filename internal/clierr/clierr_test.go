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

package clierr

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/a2aproject/a2a-serve/internal/agenterr"
)

func TestClassify(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		err      error
		wantCode string
		wantA2A  string
		wantExit int
	}{
		{
			name:     "already-classified usage error passes through",
			err:      Usage("bad flag combo"),
			wantCode: CodeUsage,
			wantExit: 2,
		},
		{
			name:     "error marked as a usage error",
			err:      fmt.Errorf("loading: %w", markedUsage{}),
			wantCode: CodeUsage,
			wantExit: 2,
		},
		{
			name:     "missing pairing",
			err:      fmt.Errorf("pair rm: %w", &agenterr.NotFoundError{Kind: "pairing", Name: "anna"}),
			wantCode: CodeUsage,
			wantExit: 2,
		},
		{
			name:     "invalid request",
			err:      agenterr.Invalidf("unknown messenger %s", "irc"),
			wantCode: CodeUsage,
			wantExit: 2,
		},
		{
			name:     "conflicting request",
			err:      agenterr.Conflictf("anna is already paired"),
			wantCode: CodeUsage,
			wantExit: 2,
		},
		{
			name:     "agent server starting",
			err:      &agenterr.BusyError{Name: "coder"},
			wantCode: CodeBusy,
			wantExit: 8,
		},
		{
			name:     "agent server not running",
			err:      fmt.Errorf("coder: %w", NotRunning(errors.New("the agent server is not running"))),
			wantCode: CodeNotRunning,
			wantExit: 6,
		},
		{
			name:     "agent server not running, from agenterr",
			err:      fmt.Errorf("coder: %w", agenterr.NotRunning("coder")),
			wantCode: CodeNotRunning,
			wantExit: 6,
		},
		{
			name:     "no agent directory",
			err:      fmt.Errorf("loading: %w", NoAgent(errors.New("no .a2a directory found"))),
			wantCode: CodeNoAgent,
			wantExit: 7,
		},
		{
			name:     "deadline exceeded is a timeout",
			err:      fmt.Errorf("waiting: %w", context.DeadlineExceeded),
			wantCode: CodeTimeout,
			wantExit: 5,
		},
		{
			name:     "protocol error carries the A2A reason in a2aCode, exit 1",
			err:      fmt.Errorf("failed to get task x: %w", a2a.NewError(a2a.ErrTaskNotFound, "task not found")),
			wantCode: CodeProtocol,
			wantA2A:  "TASK_NOT_FOUND",
			wantExit: 1,
		},
		{
			name:     "auth is a protocol error, not a special case",
			err:      fmt.Errorf("send: %w", a2a.NewError(a2a.ErrUnauthenticated, "unauthenticated")),
			wantCode: CodeProtocol,
			wantA2A:  "UNAUTHENTICATED",
			wantExit: 1,
		},
		{
			name:     "net error is io, exit 3",
			err:      fmt.Errorf("resolving agent card: %w", &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}),
			wantCode: CodeIO,
			wantExit: 3,
		},
		{
			name:     "connection reset sentinel is io, exit 3",
			err:      fmt.Errorf("reading stream: %w", syscall.ECONNRESET),
			wantCode: CodeIO,
			wantExit: 3,
		},
		{
			name:     "missing file is internal, exit 1",
			err:      fmt.Errorf("restoring: %w", &fs.PathError{Op: "open", Path: "/a/nope", Err: syscall.ENOENT}),
			wantCode: CodeInternal,
			wantExit: 1,
		},
		{
			name:     "port in use is internal, exit 1",
			err:      &net.OpError{Op: "listen", Net: "tcp", Err: &os.SyscallError{Syscall: "bind", Err: syscall.EADDRINUSE}},
			wantCode: CodeInternal,
			wantExit: 1,
		},
		{
			name:     "unclassified error is internal, exit 1",
			err:      errors.New("something odd happened"),
			wantCode: CodeInternal,
			wantExit: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := Classify(tt.err)
			if got == nil {
				t.Fatalf("Classify(%v) = nil, want a classified error", tt.err)
			}
			if got.Code != tt.wantCode {
				t.Errorf("Classify(%v).Code = %q, want %q", tt.err, got.Code, tt.wantCode)
			}
			if got.A2ACode != tt.wantA2A {
				t.Errorf("Classify(%v).A2ACode = %q, want %q", tt.err, got.A2ACode, tt.wantA2A)
			}
			if got.Exit != tt.wantExit {
				t.Errorf("Classify(%v).Exit = %d, want %d", tt.err, got.Exit, tt.wantExit)
			}
		})
	}
}

type markedUsage struct{}

func (markedUsage) Error() string    { return "invalid setting" }
func (markedUsage) UsageError() bool { return true }

func TestClassifyHint(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	existing := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(existing, nil, 0o600); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}
	tests := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "net error",
			err:  &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED},
			want: IOHint,
		},
		{
			name: "missing file",
			err:  &fs.PathError{Op: "open", Path: "/a/nope", Err: syscall.ENOENT},
		},
		{
			name: "file without permission",
			err:  fmt.Errorf("reading the config: %w", &fs.PathError{Op: "open", Path: existing, Err: syscall.EACCES}),
			want: "check the permissions of " + existing + " and the directories above it",
		},
		{
			name: "file the directory doesn't allow to create",
			err:  fmt.Errorf("writing the config: %w", &fs.PathError{Op: "open", Path: filepath.Join(dir, "state", "pid"), Err: syscall.EACCES}),
			want: "check the permissions of " + filepath.Join(dir, "state") + " and the directories above it",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := Classify(tt.err).Hint; got != tt.want {
				t.Fatalf("Classify(%v).Hint = %q, want %q", tt.err, got, tt.want)
			}
		})
	}
}

func TestClassifyWrapped(t *testing.T) {
	t.Parallel()
	err := fmt.Errorf("coder: %w", NotRunning(errors.New("the agent server is not running")))

	got := Classify(err)

	want := &Error{Code: CodeNotRunning, Message: "coder: the agent server is not running", Exit: 6}
	if diff := cmp.Diff(want, got, cmpopts.IgnoreUnexported(Error{})); diff != "" {
		t.Fatalf("Classify() wrong result (-want +got) diff = %s", diff)
	}
}

func TestWrap(t *testing.T) {
	t.Parallel()
	cause := errors.New("can't reach http://127.0.0.1:1")
	tests := []struct {
		name string
		code string
		want *Error
	}{
		{name: "io", code: CodeIO, want: &Error{Code: CodeIO, Message: cause.Error(), Hint: IOHint, Exit: 3}},
		{name: "busy", code: CodeBusy, want: &Error{Code: CodeBusy, Message: cause.Error(), Exit: 8}},
		{name: "unknown code", code: "A2ASERVE_ERR_FUTURE", want: &Error{Code: "A2ASERVE_ERR_FUTURE", Message: cause.Error(), Exit: 1}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := Wrap(tc.code, cause)
			if diff := cmp.Diff(tc.want, got, cmpopts.IgnoreUnexported(Error{})); diff != "" {
				t.Fatalf("Wrap() wrong result (-want +got) diff = %s", diff)
			}
			if !errors.Is(got, cause) {
				t.Fatalf("Wrap() = %v, want it to wrap %v", got, cause)
			}
		})
	}
}

func TestClassifyNil(t *testing.T) {
	t.Parallel()
	if got := Classify(nil); got != nil {
		t.Fatalf("Classify(nil) = %v, want nil", got)
	}
}

func TestUsage(t *testing.T) {
	t.Parallel()
	got := Usage("bad flags")
	if got.Code != CodeUsage {
		t.Errorf("Usage().Code = %q, want %q", got.Code, CodeUsage)
	}
	if got.Exit != 2 {
		t.Errorf("Usage().Exit = %d, want 2", got.Exit)
	}
	if got.Hint != UsageHint {
		t.Errorf("Usage().Hint = %q, want %q", got.Hint, UsageHint)
	}
}

func TestClassifyTimeoutHint(t *testing.T) {
	t.Parallel()

	got := Classify(fmt.Errorf("waiting: %w", context.DeadlineExceeded))

	if want := "increase --timeout"; got.Hint != want {
		t.Fatalf("Classify(context.DeadlineExceeded).Hint = %q, want %q", got.Hint, want)
	}
}

func TestClassifyWordsHint(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		err      error
		wantHint string
	}{
		{name: "not running", err: agenterr.ErrNotRunning, wantHint: "start it with `a2a-serve start --detach`"},
		{name: "hint with an argument", err: agenterr.WithHint(agenterr.Conflictf("anna is already paired"), agenterr.HintPairRm, "anna"), wantHint: "pick another name, or run `a2a-serve pair rm anna` first"},
		{name: "usage error without a hint", err: agenterr.Invalidf("bad"), wantHint: UsageHint},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := Classify(tc.err).Hint; got != tc.wantHint {
				t.Fatalf("Classify(%v).Hint = %q, want %q", tc.err, got, tc.wantHint)
			}
		})
	}
}
