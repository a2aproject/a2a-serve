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

package localsrv

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"iter"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-serve/internal/a2atext"
)

// Environment variables of the script, so it can keep state per task or
// conversation.
const (
	EnvTaskID    = "A2A_TASK_ID"
	EnvContextID = "A2A_CONTEXT_ID"
)

// ServeScript runs a local server whose agent runs a shell command for each
// message, streaming its output when s.Chunk or s.Stream is set.
func ServeScript(ctx context.Context, cfg Config, s ScriptOptions) error {
	if cfg.AgentName == "" {
		cfg.AgentName = "Script Agent"
	}
	if cfg.AgentDesc == "" {
		cfg.AgentDesc = fmt.Sprintf("Runs the command: %s", s.Command)
	}

	card, err := createAgentCard(cfg.CardParams)
	if err != nil {
		return err
	}

	executor := newExecExecutor(s)
	caps := &a2a.AgentCapabilities{Streaming: executor.streaming()}
	handler := a2asrv.NewHandler(executor, a2asrv.WithCapabilityChecks(caps))

	if !cfg.Quiet {
		cfg.Logger("script mode, command=%q chunk=%q stream=%t transport=%s protocol=%s", s.Command, s.Chunk, s.Stream, cfg.Transport, cfg.ProtocolVersion)
	}

	return serve(ctx, cfg, handler, card)
}

// waitDelay is how long a stopped command may keep its output open, through
// processes that left its group.
const waitDelay = 2 * time.Second

type execExecutor struct {
	command string
	chunk   string
	// keepDelim keeps the delimiter at the end of each chunk.
	keepDelim bool

	mu        sync.Mutex
	processes map[a2a.TaskID]*os.Process
}

func newExecExecutor(s ScriptOptions) *execExecutor {
	e := &execExecutor{
		command:   s.Command,
		chunk:     s.Chunk,
		processes: make(map[a2a.TaskID]*os.Process),
	}
	if s.Stream && s.Chunk == "" {
		e.chunk, e.keepDelim = "\n", true
	}
	return e
}

func (e *execExecutor) streaming() bool { return e.chunk != "" }

// newCommand returns the script process for the task of execCtx.
func (e *execExecutor) newCommand(ctx context.Context, execCtx *a2asrv.ExecutorContext, input string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "sh", "-c", e.command)
	cmd.Stdin = strings.NewReader(input)
	cmd.Env = append(os.Environ(), EnvTaskID+"="+string(execCtx.TaskID), EnvContextID+"="+execCtx.ContextID)
	inOwnGroup(cmd)
	cmd.WaitDelay = waitDelay
	return cmd
}

func (e *execExecutor) trackProcess(id a2a.TaskID, p *os.Process) {
	e.mu.Lock()
	e.processes[id] = p
	e.mu.Unlock()
}

func (e *execExecutor) untrackProcess(id a2a.TaskID) {
	e.mu.Lock()
	delete(e.processes, id)
	e.mu.Unlock()
}

func (e *execExecutor) killProcess(id a2a.TaskID) error {
	e.mu.Lock()
	p := e.processes[id]
	e.mu.Unlock()
	if p != nil {
		return killGroup(p)
	}
	return nil
}

func (e *execExecutor) Execute(ctx context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		if execCtx.StoredTask == nil {
			if !yield(a2a.NewSubmittedTask(execCtx, execCtx.Message), nil) {
				return
			}
		}
		if !yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateWorking, nil), nil) {
			return
		}
		input := a2atext.Message(execCtx.Message)

		if !e.streaming() {
			e.executeBuffered(ctx, execCtx, input, yield)
		} else {
			e.executeChunked(ctx, execCtx, input, yield)
		}
	}
}

func (e *execExecutor) executeBuffered(ctx context.Context, execCtx *a2asrv.ExecutorContext, input string, yield func(a2a.Event, error) bool) {
	cmd := e.newCommand(ctx, execCtx, input)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateFailed,
			a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart(err.Error()))), nil)
		return
	}
	e.trackProcess(execCtx.TaskID, cmd.Process)
	defer e.untrackProcess(execCtx.TaskID)

	if err := cmd.Wait(); err != nil {
		msg := stderr.String()
		if msg == "" {
			msg = err.Error()
		}
		yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateFailed,
			a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart(msg))), nil)
		return
	}

	evt := a2a.NewArtifactEvent(execCtx, a2a.NewTextPart(stdout.String()))
	evt.LastChunk = true
	if !yield(evt, nil) {
		return
	}
	yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateCompleted, nil), nil)
}

func (e *execExecutor) executeChunked(ctx context.Context, execCtx *a2asrv.ExecutorContext, input string, yield func(a2a.Event, error) bool) {
	cmd := e.newCommand(ctx, execCtx, input)
	// Background children of the shell can keep stderr open after it exits.
	cmd.WaitDelay = 5 * time.Second

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		yield(nil, fmt.Errorf("creating stdout pipe: %w", err))
		return
	}
	if err := cmd.Start(); err != nil {
		yield(nil, fmt.Errorf("starting command: %w", err))
		return
	}
	e.trackProcess(execCtx.TaskID, cmd.Process)
	defer e.untrackProcess(execCtx.TaskID)
	waited := false
	defer func() {
		if !waited {
			stopProcess(cmd, stdout)
		}
	}()

	scanner := bufio.NewScanner(stdout)
	const maxScanCapacity = 10 * 1024 * 1024 // 10 MB
	scanner.Buffer(make([]byte, 64*1024), maxScanCapacity)
	scanner.Split(splitByDelimiter(e.chunk, e.keepDelim))

	// Buffer one chunk ahead so we can set LastChunk on the final one.
	var pending *string
	var artifactID a2a.ArtifactID

	for scanner.Scan() {
		text := scanner.Text()
		if pending != nil {
			if artifactID == "" {
				evt := a2a.NewArtifactEvent(execCtx, a2a.NewTextPart(*pending))
				artifactID = evt.Artifact.ID
				if !yield(evt, nil) {
					return
				}
			} else {
				evt := a2a.NewArtifactUpdateEvent(execCtx, artifactID, a2a.NewTextPart(*pending))
				evt.Append = true
				if !yield(evt, nil) {
					return
				}
			}
		}
		pending = &text
	}
	if err := scanner.Err(); err != nil {
		yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateFailed,
			a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart(fmt.Sprintf("reading output: %v", err)))), nil)
		return
	}

	// Emit the final chunk with LastChunk=true.
	if pending != nil {
		if artifactID == "" {
			evt := a2a.NewArtifactEvent(execCtx, a2a.NewTextPart(*pending))
			evt.LastChunk = true
			if !yield(evt, nil) {
				return
			}
		} else {
			evt := a2a.NewArtifactUpdateEvent(execCtx, artifactID, a2a.NewTextPart(*pending))
			evt.Append = true
			evt.LastChunk = true
			if !yield(evt, nil) {
				return
			}
		}
	}

	waited = true
	if err := cmd.Wait(); err != nil {
		msg := stderr.String()
		if msg == "" {
			msg = err.Error()
		}
		statusMsg := a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart(msg))
		yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateFailed, statusMsg), nil)
		return
	}
	yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateCompleted, nil), nil)
}

// stopProcess kills a command whose output is no longer read, and the
// processes it started, and reaps it.
func stopProcess(cmd *exec.Cmd, stdout io.Closer) {
	if err := killGroup(cmd.Process); err != nil && !errors.Is(err, os.ErrProcessDone) {
		slog.Debug("killing exec command", "error", err)
	}
	if err := stdout.Close(); err != nil {
		slog.Debug("closing exec command output", "error", err)
	}
	if err := cmd.Wait(); err != nil {
		slog.Debug("exec command stopped", "error", err)
	}
}

func (e *execExecutor) Cancel(_ context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		if err := e.killProcess(execCtx.TaskID); err != nil {
			yield(nil, err)
			return
		}
		yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateCanceled, nil), nil)
	}
}

func splitByDelimiter(delim string, keep bool) bufio.SplitFunc {
	return func(data []byte, atEOF bool) (advance int, token []byte, err error) {
		if atEOF && len(data) == 0 {
			return 0, nil, nil
		}
		if i := bytes.Index(data, []byte(delim)); i >= 0 {
			end := i
			if keep {
				end += len(delim)
			}
			return i + len(delim), data[:end], nil
		}
		if atEOF {
			return len(data), data, nil
		}
		return 0, nil, nil
	}
}
