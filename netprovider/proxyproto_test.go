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

package netprovider

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestHeaderRoundTrip(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		h    Header
	}{
		{name: "token only", h: Header{Token: "t"}},
		{name: "caller", h: Header{Token: "t", Caller: Caller{Node: "n1", Email: "anna@example.com", Machine: "laptop", ClientKey: "k"}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			if err := WriteHeader(&buf, tc.h); err != nil {
				t.Fatalf("WriteHeader() error = %v", err)
			}
			buf.WriteString("GET / HTTP/1.1\r\n")
			got, err := ReadHeader(&buf)
			if err != nil {
				t.Fatalf("ReadHeader() error = %v", err)
			}
			if diff := cmp.Diff(tc.h, got); diff != "" {
				t.Fatalf("ReadHeader() wrong result (-want +got) diff = %s", diff)
			}
			if rest := buf.String(); rest != "GET / HTTP/1.1\r\n" {
				t.Fatalf("ReadHeader() left %q, want the request", rest)
			}
		})
	}
}

func TestReadHeaderRefuses(t *testing.T) {
	t.Parallel()
	valid, err := AppendHeader(nil, Header{Token: "t"})
	if err != nil {
		t.Fatalf("AppendHeader() error = %v", err)
	}
	dup := append([]byte{}, valid...)
	dup = append(dup, TLVToken, 0, 1, 'x')
	dup[15] += 4
	tests := []struct {
		name  string
		input []byte
	}{
		{name: "plain HTTP", input: []byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n")},
		{name: "version 1", input: []byte("PROXY TCP4 1.2.3.4 5.6.7.8 1 2\r\n")},
		{name: "truncated", input: valid[:len(valid)-1]},
		{name: "duplicate TLV", input: dup},
		{name: "too long", input: append(append(append([]byte{}, proxySignature...), 0x21, 0x00, 0xFF, 0xFF), make([]byte, 0xFFFF)...)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := ReadHeader(bytes.NewReader(tc.input)); !errors.Is(err, ErrHeader) {
				t.Fatalf("ReadHeader() error = %v, want %v", err, ErrHeader)
			}
		})
	}
}

func TestAppendHeaderRefusesLongField(t *testing.T) {
	t.Parallel()
	if _, err := AppendHeader(nil, Header{Token: "t", Caller: Caller{Node: strings.Repeat("n", maxField+1)}}); !errors.Is(err, ErrHeader) {
		t.Fatalf("AppendHeader() error = %v, want %v", err, ErrHeader)
	}
}

func TestErrorIs(t *testing.T) {
	t.Parallel()
	err := RPCErrorOf(&Error{Kind: KindUnsupported, Problem: "no peers here"}).Err()
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("errors.Is(%v, ErrUnsupported) = false, want true", err)
	}
	if errors.Is(err, ErrNotMember) {
		t.Fatalf("errors.Is(%v, ErrNotMember) = true, want false", err)
	}
	if got := ErrUnsupported.Error(); got == "" {
		t.Fatalf("ErrUnsupported.Error() = %q, want a message", got)
	}
}
