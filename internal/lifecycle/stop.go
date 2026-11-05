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
	"time"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/controlapi"
	"github.com/a2aproject/a2a-serve/internal/fsutil"
	"github.com/a2aproject/a2a-serve/internal/service"
)

// stopTimeout is how long [Stop] waits for the server to exit.
const stopTimeout = 2 * time.Minute

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
}

// StopResult is what [Stop] did.
type StopResult struct {
	// Running is whether the agent was running.
	Running bool
	// ServiceInstalled is whether a user service was installed.
	ServiceInstalled bool
	// ServiceRemoved is whether the user service was removed.
	ServiceRemoved bool
}

// Stop stops the agent of dir through client. With [WaitStopped] it returns
// once the server has shut everything down.
func Stop(ctx context.Context, dir agentdir.Dir, client *controlapi.Client, opts StopOptions) (StopResult, error) {
	var res StopResult
	_, err := client.Status(ctx)
	if err != nil && !errors.Is(err, controlapi.ErrNotRunning) {
		return res, err
	}
	res.Running = err == nil
	pid, starting := 0, false
	if !res.Running {
		pid, starting = findStarting(dir)
		res.Running = starting
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
	if starting {
		if err := terminate(pid); err != nil {
			return res, fmt.Errorf("stopping the agent server (pid %d): %w", pid, err)
		}
	} else if err := client.Stop(ctx); err != nil && !errors.Is(err, controlapi.ErrNotRunning) {
		return res, err
	}
	if opts.Wait {
		return res, waitStopped(ctx, dir)
	}
	return res, nil
}

// waitStopped waits until the server releases the lock of the agent
// directory, which it holds until everything is shut down.
func waitStopped(ctx context.Context, d agentdir.Dir) error {
	deadline := time.Now().Add(stopTimeout)
	for {
		release, err := fsutil.TryLock(d.LockPath())
		if err == nil {
			return release()
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the agent server did not stop in time; see %s", d.LogPath())
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}
