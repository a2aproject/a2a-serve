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

package httpserver

import (
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"github.com/a2aproject/a2a-serve/internal/jsonhttp"
)

// KeyedRate limits how often the requests of each key may come.
type KeyedRate struct {
	limit   rate.Limit
	burst   int
	maxKeys int
	key     func(*http.Request) string
	now     func() time.Time
	// idle is how long a key's bucket takes to refill. A key idle that long
	// has a full bucket, so forgetting it changes nothing.
	idle time.Duration

	mu        sync.Mutex
	keys      map[string]*rate.Limiter
	nextPrune time.Time
}

// NewKeyedRate returns a limiter of limit requests a second, with bursts
// of burst, for each key of a request. It keeps the buckets of at most
// maxKeys keys, and forgets a key once its bucket is full again.
func NewKeyedRate(limit rate.Limit, burst, maxKeys int, key func(*http.Request) string) *KeyedRate {
	return newKeyedRate(limit, burst, maxKeys, key, time.Now)
}

func newKeyedRate(limit rate.Limit, burst, maxKeys int, key func(*http.Request) string, now func() time.Time) *KeyedRate {
	return &KeyedRate{
		limit: limit, burst: burst, maxKeys: maxKeys, key: key, now: now,
		idle: time.Duration(float64(burst) / float64(limit) * float64(time.Second)),
		keys: map[string]*rate.Limiter{},
	}
}

// Handler serves next within the limits. It answers a request over its
// key's rate with 429, and the request of a new key while maxKeys keys
// are in use with 503. Both name the wait in Retry-After.
func (l *KeyedRate) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		code, wait := l.allow(l.key(r))
		if code == 0 {
			next.ServeHTTP(w, r)
			return
		}
		msg := BusyMessage
		if code == http.StatusTooManyRequests {
			msg = "too many requests from this client; try again later"
		}
		w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(wait.Seconds()))))
		w.Header().Set("Connection", "close")
		jsonhttp.Error(w, code, msg)
	})
}

// allow takes a request of key, or returns the status of its refusal and
// how long to wait.
func (l *KeyedRate) allow(key string) (int, time.Duration) {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	if !now.Before(l.nextPrune) {
		l.prune(now)
	}
	lim, ok := l.keys[key]
	if !ok {
		if len(l.keys) >= l.maxKeys {
			return http.StatusServiceUnavailable, max(time.Second, l.nextPrune.Sub(now))
		}
		lim = rate.NewLimiter(l.limit, l.burst)
		l.keys[key] = lim
	}
	if lim.AllowN(now, 1) {
		return 0, 0
	}
	wait := time.Duration((1 - lim.TokensAt(now)) / float64(l.limit) * float64(time.Second))
	return http.StatusTooManyRequests, max(time.Second, wait)
}

// prune forgets the keys whose buckets are full. It runs at most once in
// the time a bucket takes to refill. l.mu is held.
func (l *KeyedRate) prune(now time.Time) {
	for k, lim := range l.keys {
		if lim.TokensAt(now) >= float64(l.burst) {
			delete(l.keys, k)
		}
	}
	l.nextPrune = now.Add(l.idle)
}
