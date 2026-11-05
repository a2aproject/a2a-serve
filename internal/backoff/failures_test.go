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
			b := NewFailures(time.Second, 10*time.Second, func() time.Time { return now })
			for range tc.failures {
				b.Fail()
			}
			if tc.success {
				b.Reset()
			}
			now = now.Add(tc.elapsed)
			if got := b.Wait(); got != tc.want {
				t.Fatalf("Wait() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestBackoffForgetsFailuresAfterQuietPeriod(t *testing.T) {
	t.Parallel()
	now := time.Unix(1000, 0)
	b := NewFailures(time.Second, 10*time.Second, func() time.Time { return now })
	for range 5 {
		b.Fail()
	}
	now = now.Add(time.Hour)
	b.Fail()
	if got, want := b.Wait(), time.Second; got != want {
		t.Fatalf("Wait() = %v, want %v", got, want)
	}
}
