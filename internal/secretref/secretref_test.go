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


package secretref

import (
	"bytes"
	"errors"
	"os"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/clierr"
)

func pipeWith(t *testing.T, text string) *os.File {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := r.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})
	if _, err := w.WriteString(text); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return r
}

func isUsage(err error) bool {
	var ce *clierr.Error
	return errors.As(clierr.Classify(err), &ce) && ce.Code == clierr.CodeUsage
}

// TestParse can't run in parallel because of t.Setenv.
func TestParse(t *testing.T) {
	t.Setenv("SECRETREF_TEST_TOKEN", "from-env")
	t.Setenv("SECRETREF_TEST_EMPTY", "")
	tests := []struct {
		name      string
		value     string
		stdin     string
		want      Ref
		wantUsage bool
	}{
		{name: "literal", value: "123:abc", want: Ref{Kind: Literal, Value: "123:abc"}},
		{name: "env", value: "env:SECRETREF_TEST_TOKEN", want: Ref{Kind: Env, Name: "SECRETREF_TEST_TOKEN", Value: "from-env"}},
		{name: "empty env", value: "env:SECRETREF_TEST_EMPTY", wantUsage: true},
		{name: "stdin", value: "-", stdin: "from-stdin\n", want: Ref{Kind: Prompted, Value: "from-stdin"}},
		{name: "empty stdin", value: "-", wantUsage: true},
		{name: "stored", value: "secret:TELEGRAM_TOKEN", want: Ref{Kind: Stored, Name: "TELEGRAM_TOKEN"}},
		{name: "stored without a name", value: "secret:", wantUsage: true},
		{name: "empty", value: "", wantUsage: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := NewReader(pipeWith(t, tc.stdin), &bytes.Buffer{})

			got, err := Parse("token", tc.value, r)

			if tc.wantUsage {
				if !isUsage(err) {
					t.Fatalf("Parse(%q) error = %v, want a usage error", tc.value, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Parse(%q) error = %v", tc.value, err)
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Fatalf("Parse(%q) wrong result (-want +got) diff = %s", tc.value, diff)
			}
		})
	}
}

func TestResolveRejectsStored(t *testing.T) {
	t.Parallel()
	r := NewReader(pipeWith(t, ""), &bytes.Buffer{})
	if _, err := Resolve("token", "secret:TOKEN", r); !isUsage(err) {
		t.Fatalf("Resolve(secret:TOKEN) error = %v, want a usage error", err)
	}
}

func TestReaderReadsStdinOnce(t *testing.T) {
	t.Parallel()
	r := NewReader(pipeWith(t, "abc"), &bytes.Buffer{})
	if _, err := Parse("telegram", "-", r); err != nil {
		t.Fatalf("Parse(-) error = %v", err)
	}
	if _, err := Parse("discord", "-", r); !isUsage(err) {
		t.Fatalf("Parse(-) reading stdin twice error = %v, want a usage error", err)
	}
}

// TestResolveHeader can't run in parallel because of t.Setenv.
func TestResolveHeader(t *testing.T) {
	t.Setenv("SECRETREF_TEST_TOKEN", "from-env")
	tests := []struct {
		name      string
		header    string
		stdin     string
		wantName  string
		wantValue string
		wantUsage bool
	}{
		{name: "literal", header: "X-Key: abc", wantName: "X-Key", wantValue: "abc"},
		{name: "env at the end", header: "Authorization: Bearer env:SECRETREF_TEST_TOKEN", wantName: "Authorization", wantValue: "Bearer from-env"},
		{name: "env only", header: "X-Key: env:SECRETREF_TEST_TOKEN", wantName: "X-Key", wantValue: "from-env"},
		{name: "stdin at the end", header: "Authorization: Bearer -", stdin: "from-stdin", wantName: "Authorization", wantValue: "Bearer from-stdin"},
		{name: "env not at the end", header: "X-Key: env:SECRETREF_TEST_TOKEN suffix", wantName: "X-Key", wantValue: "env:SECRETREF_TEST_TOKEN suffix"},
		{name: "no colon", header: "Authorization", wantUsage: true},
		{name: "no name", header: ": abc", wantUsage: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := NewReader(pipeWith(t, tc.stdin), &bytes.Buffer{})

			name, value, err := ResolveHeader("webhook-header", tc.header, r)

			if tc.wantUsage {
				if !isUsage(err) {
					t.Fatalf("ResolveHeader(%q) error = %v, want a usage error", tc.header, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ResolveHeader(%q) error = %v", tc.header, err)
			}
			if name != tc.wantName || value != tc.wantValue {
				t.Fatalf("ResolveHeader(%q) = %q, %q, want %q, %q", tc.header, name, value, tc.wantName, tc.wantValue)
			}
		})
	}
}
