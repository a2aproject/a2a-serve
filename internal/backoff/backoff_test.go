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
	"testing"
	"time"
)

func TestBackoffNext(t *testing.T) {
	t.Parallel()
	type step struct {
		uptime time.Duration
		// ceiling is the longest delay Next may return; the shortest is half.
		ceiling time.Duration
	}
	tests := []struct {
		name  string
		steps []step
	}{
		{
			name:  "doubles up to the maximum",
			steps: []step{{0, time.Second}, {0, 2 * time.Second}, {0, 4 * time.Second}, {0, 5 * time.Second}, {0, 5 * time.Second}},
		},
		{
			name:  "short runs keep growing the delay",
			steps: []step{{0, time.Second}, {time.Second, 2 * time.Second}, {9 * time.Second, 4 * time.Second}},
		},
		{
			name:  "a healthy run resets the delay",
			steps: []step{{0, time.Second}, {0, 2 * time.Second}, {0, 4 * time.Second}, {10 * time.Second, time.Second}, {0, 2 * time.Second}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b := New(time.Second, 5*time.Second, 10*time.Second)
			for i, s := range tc.steps {
				got := b.Next(s.uptime)
				if got < s.ceiling/2 || got > s.ceiling {
					t.Fatalf("Backoff.Next() #%d = %v, want between %v and %v", i, got, s.ceiling/2, s.ceiling)
				}
			}
		})
	}
}

func TestBackoffNextAddsJitter(t *testing.T) {
	t.Parallel()
	seen := map[time.Duration]bool{}
	for range 50 {
		seen[New(time.Minute, time.Hour, time.Hour).Next(0)] = true
	}
	if len(seen) < 2 {
		t.Fatalf("Backoff.Next() returned %d distinct delays in 50 runs, want jitter", len(seen))
	}
}
