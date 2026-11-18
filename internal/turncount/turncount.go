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

// Package turncount counts the turns in progress, so that a stop can wait
// for them.
package turncount

import (
	"context"
	"sync"
)

// Count counts the turns in progress, and lets callers wait until none
// is. The zero value is ready to use.
type Count struct {
	mu sync.Mutex
	n  int
	// idle is closed when n drops to zero, for those who wait for it.
	idle chan struct{}
	// held are the keys of the turns that Hold added, until Release.
	held map[any]struct{}
}

// Hold counts a turn under key until Release is called with it.
func (c *Count) Hold(key any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.held[key]; ok {
		return
	}
	if c.held == nil {
		c.held = make(map[any]struct{})
	}
	c.held[key] = struct{}{}
	c.n++
}

// Release ends the turn that Hold counted under key, if there is one.
func (c *Count) Release(key any) {
	c.mu.Lock()
	_, ok := c.held[key]
	delete(c.held, key)
	c.mu.Unlock()
	if ok {
		c.Done()
	}
}

// Add counts a turn until Done.
func (c *Count) Add() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n++
}

// Done ends a turn that Add counted.
func (c *Count) Done() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n--
	if c.n == 0 && c.idle != nil {
		close(c.idle)
		c.idle = nil
	}
}

// Wait waits until no turn is in progress, or until ctx is done.
func (c *Count) Wait(ctx context.Context) {
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
