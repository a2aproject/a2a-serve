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

package backup

import (
	"bytes"
	"crypto/rand"
	"errors"
	"io"
	"testing"
)

func encrypt(t *testing.T, plain []byte, key string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, err := Encrypt(&buf, key)
	if err != nil {
		t.Fatalf("Encrypt() error = %v", err)
	}
	if _, err := w.Write(plain); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	return buf.Bytes()
}

func decrypt(enc []byte, key string) ([]byte, error) {
	r, err := Decrypt(bytes.NewReader(enc), key)
	if err != nil {
		return nil, err
	}
	return io.ReadAll(r)
}

func TestEncryptRoundTrip(t *testing.T) {
	t.Parallel()
	for _, size := range []int{0, 1, chunkSize - 1, chunkSize, chunkSize + 1, 3 * chunkSize} {
		plain := make([]byte, size)
		if _, err := rand.Read(plain); err != nil {
			t.Fatal(err)
		}
		got, err := decrypt(encrypt(t, plain, "passphrase"), "passphrase")
		if err != nil {
			t.Fatalf("Decrypt() of %d bytes error = %v", size, err)
		}
		if !bytes.Equal(got, plain) {
			t.Fatalf("Decrypt() of %d bytes returned %d different bytes", size, len(got))
		}
	}
}

func TestDecryptFails(t *testing.T) {
	t.Parallel()
	plain := bytes.Repeat([]byte("a2a"), chunkSize)
	enc := encrypt(t, plain, "passphrase")
	header := len(magic) + saltLen + prefixLen
	flipped := bytes.Clone(enc)
	flipped[header+10] ^= 1
	tests := []struct {
		name string
		enc  []byte
		key  string
	}{
		{name: "wrong key", enc: enc, key: "other"},
		{name: "cut at a chunk boundary", enc: enc[:header+chunkSize+gcmTagSize], key: "passphrase"},
		{name: "cut in a chunk", enc: enc[:len(enc)-5], key: "passphrase"},
		{name: "changed byte", enc: flipped, key: "passphrase"},
		{name: "not a snapshot", enc: []byte("PK\x03\x04 a zip file, not a snapshot"), key: "passphrase"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := decrypt(tc.enc, tc.key); err == nil {
				t.Fatalf("Decrypt() error = nil, want error")
			}
		})
	}
	if _, err := decrypt(enc, "other"); !errors.Is(err, ErrWrongKey) {
		t.Fatalf("Decrypt() with the wrong key error = %v, want %v", err, ErrWrongKey)
	}
}
