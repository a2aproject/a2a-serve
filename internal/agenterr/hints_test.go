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

package agenterr

import (
	"errors"
	"testing"
)

func TestText(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		want string
	}{
		{name: "nil", err: nil, want: ""},
		{name: "without a hint", err: errors.New("boom"), want: "boom"},
		{name: "with a hint", err: WithHint(Invalidf("anna has no chat on telegram"), HintLinkChat, "anna"), want: "anna has no chat on telegram: link it with a code from `a2a-serve pair invite anna`"},
		{name: "with a hint argument", err: WithHint(Conflictf("anna is already paired"), HintPairRm, "anna"), want: "anna is already paired: pick another name, or run `a2a-serve pair rm anna` first"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := Text(tc.err); got != tc.want {
				t.Fatalf("Text(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}
