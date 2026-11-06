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

package lifecycle

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/agenterr"
	"github.com/a2aproject/a2a-serve/internal/controlapi"
	"github.com/a2aproject/a2a-serve/internal/service"
)

// stopTimeout is how long [Stop] waits for the server to exit.
const stopTimeout = 2 * time.Minute

// probeTimeout bounds the status call with which [Stop] checks that the
// server answers.
const probeTimeout = 10 * time.Second

// killAfter is how long a server that doesn't answer gets to exit after
// SIGTERM before it is killed.
const killAfter = 10 * time.Second

// Wait is whether [Stop] waits for the server to exit.
type Wait bool

const (
	// NoWait returns once the server was asked to stop.
	NoWait Wait = false
	// WaitStopped returns once the server has exited.
	WaitStopped Wait = true
)

// StopOptions configure [Stop].
type StopOptions struct {
	Wait Wait
	// Uninstall also removes the user service.
	Uninstall bool
	// KillAfter is how long a server that doesn't answer gets to exit
	// after SIGTERM before it is killed. Zero means 10 seconds.
	KillAfter time.Duration
}

// StopResult is what [Stop] did.
type StopResult struct {
	// Name is the name the running server reported, if it answered.
	Name string
	// Running is whether the agent was running.
	Running bool
	// ServiceInstalled is whether a user service was installed.
	ServiceInstalled bool
	// ServiceRemoved is whether the user service was removed.
	ServiceRemoved bool
	// Unresponsive is whether the server didn't answer, so that it was
	// stopped by the PID in its PID file.
	Unresponsive bool
}

// Stop stops the agent of dir through client. With [WaitStopped] it returns
// once the server has shut everything down.
func Stop(ctx context.Context, dir agentdir.Dir, client *controlapi.Client, opts StopOptions) (StopResult, error) {
	var res StopResult
	pctx, cancel := context.WithTimeout(ctx, probeTimeout)
	st, err := client.Status(pctx)
	cancel()
	pid, byPID := 0, false
	switch {
	case err == nil:
		res.Running, res.Name = true, st.Name
	case errors.Is(err, agenterr.ErrNotRunning):
		pid, byPID = findStarting(dir)
		res.Running = byPID
	case ctx.Err() != nil:
		return res, err
	default:
		if pid, byPID = findStarting(dir); !byPID {
			return res, err
		}
		slog.Debug("the server did not answer; stopping it by its PID", "pid", pid, "error", err)
		res.Running, res.Unresponsive = true, true
	}
	name := dir.ServiceName()
	res.ServiceInstalled = service.Installed(name)
	if res.ServiceInstalled && opts.Uninstall {
		if err := service.Uninstall(name); err != nil {
			return res, err
		}
		res.ServiceRemoved = true
	}
	// A detached agent, or one the service manager didn't stop.
	if byPID {
		if err := terminate(pid); err != nil {
			return res, fmt.Errorf("stopping the server (pid %d): %w", pid, err)
		}
	} else if err := client.Stop(ctx); err != nil && !errors.Is(err, agenterr.ErrNotRunning) {
		return res, err
	}
	if !opts.Wait {
		return res, nil
	}
	kill := func() error { return nil }
	if res.Unresponsive {
		kill = func() error {
			slog.Debug("the server did not exit; killing it", "pid", pid)
			return killProcess(pid)
		}
	}
	return res, waitStopped(ctx, dir, cmp.Or(opts.KillAfter, killAfter), kill)
}

// waitStopped waits until the server releases the lock of the agent
// directory, which it holds until everything is shut down. kill runs once
// the server is still running after killAfter.
func waitStopped(ctx context.Context, d agentdir.Dir, killAfter time.Duration, kill func() error) error {
	start := time.Now()
	killed := false
	for {
		release, err := d.Lock()
		if err == nil {
			return release()
		}
		if !killed && time.Since(start) > killAfter {
			if err := kill(); err != nil {
				return fmt.Errorf("killing the server: %w", err)
			}
			killed = true
		}
		if time.Since(start) > stopTimeout {
			return fmt.Errorf("the server did not stop in time; see %s", d.LogPath())
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}
