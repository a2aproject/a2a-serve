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

package agentsrv

import (
	"context"
	"sync"
)

// turnCount counts the turns in progress, and lets callers wait until none
// is.
type turnCount struct {
	mu sync.Mutex
	n  int
	// idle is closed when n drops to zero, for those who wait for it.
	idle chan struct{}
}

func (c *turnCount) add() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n++
}

func (c *turnCount) done() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n--
	if c.n == 0 && c.idle != nil {
		close(c.idle)
		c.idle = nil
	}
}

// wait waits until no turn is in progress, or until ctx is done.
func (c *turnCount) wait(ctx context.Context) {
	c.mu.Lock()
	if c.n == 0 {
		c.mu.Unlock()
		return
	}
	if c.idle == nil {
		c.idle = make(chan struct{})
	}
	idle := c.idle
	c.mu.Unlock()
	select {
	case <-idle:
	case <-ctx.Done():
	}
}
