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

package backoff

import (
	"sync"
	"time"
)

// Failures delays the next attempt after each failure, doubling the delay
// from base up to max. Callers behind a tunnel share one address, so they
// share one Failures: it bounds the guessing rate while a caller with a
// valid secret waits at most max.
type Failures struct {
	base, max time.Duration
	now       func() time.Time

	mu       sync.Mutex
	failures int
	until    time.Time
}

// NewFailures returns a backoff from base to max on the clock now.
func NewFailures(base, max time.Duration, now func() time.Time) *Failures {
	return &Failures{base: base, max: max, now: now}
}

// Wait returns how long until the next attempt is allowed.
func (b *Failures) Wait() time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	return max(b.until.Sub(b.now()), 0)
}

// Fail records a failure. Failures after a quiet period of max start over.
func (b *Failures) Fail() {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	if now.Sub(b.until) >= b.max {
		b.failures = 0
	}
	delay := b.base
	for i := 0; i < b.failures && delay < b.max; i++ {
		delay *= 2
	}
	b.failures++
	b.until = now.Add(min(delay, b.max))
}

// Reset forgets the failures after a success.
func (b *Failures) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failures = 0
	b.until = time.Time{}
}
