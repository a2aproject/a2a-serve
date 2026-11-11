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
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/agenterr"
	"github.com/a2aproject/a2a-serve/internal/controlapi"
)

// ErrForeground is returned by [Restart] for an agent which runs in the
// foreground of a terminal, which can't be restarted from another one.
var ErrForeground = errors.New("it runs in the foreground of another terminal; press Ctrl-C there and run `a2a-serve start` again")

// ServiceManager tells whether the service manager runs an agent.
type ServiceManager interface {
	// Installed reports whether the user service name exists.
	Installed(name string) bool
	// MainPID returns the PID of the process that the service name runs,
	// or zero if it doesn't run.
	MainPID(name string) (int, error)
}

// Restart stops the running agent and starts it again in the same mode,
// detached or as a user service. A stopped agent is started as a user
// service if one is installed, and detached otherwise. The config is
// checked first, so that an agent which couldn't start again isn't stopped.
func Restart(ctx context.Context, d agentdir.Dir, client *controlapi.Client, services ServiceManager, quiet bool, out io.Writer) error {
	cfg, err := d.LoadConfig()
	if err != nil {
		return err
	}
	secrets, err := d.LoadSecrets()
	if err != nil {
		return err
	}
	if err := d.CheckConfig(cfg, secrets); err != nil {
		return &agentdir.SettingsError{Err: err}
	}
	pid, err := runningPID(ctx, d, client, cfg.Name)
	if errors.Is(err, agenterr.ErrNotRunning) {
		pid, err = 0, nil
	}
	if err != nil {
		return err
	}
	mode, err := restartMode(services, d.ServiceName(), pid)
	if errors.Is(err, ErrForeground) {
		return fmt.Errorf("%s (pid %d) can't be restarted: %w", cfg.Name, pid, err)
	}
	if err != nil {
		return err
	}
	if pid != 0 {
		if _, err := Stop(ctx, d, client, StopOptions{Wait: WaitStopped}); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(out, "✓ stopped %s\n", cfg.Name); err != nil {
			return err
		}
	}
	return background(ctx, d, services, mode == Service, quiet, out)
}

// runningPID returns the PID of the server, which may still be starting.
func runningPID(ctx context.Context, d agentdir.Dir, client *controlapi.Client, name string) (int, error) {
	st, err := client.Status(ctx)
	if err == nil {
		return st.PID, nil
	}
	if !errors.Is(err, agenterr.ErrNotRunning) {
		return 0, err
	}
	pid, starting := findStarting(d)
	if !starting {
		return 0, agenterr.NotRunning(name)
	}
	return pid, nil
}

// restartMode returns the mode in which to start again the agent whose
// server runs with pid, or zero if it is stopped. A server runs as a service
// only if it is the service's process: a service may stay installed while
// the agent runs in a terminal or detached. It returns ErrForeground for a
// server that runs in the foreground of a terminal.
func restartMode(services ServiceManager, name string, pid int) (Mode, error) {
	installed := services.Installed(name)
	if pid == 0 {
		if installed {
			return Service, nil
		}
		return Detached, nil
	}
	if installed {
		mainPID, err := services.MainPID(name)
		if err != nil {
			return 0, err
		}
		if mainPID == pid {
			return Service, nil
		}
	}
	leader, err := sessionLeader(pid)
	if err != nil {
		return 0, err
	}
	if !leader {
		return 0, ErrForeground
	}
	return Detached, nil
}
