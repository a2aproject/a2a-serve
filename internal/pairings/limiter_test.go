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
	"strconv"
	"testing"
	"time"
)

func TestFailureLimiter(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		failures int
		refunds  int
		elapsed  time.Duration
		key      string
		want     time.Duration
	}{
		{name: "below the limit", failures: 2, key: "a", want: 0},
		{name: "at the limit", failures: 3, key: "a", want: time.Minute},
		{name: "partly waited", failures: 3, elapsed: 20 * time.Second, key: "a", want: 40 * time.Second},
		{name: "refunded", failures: 3, refunds: 1, key: "a", want: 0},
		{name: "other key", failures: 3, key: "b", want: 0},
		{name: "next window", failures: 3, elapsed: time.Minute, key: "a", want: 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			now := time.Unix(1000, 0)
			l := newFailureLimiter(3, time.Minute)
			l.now = func() time.Time { return now }
			for i := range tc.failures {
				_, refund := l.reserve("a")
				if i < tc.refunds {
					refund()
				}
			}
			now = now.Add(tc.elapsed)
			if got, _ := l.reserve(tc.key); got != tc.want {
				t.Fatalf("reserve(%q) = %v, want %v", tc.key, got, tc.want)
			}
		})
	}
}

func TestFailureLimiterKeepsAtMostMaxKeys(t *testing.T) {
	t.Parallel()
	now := time.Unix(1000, 0)
	l := newFailureLimiter(1, time.Minute)
	l.now = func() time.Time { return now }
	for i := range maxFailureKeys + 10 {
		l.reserve(strconv.Itoa(i))
		now = now.Add(time.Millisecond)
	}
	if got := len(l.hits); got > maxFailureKeys {
		t.Fatalf("len(hits) = %d, want at most %d", got, maxFailureKeys)
	}
	if wait, _ := l.reserve(strconv.Itoa(maxFailureKeys + 9)); wait == 0 {
		t.Fatalf("reserve(newest) = 0, want a wait")
	}
}

func TestBackoff(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		failures int
		elapsed  time.Duration
		success  bool
		want     time.Duration
	}{
		{name: "no failures", want: 0},
		{name: "one failure", failures: 1, want: time.Second},
		{name: "doubles", failures: 3, want: 4 * time.Second},
		{name: "capped", failures: 10, want: 10 * time.Second},
		{name: "waited out", failures: 3, elapsed: 4 * time.Second, want: 0},
		{name: "partly waited", failures: 3, elapsed: time.Second, want: 3 * time.Second},
		{name: "success resets", failures: 3, success: true, want: 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			now := time.Unix(1000, 0)
			b := newFailureBackoff(time.Second, 10*time.Second)
			b.now = func() time.Time { return now }
			failAfterWaiting(b, &now, tc.failures)
			if tc.success {
				b.reset()
			}
			now = now.Add(tc.elapsed)
			if got := b.reserve(); got != tc.want {
				t.Fatalf("reserve() = %v, want %v", got, tc.want)
			}
		})
	}
}

// failAfterWaiting records n failures of b, each after waiting out the
// backoff of the one before.
func failAfterWaiting(b *failureBackoff, now *time.Time, n int) {
	for n > 0 {
		if wait := b.reserve(); wait > 0 {
			*now = now.Add(wait)
			continue
		}
		n--
	}
}

func TestBackoffForgetsFailuresAfterQuietPeriod(t *testing.T) {
	t.Parallel()
	now := time.Unix(1000, 0)
	b := newFailureBackoff(time.Second, 10*time.Second)
	b.now = func() time.Time { return now }
	failAfterWaiting(b, &now, 5)
	now = now.Add(time.Hour)
	b.reserve()
	if got, want := b.reserve(), time.Second; got != want {
		t.Fatalf("reserve() = %v, want %v", got, want)
	}
}
