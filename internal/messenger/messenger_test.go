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

package messenger

import (
	"errors"
	"io"
	"strings"
	"testing"
)

func TestTruncate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		in    string
		limit int
		want  string
	}{
		{name: "short", in: "abc", limit: 5, want: "abc"},
		{name: "ascii", in: "abcdef", limit: 3, want: "abc"},
		{name: "inside a character", in: "ab✓cd", limit: 4, want: "ab"},
		{name: "after a character", in: "ab✓cd", limit: 5, want: "ab✓"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := Truncate(tc.in, tc.limit); got != tc.want {
				t.Fatalf("Truncate(%q, %d) = %q, want %q", tc.in, tc.limit, got, tc.want)
			}
		})
	}
}

type failingReader struct{ err error }

func (r failingReader) Read([]byte) (int, error) { return 0, r.err }

func TestReadLimited(t *testing.T) {
	t.Parallel()
	readErr := errors.New("connection reset")
	tests := []struct {
		name       string
		r          io.Reader
		limit      int64
		want       string
		wantErr    error
		wantTooBig bool
	}{
		{name: "under the limit", r: strings.NewReader("abc"), limit: 5, want: "abc"},
		{name: "at the limit", r: strings.NewReader("abcde"), limit: 5, want: "abcde"},
		{name: "empty", r: strings.NewReader(""), limit: 5, want: ""},
		{name: "one byte over", r: strings.NewReader("abcdef"), limit: 5, wantTooBig: true},
		{name: "far over", r: strings.NewReader(strings.Repeat("x", 100)), limit: 5, wantTooBig: true},
		{name: "read error", r: failingReader{readErr}, limit: 5, wantErr: readErr},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := ReadLimited(tc.r, tc.limit)
			var tooLarge *TooLargeError
			if gotTooBig := errors.As(err, &tooLarge); gotTooBig != tc.wantTooBig {
				t.Fatalf("ReadLimited() error = %v, want TooLargeError = %v", err, tc.wantTooBig)
			}
			if tc.wantTooBig {
				if tooLarge.Limit != tc.limit {
					t.Fatalf("ReadLimited() limit = %d, want %d", tooLarge.Limit, tc.limit)
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("ReadLimited() error = %v, want %v", err, tc.wantErr)
			}
			if string(got) != tc.want {
				t.Fatalf("ReadLimited() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestTooLargeErrorNamesTheLimit(t *testing.T) {
	t.Parallel()
	tests := []struct {
		limit int64
		want  string
	}{
		{limit: 20 << 20, want: "over 20 MiB"},
		{limit: 1500, want: "over 1500 bytes"},
	}
	for _, tc := range tests {
		if got := (&TooLargeError{Limit: tc.limit}).Over(); got != tc.want {
			t.Fatalf("TooLargeError{%d}.Over() = %q, want %q", tc.limit, got, tc.want)
		}
	}
}
