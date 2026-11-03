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
	"bufio"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"golang.org/x/crypto/scrypt"
)

// A snapshot is encrypted with a key derived from the passphrase with
// scrypt, in chunks of AES-256-GCM. Each chunk's nonce is a random prefix,
// the chunk's index and whether it is the last one, so chunks can't be
// reordered, and a snapshot cut short fails to decrypt.
const (
	magic      = "A2ABAK1\n"
	saltLen    = 16
	prefixLen  = 7
	chunkSize  = 64 << 10
	scryptN    = 1 << 15
	scryptR    = 8
	scryptP    = 1
	gcmTagSize = 16
)

// ErrWrongKey is returned when a snapshot doesn't decrypt with the key.
var ErrWrongKey = errors.New("the snapshot doesn't decrypt with this backup key, or it is damaged")

func newAEAD(passphrase string, salt []byte) (cipher.AEAD, error) {
	if passphrase == "" {
		return nil, errors.New("empty backup key")
	}
	key, err := scrypt.Key([]byte(passphrase), salt, scryptN, scryptR, scryptP, 32)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func nonce(prefix []byte, i uint32, last bool) []byte {
	n := make([]byte, 12)
	copy(n, prefix)
	binary.BigEndian.PutUint32(n[prefixLen:], i)
	if last {
		n[11] = 1
	}
	return n
}

type encryptWriter struct {
	w      io.Writer
	aead   cipher.AEAD
	prefix []byte
	i      uint32
	buf    []byte
}

// Encrypt returns a writer which encrypts what is written to it into w.
// Close writes the last chunk; it doesn't close w.
func Encrypt(w io.Writer, passphrase string) (io.WriteCloser, error) {
	header := make([]byte, len(magic)+saltLen+prefixLen)
	copy(header, magic)
	if _, err := rand.Read(header[len(magic):]); err != nil {
		return nil, err
	}
	salt := header[len(magic) : len(magic)+saltLen]
	aead, err := newAEAD(passphrase, salt)
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(header); err != nil {
		return nil, err
	}
	return &encryptWriter{w: w, aead: aead, prefix: header[len(magic)+saltLen:], buf: make([]byte, 0, chunkSize)}, nil
}

func (e *encryptWriter) Write(p []byte) (int, error) {
	n := 0
	for len(p) > 0 {
		// A full chunk is written once more data follows, so that the last
		// chunk is always written by Close, marked as last.
		if len(e.buf) == chunkSize {
			if err := e.flush(false); err != nil {
				return n, err
			}
		}
		k := copy(e.buf[len(e.buf):chunkSize], p)
		e.buf = e.buf[:len(e.buf)+k]
		p, n = p[k:], n+k
	}
	return n, nil
}

func (e *encryptWriter) flush(last bool) error {
	out := e.aead.Seal(nil, nonce(e.prefix, e.i, last), e.buf, nil)
	e.i++
	e.buf = e.buf[:0]
	_, err := e.w.Write(out)
	return err
}

func (e *encryptWriter) Close() error { return e.flush(true) }

type decryptReader struct {
	r      *bufio.Reader
	aead   cipher.AEAD
	prefix []byte
	i      uint32
	out    []byte
	done   bool
}

// Decrypt returns a reader of the plaintext of the snapshot r.
func Decrypt(r io.Reader, passphrase string) (io.Reader, error) {
	header := make([]byte, len(magic)+saltLen+prefixLen)
	if _, err := io.ReadFull(r, header); err != nil {
		return nil, fmt.Errorf("reading the snapshot header: %w", err)
	}
	if string(header[:len(magic)]) != magic {
		return nil, errors.New("not an a2a-serve backup snapshot")
	}
	aead, err := newAEAD(passphrase, header[len(magic):len(magic)+saltLen])
	if err != nil {
		return nil, err
	}
	return &decryptReader{r: bufio.NewReaderSize(r, chunkSize+gcmTagSize+1), aead: aead, prefix: header[len(magic)+saltLen:]}, nil
}

func (d *decryptReader) Read(p []byte) (int, error) {
	for len(d.out) == 0 {
		if d.done {
			return 0, io.EOF
		}
		if err := d.next(); err != nil {
			return 0, err
		}
	}
	n := copy(p, d.out)
	d.out = d.out[n:]
	return n, nil
}

func (d *decryptReader) next() error {
	chunk := make([]byte, chunkSize+gcmTagSize)
	n, err := io.ReadFull(d.r, chunk)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return err
	}
	last := n < len(chunk)
	if !last {
		if _, err := d.r.Peek(1); errors.Is(err, io.EOF) {
			last = true
		}
	}
	out, err := d.aead.Open(chunk[:0], nonce(d.prefix, d.i, last), chunk[:n], nil)
	if err != nil {
		return ErrWrongKey
	}
	d.i++
	d.out, d.done = out, last
	return nil
}
