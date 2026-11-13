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

package harness

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"

	"github.com/a2aproject/a2a-serve/internal/sandbox"
)

// boundaryFallback starts the agent's container with the egress boundary
// until a start finds that the boundary can't work on this host, and with
// Docker's own network from then on. Other failures fail the start, which
// the server retries as it restarts any agent process.
type boundaryFallback struct {
	boundary, open sandbox.Sandbox
	stderr         io.Writer

	mu     sync.Mutex
	reason string
}

func (f *boundaryFallback) Start(ctx context.Context) (io.ReadWriteCloser, error) {
	f.mu.Lock()
	fellBack := f.reason != ""
	f.mu.Unlock()
	if !fellBack {
		rwc, err := f.boundary.Start(ctx)
		if !errors.Is(err, sandbox.ErrNoBoundary) {
			return rwc, err
		}
		f.mu.Lock()
		f.reason = err.Error()
		f.mu.Unlock()
		if f.stderr != nil {
			if _, werr := fmt.Fprintf(f.stderr, "a2a-serve: egress control is off, as the container can't set up its network: %s\n", err); werr != nil {
				slog.Warn("writing to the log of the agent", "error", werr)
			}
		}
	}
	return f.open.Start(ctx)
}

func (f *boundaryFallback) Close(ctx context.Context) error {
	return errors.Join(f.boundary.Close(ctx), f.open.Close(ctx))
}

// problem returns why the container has Docker's own network, or "".
func (f *boundaryFallback) problem() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.reason == "" {
		return ""
	}
	return noBoundary("the agent's container can't set up its network: " + f.reason)
}

// withClose returns sb, whose Close also calls closeFn.
func withClose(sb sandbox.Sandbox, closeFn func() error) sandbox.Sandbox {
	return closer{Sandbox: sb, closeFn: closeFn}
}

type closer struct {
	sandbox.Sandbox
	closeFn func() error
}

func (c closer) Close(ctx context.Context) error {
	return errors.Join(c.Sandbox.Close(ctx), c.closeFn())
}
