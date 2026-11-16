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

package sandbox

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"time"
)

// startProcess starts cmd and returns its stdio: reads come from its
// stdout, writes go to its stdin. Reads report [io.EOF] once the process
// exits. Close closes stdin, which ACP agents treat as a request to exit,
// and terminates the process group if the process doesn't exit in time.
func startProcess(cmd *exec.Cmd, stderr io.Writer) (io.ReadWriteCloser, error) {
	cmd.Stderr = stderr
	cmd.WaitDelay = 5 * time.Second
	configureProcessGroup(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	// Wait closes pipes made by StdoutPipe as soon as the process exits,
	// which could drop its last messages, so stdout is a pipe we own.
	stdout, stdoutW, err := os.Pipe()
	if err != nil {
		return nil, errors.Join(err, stdin.Close())
	}
	cmd.Stdout = stdoutW
	startErr := cmd.Start()
	if err := stdoutW.Close(); err != nil && startErr == nil {
		startErr = err
	}
	if startErr != nil {
		return nil, errors.Join(fmt.Errorf("starting %s: %w", cmd.Args[0], startErr), stdout.Close())
	}
	p := &process{cmd: cmd, stdin: stdin, stdout: stdout, exited: make(chan struct{})}
	go func() {
		p.waitErr = cmd.Wait()
		close(p.exited)
		// A child process which inherited stdout would keep it open after
		// the process exits and hide the exit from the reader.
		time.AfterFunc(outputGrace, func() {
			if err := p.closeStdout(); err != nil {
				slog.Warn("closing the output of the agent process", "error", err)
			}
		})
	}()
	return p, nil
}

const (
	outputGrace = 2 * time.Second
	closeGrace  = 2 * time.Second
)

type process struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	stdout  *os.File
	exited  chan struct{}
	waitErr error
}

// closeStdout closes the read end of stdout, tolerating a second close.
func (p *process) closeStdout() error {
	if err := p.stdout.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
		return err
	}
	return nil
}

// Read reads stdout. Once stdout was closed, after the process exited or by
// Close, reads report [io.EOF].
func (p *process) Read(b []byte) (int, error) {
	n, err := p.stdout.Read(b)
	if errors.Is(err, os.ErrClosed) {
		err = io.EOF
	}
	return n, err
}

func (p *process) Write(b []byte) (int, error) { return p.stdin.Write(b) }

// Close closes stdin, terminates the process group if the process doesn't
// exit in time and kills it if it doesn't exit after that either. Processes
// left in the group, such as MCP servers the agent started, are terminated.
func (p *process) Close() error {
	// Wait closes stdin once the process exits, which is fine here.
	stdinErr := p.stdin.Close()
	if errors.Is(stdinErr, os.ErrClosed) {
		stdinErr = nil
	}
	var signalErr error
	if !p.waitExit(closeGrace) {
		signalErr = terminateProcessGroup(p.cmd)
		if !p.waitExit(closeGrace) {
			if err := killProcessGroup(p.cmd); err != nil {
				return errors.Join(stdinErr, signalErr, err)
			}
			<-p.exited
		}
	}
	groupErr := terminateProcessGroup(p.cmd)
	stdoutErr := p.closeStdout()
	if _, exited := errors.AsType[*exec.ExitError](p.waitErr); p.waitErr != nil && !exited {
		return errors.Join(stdinErr, signalErr, groupErr, stdoutErr, p.waitErr)
	}
	return errors.Join(stdinErr, signalErr, groupErr, stdoutErr)
}

func (p *process) waitExit(timeout time.Duration) bool {
	select {
	case <-p.exited:
		return true
	case <-time.After(timeout):
		return false
	}
}
