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
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func newRateHandler(burst, maxKeys int, clock *fakeClock) http.Handler {
	l := newKeyedRate(1, burst, maxKeys, func(r *http.Request) string { return r.Header.Get("X-Key") }, clock.Now)
	return l.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
}

func serveKey(h http.Handler, key string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("X-Key", key)
	h.ServeHTTP(w, r)
	return w
}

func TestKeyedRate(t *testing.T) {
	t.Parallel()
	clock := &fakeClock{now: time.Unix(1000, 0)}
	h := newRateHandler(3, 10, clock)
	for i := range 3 {
		if got := serveKey(h, "a").Code; got != http.StatusOK {
			t.Fatalf("KeyedRate.Handler() request %d of a key status = %d, want %d", i+1, got, http.StatusOK)
		}
	}

	over := serveKey(h, "a")
	other := serveKey(h, "b")
	clock.Advance(time.Second)
	refilled := serveKey(h, "a")

	if over.Code != http.StatusTooManyRequests || over.Header().Get("Retry-After") != "1" {
		t.Fatalf("KeyedRate.Handler() request over the key's rate = %d with Retry-After %q, want %d with Retry-After 1", over.Code, over.Header().Get("Retry-After"), http.StatusTooManyRequests)
	}
	if got := over.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("KeyedRate.Handler() refused request Content-Type = %q, want %q", got, "application/json")
	}
	if other.Code != http.StatusOK {
		t.Fatalf("KeyedRate.Handler() request of another key status = %d, want %d", other.Code, http.StatusOK)
	}
	if refilled.Code != http.StatusOK {
		t.Fatalf("KeyedRate.Handler() request a second after the refusal status = %d, want %d", refilled.Code, http.StatusOK)
	}
}

func TestKeyedRateAdmitsUntrackedKeysWhenFull(t *testing.T) {
	t.Parallel()
	clock := &fakeClock{now: time.Unix(1000, 0)}
	h := newRateHandler(2, 1, clock)
	if got := serveKey(h, "a").Code; got != http.StatusOK {
		t.Fatalf("KeyedRate.Handler() first key status = %d, want %d", got, http.StatusOK)
	}

	for i := range 3 {
		if got := serveKey(h, "b").Code; got != http.StatusOK {
			t.Fatalf("KeyedRate.Handler() request %d of a key over the cap status = %d, want %d", i+1, got, http.StatusOK)
		}
	}
	clock.Advance(4 * time.Second)
	var codes []int
	for range 3 {
		codes = append(codes, serveKey(h, "b").Code)
	}

	want := []int{http.StatusOK, http.StatusOK, http.StatusTooManyRequests}
	if diff := cmp.Diff(want, codes); diff != "" {
		t.Fatalf("KeyedRate.Handler() key once the first is forgotten wrong statuses (-want +got) diff = %s", diff)
	}
}
