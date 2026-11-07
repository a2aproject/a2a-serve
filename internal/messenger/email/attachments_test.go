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
	"errors"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/messenger"
)

func mailWithAttachments(attachments map[string]int) []byte {
	var sb strings.Builder
	sb.WriteString("From: alice@alice.org\r\nTo: coder@example.com\r\nSubject: files\r\nMessage-ID: <f1@alice.org>\r\n")
	sb.WriteString("Authentication-Results: mx.example.com; dmarc=pass header.from=alice.org\r\nMIME-Version: 1.0\r\n")
	sb.WriteString("Content-Type: multipart/mixed; boundary=b\r\n\r\n")
	sb.WriteString("--b\r\nContent-Type: text/plain\r\n\r\nsee attached\r\n")
	for name, size := range attachments {
		sb.WriteString("--b\r\nContent-Type: application/octet-stream\r\nContent-Transfer-Encoding: 8bit\r\n")
		sb.WriteString("Content-Disposition: attachment; filename=" + name + "\r\n\r\n")
		sb.WriteString(strings.Repeat("x", size))
		sb.WriteString("\r\n")
	}
	sb.WriteString("--b--\r\n")
	return []byte(sb.String())
}

func TestBodyRefusesAttachmentsOverTheLimit(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		attachments map[string]int
		wantFiles   []string
		wantRefused []string
	}{
		{name: "small attachment", attachments: map[string]int{"a.bin": 10}, wantFiles: []string{"a.bin"}},
		{name: "attachment over the limit", attachments: map[string]int{"big.bin": maxPart + 1}, wantRefused: []string{"big.bin"}},
		{name: "small attachment next to a large one", attachments: map[string]int{"a.bin": 10, "big.bin": maxPart + 1}, wantFiles: []string{"a.bin"}, wantRefused: []string{"big.bin"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			text, files, refused, err := body(mailWithAttachments(tc.attachments), false, false)
			if err != nil {
				t.Fatalf("body() error = %v, want nil", err)
			}
			if text != "see attached" {
				t.Fatalf("body() text = %q, want %q", text, "see attached")
			}
			var gotFiles []string
			for _, f := range files {
				gotFiles = append(gotFiles, f.Name)
				if len(f.Data) != tc.attachments[f.Name] {
					t.Fatalf("body() file %s has %d bytes, want %d", f.Name, len(f.Data), tc.attachments[f.Name])
				}
			}
			var gotRefused []string
			for _, err := range refused {
				var tooLarge *messenger.TooLargeError
				if !errors.As(err, &tooLarge) {
					t.Fatalf("body() refused %v, want a TooLargeError", err)
				}
				gotRefused = append(gotRefused, messenger.FailedAttachments(err)[0].Name)
			}
			if diff := cmp.Diff(tc.wantFiles, gotFiles); diff != "" {
				t.Fatalf("body() wrong files (-want +got) diff = %s", diff)
			}
			if diff := cmp.Diff(tc.wantRefused, gotRefused); diff != "" {
				t.Fatalf("body() wrong refused attachments (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestPrepareReportsRefusedAttachments(t *testing.T) {
	t.Parallel()
	c := newTestMessenger(t)
	in, err := parseUnprepared(t.Context(), c, string(mailWithAttachments(map[string]int{"a.bin": 10, "big.bin": maxPart + 1})))
	if err != nil {
		t.Fatalf("parseUnprepared() error = %v, want a message", err)
	}

	err = in.Prepare(t.Context(), &in)

	var failed []string
	for _, f := range messenger.FailedAttachments(err) {
		failed = append(failed, f.Name)
	}
	if diff := cmp.Diff([]string{"big.bin"}, failed); diff != "" {
		t.Fatalf("Inbound.Prepare() wrong failed attachments (-want +got) diff = %s", diff)
	}
	if len(in.Files) != 1 || in.Files[0].Name != "a.bin" {
		t.Fatalf("Inbound.Files = %+v, want a.bin", in.Files)
	}
	if in.Text != "see attached" {
		t.Fatalf("Inbound.Text = %q, want %q", in.Text, "see attached")
	}
}
