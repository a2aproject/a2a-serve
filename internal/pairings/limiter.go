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

package pairings

import (
	"sync"
	"time"
)

// Limits on failed attempts to redeem a code.
const (
	// maxFailures and failurePeriod limit the failures of one client.
	maxFailures   = 10
	failurePeriod = time.Minute
	// backoffBase and backoffMax bound the backoff of clients that can't
	// be told apart.
	backoffBase = time.Second
	backoffMax  = 10 * time.Second
)

// Limiter slows down guessing of pairing codes. Each client may fail
// maxFailures times in a window. Clients that can't be told apart, such
// as anonymous callers through a relay, share one backoff: it bounds their
// guessing rate while a caller with a valid code waits at most backoffMax.
//
// An attempt counts as a failure from its start, so concurrent attempts
// can't exceed the limit, and a successful one is refunded.
type Limiter struct {
	clients *failureLimiter
	shared  *failureBackoff
}

// NewLimiter returns a limiter with no failures recorded on the clock now.
func NewLimiter(now func() time.Time) *Limiter {
	return &Limiter{clients: newFailureLimiter(maxFailures, failurePeriod, now), shared: newFailureBackoff(backoffBase, backoffMax, now)}
}

// attempt starts an attempt of client, or returns how long the client
// must wait first. An empty client shares the backoff. done ends the
// attempt and refunds it if it succeeded.
func (l *Limiter) attempt(client string) (wait time.Duration, done func(ok bool)) {
	if client == "" {
		if wait := l.shared.reserve(); wait > 0 {
			return wait, nil
		}
		return 0, func(ok bool) {
			if ok {
				l.shared.reset()
			}
		}
	}
	wait, refund := l.clients.reserve(client)
	if wait > 0 {
		return wait, nil
	}
	return 0, func(ok bool) {
		if ok {
			refund()
		}
	}
}

// maxFailureKeys bounds the keys a failureLimiter tracks.
const maxFailureKeys = 1024

// failureLimiter allows at most max failures per key in each window. It
// tracks at most maxFailureKeys keys and forgets the oldest when full.
type failureLimiter struct {
	max    int
	window time.Duration
	now    func() time.Time

	mu   sync.Mutex
	hits map[string]*failureWindow
}

type failureWindow struct {
	start time.Time
	count int
}

func newFailureLimiter(max int, window time.Duration, now func() time.Time) *failureLimiter {
	return &failureLimiter{max: max, window: window, now: now, hits: map[string]*failureWindow{}}
}

// reserve records a failure of key and returns the function that takes
// it back, or returns how long until key may fail again.
func (l *failureLimiter) reserve(key string) (time.Duration, func()) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	w := l.hits[key]
	if w == nil || now.Sub(w.start) >= l.window {
		if w == nil && len(l.hits) >= maxFailureKeys {
			l.evict(now)
		}
		w = &failureWindow{start: now}
		l.hits[key] = w
	}
	if w.count >= l.max {
		return w.start.Add(l.window).Sub(now), nil
	}
	w.count++
	return 0, func() { l.refund(key, w) }
}

// refund takes back a failure of key recorded in the window w.
func (l *failureLimiter) refund(key string, w *failureWindow) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.hits[key] == w && w.count > 0 {
		w.count--
	}
}

// evict drops the expired keys, or the oldest key if none expired.
func (l *failureLimiter) evict(now time.Time) {
	oldest := ""
	for k, v := range l.hits {
		if now.Sub(v.start) >= l.window {
			delete(l.hits, k)
			continue
		}
		if oldest == "" || v.start.Before(l.hits[oldest].start) {
			oldest = k
		}
	}
	if len(l.hits) >= maxFailureKeys {
		delete(l.hits, oldest)
	}
}

// failureBackoff delays the next attempt after each failure, doubling the
// delay from base up to max.
type failureBackoff struct {
	base, max time.Duration
	now       func() time.Time

	mu       sync.Mutex
	failures int
	until    time.Time
}

func newFailureBackoff(base, max time.Duration, now func() time.Time) *failureBackoff {
	return &failureBackoff{base: base, max: max, now: now}
}

// reserve records a failure, or returns how long until the next attempt is
// allowed. Failures after a quiet period of max start over.
func (b *failureBackoff) reserve() time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	if wait := b.until.Sub(now); wait > 0 {
		return wait
	}
	if now.Sub(b.until) >= b.max {
		b.failures = 0
	}
	delay := b.base
	for i := 0; i < b.failures && delay < b.max; i++ {
		delay *= 2
	}
	b.failures++
	b.until = now.Add(min(delay, b.max))
	return 0
}

// reset forgets the failures after a success.
func (b *failureBackoff) reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failures = 0
	b.until = time.Time{}
}
