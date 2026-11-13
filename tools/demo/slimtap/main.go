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

// Command slimtap sits between SLIM clients and a SLIM node, forwards their
// traffic and prints what the node sees of each application message: who
// sends it to whom, and the start of its payload. The demo of
// examples/07-slim-network shows with it that the node routes by SLIM name
// and can't read what agents say.
//
//	slimtap <listen addr> <node addr>
package main

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"sync"
	"time"
)

var out sync.Mutex

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: slimtap <listen addr> <node addr>")
		os.Exit(2)
	}
	ln, err := net.Listen("tcp", os.Args[1])
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("\033[1m%-8s %-13s   %-13s %7s  %s\033[0m\n", "TIME", "FROM", "TO", "SIZE", "WHAT THE NODE CAN READ")
	for {
		c, err := ln.Accept()
		if err != nil {
			log.Fatal(err)
		}
		go relay(c, os.Args[2])
	}
}

func relay(c net.Conn, node string) {
	u, err := net.Dial("tcp", node)
	if err != nil {
		log.Printf("dialing the node: %v", err)
		if err := c.Close(); err != nil {
			log.Printf("closing the client: %v", err)
		}
		return
	}
	go pipe(u, c, true)
	pipe(c, u, false)
}

// pipe copies src to dst, and decodes the gRPC messages of the HTTP/2
// stream as they pass from a client. Every message leaves some client, so
// each shows once.
func pipe(dst, src net.Conn, fromClient bool) {
	if !fromClient {
		if _, err := io.Copy(dst, src); err != nil && !errors.Is(err, net.ErrClosed) {
			log.Printf("forwarding: %v", err)
		}
		if err := dst.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			log.Printf("closing: %v", err)
		}
		return
	}
	pr, pw := io.Pipe()
	go func() {
		if err := decode(pr); !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrClosedPipe) {
			log.Printf("decoding: %v", err)
		}
		if _, err := io.Copy(io.Discard, pr); err != nil {
			log.Printf("draining: %v", err)
		}
	}()
	if _, err := io.Copy(io.MultiWriter(dst, pw), src); err != nil && !errors.Is(err, net.ErrClosed) {
		log.Printf("forwarding: %v", err)
	}
	if err := pw.Close(); err != nil {
		log.Printf("closing the decoder: %v", err)
	}
	if err := dst.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		log.Printf("closing: %v", err)
	}
}

// decode prints the application messages of a client's HTTP/2 stream,
// which starts with the HTTP/2 preface. It returns io.EOF when the stream
// ends.
func decode(r io.Reader) error {
	if _, err := io.ReadFull(r, make([]byte, 24)); err != nil {
		return err
	}
	streams := map[uint32][]byte{}
	for {
		h := make([]byte, 9)
		if _, err := io.ReadFull(r, h); err != nil {
			return err
		}
		p := make([]byte, int(h[0])<<16|int(h[1])<<8|int(h[2]))
		if _, err := io.ReadFull(r, p); err != nil {
			return err
		}
		if h[3] != 0 {
			continue
		}
		if h[4]&0x8 != 0 && len(p) > 0 {
			p = p[1 : len(p)-int(p[0])]
		}
		sid := binary.BigEndian.Uint32(h[5:]) & 0x7fffffff
		b := append(streams[sid], p...)
		for len(b) >= 5 {
			n := int(binary.BigEndian.Uint32(b[1:5]))
			if len(b) < 5+n {
				break
			}
			if b[0] == 0 {
				show(b[5 : 5+n])
			}
			b = b[5+n:]
		}
		streams[sid] = b
	}
}

// show prints an application message of the SLIM data plane:
// Message.publish(3) holds header(1) with source(1) and destination(2)
// names, and msg(3).app_payload(1).blob(2). The first bytes of an MLS
// message are the same for the whole group, so show prints the last ones.
func show(m []byte) {
	pub := field(m, 3)
	hdr := field(pub, 1)
	blob := field(field(field(pub, 3), 1), 2)
	if hdr == nil || len(blob) == 0 {
		return
	}
	from, to := name(field(hdr, 1)), name(field(hdr, 2))
	kind := "not MLS"
	// An MLSMessage starts with version mls10 (1) and the wire format;
	// mls_private_message (2) is an encrypted application message.
	if len(blob) > 4 && binary.BigEndian.Uint16(blob) == 1 && binary.BigEndian.Uint16(blob[2:]) == 2 {
		kind = "MLS ciphertext"
	}
	tail := blob[max(0, len(blob)-10):]
	out.Lock()
	defer out.Unlock()
	fmt.Printf("\033[2m%s\033[0m %-13s → %-13s %5d B  %s \033[33m…%s\033[0m\n",
		time.Now().Format("15:04:05"), from, to, len(blob), kind, hex.EncodeToString(tail))
}

// name returns the last component of a SLIM name: id(1) and the
// components(2) org(1), namespace(2) and agent(3).
func name(n []byte) string {
	return string(field(field(n, 2), 3))
}

// field returns the first length-delimited field num of a protobuf message.
func field(b []byte, num uint64) []byte {
	for len(b) > 0 {
		key, n := binary.Uvarint(b)
		if n <= 0 {
			return nil
		}
		b = b[n:]
		switch key & 7 {
		case 0:
			_, n := binary.Uvarint(b)
			if n <= 0 {
				return nil
			}
			b = b[n:]
		case 1:
			if len(b) < 8 {
				return nil
			}
			b = b[8:]
		case 5:
			if len(b) < 4 {
				return nil
			}
			b = b[4:]
		case 2:
			l, n := binary.Uvarint(b)
			if n <= 0 || l > uint64(len(b)-n) {
				return nil
			}
			v := b[n : n+int(l)]
			b = b[n+int(l):]
			if key>>3 == num {
				return v
			}
		default:
			return nil
		}
	}
	return nil
}
