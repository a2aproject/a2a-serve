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
	"errors"
	"io"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/google/go-cmp/cmp"
)

type readOnly struct{ io.Reader }

func (readOnly) Write(p []byte) (int, error) { return len(p), nil }
func (readOnly) Close() error                { return nil }

func TestBoundedMessages(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		in      string
		want    string
		wantErr error
	}{
		{name: "messages at the limit", in: "{\"a\":1}\n[1,22]\n", want: "{\"a\":1}\n[1,22]\n"},
		{name: "values without separators", in: "{\"a\":1}{\"b\":2}\"abcde\"", want: "{\"a\":1}{\"b\":2}\"abcde\""},
		{name: "newlines inside a value", in: "{\"a\":\n\n1,\n\"b\":2}", want: "{\"a\":\n\n", wantErr: errAgentMessageTooLarge{7}},
		{name: "object over the limit", in: "{\"a\":12}\n", want: "{\"a\":12", wantErr: errAgentMessageTooLarge{7}},
		{name: "brackets and escapes in a string", in: "\"}}\\\"}}\"", want: "\"}}\\\"}}", wantErr: errAgentMessageTooLarge{7}},
		{name: "endless number", in: "12345678", want: "1234567", wantErr: errAgentMessageTooLarge{7}},
		{name: "endless whitespace", in: "{}        ", want: "{}       ", wantErr: errAgentMessageTooLarge{7}},
		{name: "whitespace before a value", in: "\n\n{\"a\":1}", want: "\n\n{\"a\":1}"},
		{name: "scalars separated by whitespace", in: "1234567 1234567\n", want: "1234567 1234567\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			for _, r := range []io.Reader{strings.NewReader(tc.in), iotest.OneByteReader(strings.NewReader(tc.in))} {
				got, err := io.ReadAll(newBoundedMessages(readOnly{r}, 7))
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("boundedMessages.Read() error = %v, want %v", err, tc.wantErr)
				}
				if diff := cmp.Diff(tc.want, string(got)); diff != "" {
					t.Fatalf("boundedMessages.Read() wrong result (-want +got) diff = %s", diff)
				}
			}
		})
	}
}

func TestAgentMessageTooLargeError(t *testing.T) {
	t.Parallel()
	tests := []struct {
		limit int
		want  string
	}{
		{limit: maxAgentMessage, want: "the agent sent a message over 64 MiB"},
		{limit: 1000, want: "the agent sent a message over 1000 bytes"},
	}
	for _, tc := range tests {
		if got := (errAgentMessageTooLarge{tc.limit}).Error(); got != tc.want {
			t.Fatalf("errAgentMessageTooLarge.Error() = %q, want %q", got, tc.want)
		}
	}
}
