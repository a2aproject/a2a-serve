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

//go:build unix

package localsrv

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
)

func TestExecExecutor_ChunkedStopsTheCommandWhenTheConsumerLeaves(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		output string
	}{
		{name: "consumer gone", output: `printf 'a\nb\nc\n'`},
		{name: "chunk too long", output: `head -c 11000000 /dev/zero`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			pidFile := filepath.Join(t.TempDir(), "pid")
			e := newExecExecutor(ScriptOptions{Command: fmt.Sprintf("echo $$ > %s; %s; exec sleep 30", pidFile, tc.output), Chunk: "\n"})
			msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("go"))
			execCtx := &a2asrv.ExecutorContext{Message: msg, TaskID: a2a.NewTaskID(), ContextID: "ctx"}
			for ev, err := range e.Execute(t.Context(), execCtx) {
				if err != nil {
					t.Fatalf("Execute() error = %v", err)
				}
				if _, ok := ev.(*a2a.TaskArtifactUpdateEvent); ok {
					break
				}
				if s, ok := ev.(*a2a.TaskStatusUpdateEvent); ok && s.Status.State.Terminal() {
					break
				}
			}
			data, err := os.ReadFile(pidFile)
			if err != nil {
				t.Fatalf("reading the pid file error = %v", err)
			}
			pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
			if err != nil {
				t.Fatalf("strconv.Atoi() error = %v", err)
			}
			if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
				t.Fatalf("command process %d after Execute() returned: kill(0) error = %v, want %v", pid, err, syscall.ESRCH)
			}
		})
	}
}

func TestExecExecutor_CancelStopsTheCommandsChildren(t *testing.T) {
	t.Parallel()
	pidFile := filepath.Join(t.TempDir(), "pid")
	e := newExecExecutor(ScriptOptions{Command: fmt.Sprintf("sleep 30 & echo $! > %s; wait", pidFile)})
	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("go"))
	execCtx := &a2asrv.ExecutorContext{Message: msg, TaskID: a2a.NewTaskID(), ContextID: "ctx"}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range e.Execute(ctx, execCtx) {
		}
	}()
	var pid int
	for deadline := time.Now().Add(10 * time.Second); pid == 0; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the command didn't start its child")
		}
		data, err := os.ReadFile(pidFile)
		if err != nil || !strings.HasSuffix(string(data), "\n") {
			continue
		}
		if pid, err = strconv.Atoi(strings.TrimSpace(string(data))); err != nil {
			t.Fatalf("strconv.Atoi() error = %v", err)
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Execute() didn't return after its context was canceled")
	}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("child process %d still runs after Execute() returned", pid)
		}
	}
}

func TestExecExecutor_ChunkedStopKillsTheCommandsChildren(t *testing.T) {
	t.Parallel()
	pidFile := filepath.Join(t.TempDir(), "pid")
	e := newExecExecutor(ScriptOptions{Command: fmt.Sprintf("sleep 30 & echo $! > %s; printf 'a\\nb\\nc\\n'; wait", pidFile), Chunk: "\n"})
	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("go"))
	execCtx := &a2asrv.ExecutorContext{Message: msg, TaskID: a2a.NewTaskID(), ContextID: "ctx"}
	for ev, err := range e.Execute(t.Context(), execCtx) {
		if err != nil {
			t.Fatalf("Execute() error = %v", err)
		}
		if _, ok := ev.(*a2a.TaskArtifactUpdateEvent); ok {
			break
		}
	}
	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("reading the pid file error = %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatalf("strconv.Atoi() error = %v", err)
	}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("child process %d still runs after Execute() returned", pid)
		}
	}
}
