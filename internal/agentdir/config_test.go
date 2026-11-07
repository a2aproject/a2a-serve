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

package agentdir

import (
	"testing"
	"time"
)

func TestFormatDuration(t *testing.T) {
	t.Parallel()
	tests := []struct {
		d    time.Duration
		want string
	}{
		{d: DefaultTaskTTL, want: "30d"},
		{d: DefaultBackupRetention, want: "7d"},
		{d: DefaultBackupInterval, want: "24h"},
		{d: DefaultContactRefresh, want: "1h"},
		{d: 90 * time.Minute, want: "90m"},
		{d: 36 * time.Hour, want: "36h"},
		{d: 1500 * time.Millisecond, want: "1.5s"},
	}
	for _, tc := range tests {
		t.Run(tc.want, func(t *testing.T) {
			t.Parallel()
			got := FormatDuration(tc.d)
			if got != tc.want {
				t.Fatalf("FormatDuration(%v) = %q, want %q", tc.d, got, tc.want)
			}
			if back, err := ParseDays(got); err != nil || back != tc.d {
				t.Fatalf("ParseDays(%q) = %v, %v, want %v", got, back, err, tc.d)
			}
		})
	}
}
