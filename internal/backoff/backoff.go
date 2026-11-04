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

// Package backoff computes the delays between reconnection attempts.
package backoff

import (
	"math/rand/v2"
	"time"
)

// Backoff doubles the delay after each failure up to a maximum and starts
// over after a connection that stayed up for long enough.
type Backoff struct {
	min, max, healthy time.Duration
	cur               time.Duration
}

// New returns a Backoff that starts at first, doubles up to last, and resets
// after a run of at least healthy.
func New(first, last, healthy time.Duration) *Backoff {
	return &Backoff{min: first, max: last, healthy: healthy, cur: first}
}

// Next returns how long to wait before reconnecting after a connection that
// stayed up for uptime. The delay is jittered between half and the full
// current delay, so that clients that failed together don't retry together.
func (b *Backoff) Next(uptime time.Duration) time.Duration {
	if uptime >= b.healthy {
		b.cur = b.min
	}
	d := b.cur
	b.cur = min(b.cur*2, b.max)
	return d/2 + rand.N(d/2+1)
}
