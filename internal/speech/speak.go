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

package speech

import "strings"

// The tags of the block in which an agent writes what is spoken of its
// reply.
const (
	speakOpen  = "<speak>"
	speakClose = "</speak>"
)

// SpeakFilter takes the <speak> blocks out of streamed text and keeps the
// last complete one. A block starts with <speak> at the start of a line
// outside code fences, so that a reply that mentions the tag in code or in
// a sentence keeps it. Tags may be split across chunks.
type SpeakFilter struct {
	inside bool
	// fence is set inside a fenced code block.
	fence bool
	// midLine is set once the current line has text that can't start a
	// block or a fence.
	midLine bool
	// head is the start of the current line while it may still start a
	// block or a fence.
	head string
	// tail is the end of the open block, which tells its closing tag.
	tail   string
	block  strings.Builder
	spoken string
	found  bool
}

// Write returns the text of chunk outside <speak> blocks. The start of a
// line that may start a block is held back until the next chunk tells.
func (f *SpeakFilter) Write(chunk string) string {
	var out strings.Builder
	for i := 0; i < len(chunk); i++ {
		c := chunk[i]
		if f.inside {
			f.inBlock(c)
			continue
		}
		if c == '\n' {
			out.WriteString(f.head)
			out.WriteByte(c)
			f.head, f.midLine = "", false
			continue
		}
		if f.midLine {
			out.WriteByte(c)
			continue
		}
		f.head += chunk[i : i+1]
		start := strings.TrimLeft(f.head, " \t")
		switch {
		case start == "":
		case !f.fence && start == speakOpen:
			f.head, f.inside, f.midLine = "", true, true
			f.block.Reset()
		case !f.fence && strings.HasPrefix(speakOpen, start):
		case strings.HasPrefix(start, "```") || strings.HasPrefix(start, "~~~"):
			f.fence = !f.fence
			out.WriteString(f.head)
			f.head, f.midLine = "", true
		case strings.HasPrefix("```", start) || strings.HasPrefix("~~~", start):
		default:
			out.WriteString(f.head)
			f.head, f.midLine = "", true
		}
	}
	return out.String()
}

// inBlock adds c to the open block, and closes it at </speak>.
func (f *SpeakFilter) inBlock(c byte) {
	// A block longer than what is spoken is cut later anyway.
	if f.block.Len() < 4*MaxSpeechText {
		f.block.WriteByte(c)
	}
	f.tail = f.tail[max(len(f.tail)-len(speakClose)+1, 0):] + string([]byte{c})
	if f.tail != speakClose {
		return
	}
	b := f.block.String()
	for k := len(speakClose); k > 0; k-- {
		if strings.HasSuffix(b, speakClose[:k]) {
			b = b[:len(b)-k]
			break
		}
	}
	f.spoken, f.found, f.inside, f.tail = b, true, false, ""
	f.block.Reset()
}

// Flush returns the text held back at the end of the stream: the start of
// a line that didn't become a block, or a block that was never closed,
// which isn't one.
func (f *SpeakFilter) Flush() string {
	rest := f.head
	f.head = ""
	if f.inside {
		rest = speakOpen + f.block.String()
		f.inside, f.tail = false, ""
		f.block.Reset()
	}
	return rest
}

// Spoken returns the last complete <speak> block, and whether there was
// one.
func (f *SpeakFilter) Spoken() (string, bool) { return f.spoken, f.found }
