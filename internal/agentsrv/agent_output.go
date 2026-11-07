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
	"bytes"
	"fmt"
	"io"
)

// maxAgentMessage bounds one JSON-RPC message from the agent, as the email
// channel bounds a mail.
const maxAgentMessage = 64 << 20

// errAgentMessageTooLarge fails the reads of a [boundedMessages].
type errAgentMessageTooLarge struct{ limit int }

func (e errAgentMessageTooLarge) Error() string {
	return fmt.Sprintf("the agent sent a message over %d MiB", e.limit>>20)
}

// boundedMessages fails reads from the agent once a JSON value it sends
// exceeds max bytes. The ACP decoder buffers each value whole, so an
// endless value would otherwise run the server out of memory. It tracks
// just enough JSON syntax to tell where a top-level value ends; the decoder
// rejects malformed input. Only the decoder's goroutine reads it.
type boundedMessages struct {
	io.ReadWriteCloser
	limit    int
	size     int
	depth    int
	inString bool
	escaped  bool
	err      error
}

func newBoundedMessages(rwc io.ReadWriteCloser, limit int) *boundedMessages {
	return &boundedMessages{ReadWriteCloser: rwc, limit: limit}
}

func (b *boundedMessages) Read(p []byte) (int, error) {
	if b.err != nil {
		return 0, b.err
	}
	n, err := b.ReadWriteCloser.Read(p)
	if i, over := b.scan(p[:n]); over {
		b.err = errAgentMessageTooLarge{b.limit}
		return i, b.err
	}
	return n, err
}

// scan counts the bytes of p in the values they belong to. When a value
// exceeds the limit, it reports the offset of the first byte past it.
func (b *boundedMessages) scan(p []byte) (int, bool) {
	size, depth, inString, escaped := b.size, b.depth, b.inString, b.escaped
	defer func() { b.size, b.depth, b.inString, b.escaped = size, depth, inString, escaped }()
	for i := 0; i < len(p); i++ {
		if inString && !escaped {
			// Skips the plain text of a string at once.
			j := bytes.IndexAny(p[i:], `"\`)
			if j < 0 {
				j = len(p) - i
			}
			if size+j > b.limit {
				return i + b.limit - size, true
			}
			size += j
			if i += j; i == len(p) {
				break
			}
		}
		c := p[i]
		ended := false
		switch {
		case inString:
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
				ended = depth == 0
			}
		case c == '"':
			inString = true
		case c == '{' || c == '[':
			depth++
		case c == '}' || c == ']':
			depth = max(depth-1, 0)
			ended = depth == 0
		case depth == 0 && (c == ' ' || c == '\t' || c == '\n' || c == '\r'):
			size = 0
			continue
		}
		if size++; size > b.limit {
			return i, true
		}
		if ended {
			size = 0
		}
	}
	return len(p), false
}
