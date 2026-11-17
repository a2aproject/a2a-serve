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
	"os"
	"time"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/agenterr"
	"github.com/a2aproject/a2a-serve/internal/controlapi"
	"github.com/a2aproject/a2a-serve/internal/fsutil"
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
	// Services is the service manager.
	Services ServiceManager
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
	// ByService is whether the server was stopped through its user service.
	ByService bool
	// NewPID is the PID of a server that started while this one stopped.
	NewPID int
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
		res.Running, res.Name, pid = true, st.Name, st.PID
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
	res.ServiceInstalled = opts.Services.Installed(name)
	if res.ServiceInstalled && opts.Uninstall {
		if err := service.Uninstall(name); err != nil {
			return res, err
		}
		res.ServiceRemoved = true
	}
	managed := res.ServiceInstalled && !res.ServiceRemoved
	switch {
	case byPID:
		if managed {
			res.ByService = stopService(opts.Services, name, pid)
		}
		// A detached agent, or one the service manager didn't stop.
		if !res.ByService {
			if err := terminate(pid); err != nil {
				return res, fmt.Errorf("stopping the server (pid %d): %w", pid, err)
			}
		}
	case res.Running:
		if err := client.Stop(ctx); err != nil && !errors.Is(err, agenterr.ErrNotRunning) {
			return res, err
		}
	case managed:
		// The service manager may be about to restart a server that
		// failed.
		if err := opts.Services.Stop(name); err != nil {
			slog.Debug("stopping the user service", "name", name, "error", err)
		}
	}
	// A server that is shutting down removes its PID file before it
	// releases the lock.
	if opts.Wait == NoWait || !res.Running && !ServerRunning(dir) {
		return res, nil
	}
	kill := func() error { return nil }
	if res.Unresponsive {
		kill = func() error {
			slog.Debug("the server did not exit; killing it", "pid", pid)
			return killProcess(pid)
		}
	}
	res.NewPID, err = waitStopped(ctx, dir, pid, cmp.Or(opts.KillAfter, killAfter), kill)
	return res, err
}

// stopService stops the user service name through the service manager if
// it runs the server with pid, which a service manager would otherwise start
// again once the server is killed. It reports whether it did.
func stopService(services ServiceManager, name string, pid int) bool {
	mainPID, err := services.MainPID(name)
	if err != nil {
		slog.Debug("asking the service manager for the server's PID", "name", name, "error", err)
		return false
	}
	if mainPID != pid {
		return false
	}
	if err := services.Stop(name); err != nil {
		slog.Debug("stopping the user service", "name", name, "error", err)
		return false
	}
	return true
}

// Unanswered returns err, the error of a call to the server of dir, with
// a hint at `a2a-serve stop` when the call timed out while a server holds
// dir: that server is stuck, and stop ends it by its PID.
func Unanswered(dir agentdir.Dir, err error) error {
	if !errors.Is(err, context.DeadlineExceeded) || !ServerRunning(dir) {
		return err
	}
	return agenterr.WithHint(err, agenterr.HintStopUnanswered, "")
}

// waitStopped waits until the server with pid has exited, or with pid zero
// until no server holds the lock of the agent directory, which a server holds
// until everything is shut down. It returns the PID of a new server that the
// PID file names, which started meanwhile. kill runs once the server is still
// running after killAfter. A missing lock file means that the state directory
// is gone, and a server without it stops on its own.
func waitStopped(ctx context.Context, d agentdir.Dir, pid int, killAfter time.Duration, kill func() error) (int, error) {
	start := time.Now()
	killed := false
	for {
		if pid != 0 {
			if newPID := newServer(d, pid); newPID != 0 || !processExists(pid) {
				return newPID, nil
			}
		}
		release, err := d.Lock()
		if err == nil {
			return 0, release()
		}
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		if !errors.Is(err, fsutil.ErrLocked) {
			return 0, fmt.Errorf("checking that the server stopped: %w", err)
		}
		if !killed && time.Since(start) > killAfter {
			if err := kill(); err != nil {
				return 0, fmt.Errorf("killing the server: %w", err)
			}
			killed = true
		}
		if time.Since(start) > stopTimeout {
			return 0, fmt.Errorf("the server did not stop in time; see %s", d.LogPath())
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// newServer returns the PID of a server other than the one with pid that
// the PID file of d names, or zero.
func newServer(d agentdir.Dir, pid int) int {
	other, err := d.ServerPID()
	if err != nil || other == pid {
		return 0
	}
	return other
}
