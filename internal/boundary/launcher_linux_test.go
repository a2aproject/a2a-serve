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

package boundary

import (
	"strings"
	"testing"
)

func TestCheckCapabilitySets(t *testing.T) {
	t.Parallel()
	const empty = "CapInh:\t0000000000000000\nCapPrm:\t0000000000000000\nCapEff:\t0000000000000000\nCapBnd:\t0000000000000000\n"
	tests := []struct {
		name    string
		status  string
		wantErr string
	}{
		{name: "every set empty", status: "Name:\tagent\n" + empty + "CapAmb:\t0000000000000000\nSeccomp:\t2\n"},
		{name: "no ambient set", status: empty},
		{name: "bounding set left", status: strings.Replace(empty, "CapBnd:\t0000000000000000", "CapBnd:\t000001ffffffffff", 1), wantErr: "CapBnd is still 000001ffffffffff"},
		{name: "ambient set left", status: empty + "CapAmb:\t0000000000000400\n", wantErr: "CapAmb is still 0000000000000400"},
		{name: "bounding set missing", status: strings.Replace(empty, "CapBnd:\t0000000000000000\n", "", 1), wantErr: "doesn't list CapBnd"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := checkCapabilitySets(tc.status)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("checkCapabilitySets() error = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("checkCapabilitySets() error = %v, want %q", err, tc.wantErr)
			}
		})
	}
}
