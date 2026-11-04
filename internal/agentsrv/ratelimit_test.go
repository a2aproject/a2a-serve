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
	"strconv"
	"testing"
	"time"
)

func TestFailureLimiter(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		failures int
		elapsed  time.Duration
		key      string
		want     bool
	}{
		{name: "below the limit", failures: 2, key: "a", want: true},
		{name: "at the limit", failures: 3, key: "a", want: false},
		{name: "other key", failures: 3, key: "b", want: true},
		{name: "next window", failures: 3, elapsed: time.Minute, key: "a", want: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			now := time.Unix(1000, 0)
			l := newFailureLimiter(3, time.Minute)
			l.now = func() time.Time { return now }
			for range tc.failures {
				l.fail("a")
			}
			now = now.Add(tc.elapsed)
			if got := l.allow(tc.key); got != tc.want {
				t.Fatalf("allow(%q) = %v, want %v", tc.key, got, tc.want)
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
		l.fail(strconv.Itoa(i))
		now = now.Add(time.Millisecond)
	}
	if got := len(l.hits); got > maxFailureKeys {
		t.Fatalf("len(hits) = %d, want at most %d", got, maxFailureKeys)
	}
	if l.allow(strconv.Itoa(maxFailureKeys + 9)) {
		t.Fatalf("allow(newest) = true, want false")
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
			for range tc.failures {
				b.fail()
			}
			if tc.success {
				b.reset()
			}
			now = now.Add(tc.elapsed)
			if got := b.wait(); got != tc.want {
				t.Fatalf("wait() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestBackoffForgetsFailuresAfterQuietPeriod(t *testing.T) {
	t.Parallel()
	now := time.Unix(1000, 0)
	b := newFailureBackoff(time.Second, 10*time.Second)
	b.now = func() time.Time { return now }
	for range 5 {
		b.fail()
	}
	now = now.Add(time.Hour)
	b.fail()
	if got, want := b.wait(), time.Second; got != want {
		t.Fatalf("wait() = %v, want %v", got, want)
	}
}
