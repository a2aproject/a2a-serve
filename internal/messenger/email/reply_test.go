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

package email

import (
	"strings"
	"testing"
)

func TestHTMLTextCapsQuoteDepth(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "deeper quote keeps its text",
			in:   strings.Repeat("<blockquote>", maxQuoteDepth+5) + "deep" + strings.Repeat("</blockquote>", maxQuoteDepth+5) + "top",
			want: strings.Repeat("> ", maxQuoteDepth) + "deep\n\ntop",
		},
		{
			name: "deeper rule",
			in:   strings.Repeat("<blockquote>", maxQuoteDepth+1) + "<hr>",
			want: strings.Repeat("> ", maxQuoteDepth) + "__________",
		},
		{
			name: "depth at the cap",
			in:   strings.Repeat("<blockquote>", maxQuoteDepth) + "a",
			want: strings.Repeat("> ", maxQuoteDepth) + "a",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := htmlText(strings.NewReader(tt.in))
			if err != nil {
				t.Fatalf("htmlText() error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("htmlText() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestHTMLTextGrowsLinearly(t *testing.T) {
	t.Parallel()
	in := strings.Repeat("<blockquote>a", 2_000) + strings.Repeat("<hr>", 20_000)
	got, err := htmlText(strings.NewReader(in))
	if err != nil {
		t.Fatalf("htmlText() error = %v", err)
	}
	if limit := 2 * maxQuoteDepth * len(in); len(got) > limit {
		t.Fatalf("len(htmlText()) = %d for %d bytes of HTML, want at most %d", len(got), len(in), limit)
	}
}
