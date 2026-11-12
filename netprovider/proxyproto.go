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
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// The PROXY protocol v2 header that starts every inbound connection: the
// PROXY command with the UNSPEC address family, so it carries no addresses,
// and the caller and the token in TLVs of the custom range.
var proxySignature = []byte("\r\n\r\n\x00\r\nQUIT\n")

// TLV types of the header.
const (
	TLVToken     = 0xE0
	TLVNode      = 0xE1
	TLVEmail     = 0xE2
	TLVMachine   = 0xE3
	TLVClientKey = 0xE4
)

// MaxHeader bounds the header, signature included.
const MaxHeader = 4096

// maxField bounds each value of the header.
const maxField = 512

const (
	proxyVersionCommand = 0x21 // version 2, PROXY
	proxyUnspec         = 0x00
	proxyFixedLen       = 16
)

// Header is the start of an inbound connection.
type Header struct {
	Token  string
	Caller Caller
}

// ErrHeader is wrapped by the errors of [ReadHeader] for a malformed header.
var ErrHeader = errors.New("invalid PROXY v2 header")

// AppendHeader appends the header to b.
func AppendHeader(b []byte, h Header) ([]byte, error) {
	var tlvs []byte
	for _, f := range []struct {
		typ byte
		v   string
	}{
		{TLVToken, h.Token}, {TLVNode, h.Caller.Node}, {TLVEmail, h.Caller.Email},
		{TLVMachine, h.Caller.Machine}, {TLVClientKey, h.Caller.ClientKey},
	} {
		if f.v == "" {
			continue
		}
		if len(f.v) > maxField {
			return nil, fmt.Errorf("%w: TLV 0x%X is longer than %d bytes", ErrHeader, f.typ, maxField)
		}
		tlvs = append(tlvs, f.typ)
		tlvs = binary.BigEndian.AppendUint16(tlvs, uint16(len(f.v)))
		tlvs = append(tlvs, f.v...)
	}
	if proxyFixedLen+len(tlvs) > MaxHeader {
		return nil, fmt.Errorf("%w: longer than %d bytes", ErrHeader, MaxHeader)
	}
	b = append(b, proxySignature...)
	b = append(b, proxyVersionCommand, proxyUnspec)
	b = binary.BigEndian.AppendUint16(b, uint16(len(tlvs)))
	return append(b, tlvs...), nil
}

// WriteHeader writes the header to w.
func WriteHeader(w io.Writer, h Header) error {
	b, err := AppendHeader(nil, h)
	if err != nil {
		return err
	}
	_, err = w.Write(b)
	return err
}

// ReadHeader reads the header from r. It reads nothing past it.
func ReadHeader(r io.Reader) (Header, error) {
	fixed := make([]byte, proxyFixedLen)
	if _, err := io.ReadFull(r, fixed); err != nil {
		return Header{}, fmt.Errorf("%w: %w", ErrHeader, err)
	}
	if !bytes.Equal(fixed[:12], proxySignature) {
		return Header{}, fmt.Errorf("%w: no signature", ErrHeader)
	}
	if fixed[12] != proxyVersionCommand {
		return Header{}, fmt.Errorf("%w: want version 2 with the PROXY command, got 0x%02X", ErrHeader, fixed[12])
	}
	n := int(binary.BigEndian.Uint16(fixed[14:]))
	if proxyFixedLen+n > MaxHeader {
		return Header{}, fmt.Errorf("%w: longer than %d bytes", ErrHeader, MaxHeader)
	}
	rest := make([]byte, n)
	if _, err := io.ReadFull(r, rest); err != nil {
		return Header{}, fmt.Errorf("%w: %w", ErrHeader, err)
	}
	// Addresses of other families come before the TLVs; the server ignores
	// them, as the caller is in the TLVs.
	switch fixed[13] {
	case proxyUnspec:
	case 0x11, 0x12: // TCP and UDP over IPv4
		rest, n = skip(rest, 12)
	case 0x21, 0x22: // TCP and UDP over IPv6
		rest, n = skip(rest, 36)
	case 0x31, 0x32: // Unix addresses
		rest, n = skip(rest, 216)
	default:
		return Header{}, fmt.Errorf("%w: unknown address family 0x%02X", ErrHeader, fixed[13])
	}
	if n < 0 {
		return Header{}, fmt.Errorf("%w: addresses longer than the header", ErrHeader)
	}
	var h Header
	seen := map[byte]bool{}
	for len(rest) > 0 {
		if len(rest) < 3 {
			return Header{}, fmt.Errorf("%w: truncated TLV", ErrHeader)
		}
		typ, l := rest[0], int(binary.BigEndian.Uint16(rest[1:3]))
		if len(rest) < 3+l {
			return Header{}, fmt.Errorf("%w: truncated TLV", ErrHeader)
		}
		v := string(rest[3 : 3+l])
		rest = rest[3+l:]
		var dst *string
		switch typ {
		case TLVToken:
			dst = &h.Token
		case TLVNode:
			dst = &h.Caller.Node
		case TLVEmail:
			dst = &h.Caller.Email
		case TLVMachine:
			dst = &h.Caller.Machine
		case TLVClientKey:
			dst = &h.Caller.ClientKey
		default:
			continue
		}
		if seen[typ] {
			return Header{}, fmt.Errorf("%w: TLV 0x%X twice", ErrHeader, typ)
		}
		if l > maxField {
			return Header{}, fmt.Errorf("%w: TLV 0x%X is longer than %d bytes", ErrHeader, typ, maxField)
		}
		seen[typ] = true
		*dst = v
	}
	return h, nil
}

func skip(b []byte, n int) ([]byte, int) {
	if len(b) < n {
		return nil, -1
	}
	return b[n:], len(b) - n
}
