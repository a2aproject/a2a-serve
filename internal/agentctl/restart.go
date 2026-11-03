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

package agentctl

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/controlapi"
	"github.com/a2aproject/a2a-serve/internal/fsutil"
	"github.com/a2aproject/a2a-serve/internal/launch"
	"github.com/a2aproject/a2a-serve/internal/service"
)

// stopTimeout is how long Restart waits for the server to exit.
const stopTimeout = 2 * time.Minute

// ErrForeground is returned by [Restart] for an agent which runs in the
// foreground of a terminal, which can't be restarted from another one.
var ErrForeground = errors.New("it runs in the foreground of another terminal; press Ctrl-C there and run `a2a serve run` again")

// Restart stops the running agent and starts it again in the same mode,
// detached or as a user service. The config is checked first, so that an
// agent which couldn't start again isn't stopped.
func Restart(ctx context.Context, d agentdir.Dir, quiet bool, out io.Writer) error {
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
	client := controlapi.NewClient(d)
	st, err := client.Status(ctx)
	if errors.Is(err, controlapi.ErrNotRunning) {
		return controlapi.NotRunning(cfg.Name)
	}
	if err != nil {
		return err
	}
	asService := service.Installed(launch.ServiceName(d, cfg))
	if !asService {
		leader, err := sessionLeader(st.PID)
		if err != nil {
			return err
		}
		if !leader {
			return fmt.Errorf("%s (pid %d) can't be restarted: %w", cfg.Name, st.PID, ErrForeground)
		}
	}
	if err := client.Stop(ctx); err != nil {
		return err
	}
	if err := waitStopped(ctx, d); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(out, "✓ stopped %s\n", cfg.Name); err != nil {
		return err
	}
	return launch.Background(ctx, d, asService, quiet, out)
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
