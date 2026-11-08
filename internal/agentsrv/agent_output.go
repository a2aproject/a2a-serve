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
	"sync"
	"time"
)

// maxAgentMessage bounds one JSON-RPC message from the agent, as the email
// channel bounds a mail.
const maxAgentMessage = 64 << 20

// errAgentMessageTooLarge fails the reads of a [boundedMessages].
type errAgentMessageTooLarge struct{ limit int }

func (e errAgentMessageTooLarge) Error() string {
	if e.limit%(1<<20) != 0 {
		return fmt.Sprintf("the agent sent a message over %d bytes", e.limit)
	}
	return fmt.Sprintf("the agent sent a message over %d MiB", e.limit>>20)
}

// boundedMessages fails reads from the agent once a JSON value it sends, or
// the whitespace between two values, exceeds limit bytes. The ACP decoder
// buffers each value whole, and the whitespace before it, so an endless
// value would otherwise run the server out of memory. It tracks just enough
// JSON syntax to tell where a top-level value starts and ends; the decoder
// rejects malformed input. Only the decoder's goroutine reads it.
type boundedMessages struct {
	io.ReadWriteCloser
	limit    int
	size     int
	depth    int
	inString bool
	escaped  bool
	inScalar bool
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

// scan counts the bytes of p in the values, or the whitespace between
// them, they belong to. When one exceeds the limit, it reports the offset of
// the first byte past it.
func (b *boundedMessages) scan(p []byte) (int, bool) {
	size, depth, inString, escaped, inScalar := b.size, b.depth, b.inString, b.escaped, b.inScalar
	defer func() { b.size, b.depth, b.inString, b.escaped, b.inScalar = size, depth, inString, escaped, inScalar }()
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
		space := c == ' ' || c == '\t' || c == '\n' || c == '\r'
		if depth == 0 && !inString {
			if inScalar && (space || c == '{' || c == '[' || c == '"') {
				inScalar = false
				size = 0
			}
			if !inScalar && !space {
				// A value starts.
				size = 0
			}
		}
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
		case depth == 0 && !space:
			inScalar = true
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

// agentReadEndGrace bounds how long closing an agent's stream waits for its
// reader to see the output end.
const agentReadEndGrace = 5 * time.Second

// awaitedClose closes an agent's stream and waits, up to grace, until its
// reader saw the output end. The ACP pool notices that an agent exited only
// once its reader does, and until then sends new prompts to the closed
// agent, which fail.
type awaitedClose struct {
	io.ReadWriteCloser
	grace time.Duration
	ended chan struct{}
	once  sync.Once
}

func newAwaitedClose(rwc io.ReadWriteCloser, grace time.Duration) *awaitedClose {
	return &awaitedClose{ReadWriteCloser: rwc, grace: grace, ended: make(chan struct{})}
}

func (a *awaitedClose) Read(p []byte) (int, error) {
	n, err := a.ReadWriteCloser.Read(p)
	if err != nil {
		a.once.Do(func() { close(a.ended) })
	}
	return n, err
}

func (a *awaitedClose) Close() error {
	err := a.ReadWriteCloser.Close()
	t := time.NewTimer(a.grace)
	defer t.Stop()
	select {
	case <-a.ended:
	case <-t.C:
	}
	return err
}
