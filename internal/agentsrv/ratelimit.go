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
	"net"
	"sync"
	"time"
)

// maxFailureKeys bounds the keys a failureLimiter tracks.
const maxFailureKeys = 1024

// failureLimiter allows at most max failures per key in each window, to
// slow down guessing of pairing codes. It tracks at most maxFailureKeys
// keys and forgets the oldest when full.
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

func newFailureLimiter(max int, window time.Duration) *failureLimiter {
	return &failureLimiter{max: max, window: window, now: time.Now, hits: map[string]*failureWindow{}}
}

// allow reports whether key may try again.
func (l *failureLimiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	w := l.hits[key]
	return w == nil || l.now().Sub(w.start) >= l.window || w.count < l.max
}

// fail records a failure of key.
func (l *failureLimiter) fail(key string) {
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
	w.count++
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

// failureBackoff delays the next attempt after each failure, doubling the delay
// from base up to max. Callers behind a tunnel share one address, so they
// share one backoff: it bounds the guessing rate while a caller with a valid
// code waits at most max.
type failureBackoff struct {
	base, max time.Duration
	now       func() time.Time

	mu       sync.Mutex
	failures int
	until    time.Time
}

func newFailureBackoff(base, max time.Duration) *failureBackoff {
	return &failureBackoff{base: base, max: max, now: time.Now}
}

// wait returns how long until the next attempt is allowed.
func (b *failureBackoff) wait() time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	return max(b.until.Sub(b.now()), 0)
}

// fail records a failure. Failures after a quiet period of max start over.
func (b *failureBackoff) fail() {
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

// reset forgets the failures after a success.
func (b *failureBackoff) reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failures = 0
	b.until = time.Time{}
}

// pairLimiter slows down guessing of pairing codes. Direct callers are
// limited by address. Callers through a tunnel arrive from the tunnel's
// address, so they share one backoff.
type pairLimiter struct {
	direct *failureLimiter
	tunnel *failureBackoff
}

func newPairLimiter() *pairLimiter {
	return &pairLimiter{direct: newFailureLimiter(10, time.Minute), tunnel: newFailureBackoff(time.Second, 10*time.Second)}
}

// wait returns how long the caller at host must wait before trying a code.
func (p *pairLimiter) wait(host string, tunneled bool) time.Duration {
	if tunneled {
		return p.tunnel.wait()
	}
	if p.direct.allow(host) {
		return 0
	}
	return p.direct.window
}

// fail records an invalid code.
func (p *pairLimiter) fail(host string, tunneled bool) {
	if tunneled {
		p.tunnel.fail()
		return
	}
	p.direct.fail(host)
}

// succeed records a valid code.
func (p *pairLimiter) succeed(tunneled bool) {
	if tunneled {
		p.tunnel.reset()
	}
}

// remoteHost is the host of a request's remote address.
func remoteHost(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return remoteAddr
	}
	return host
}
