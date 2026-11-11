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

// maxSpeakBlock is the longest <speak> block; a longer one is shown as
// text.
const maxSpeakBlock = 4 * MaxSpeechText

// SpeakFilter takes the <speak> blocks out of streamed text and keeps the
// last complete one. A block starts with <speak> at the start of a line,
// indented by at most three spaces, outside code fences, and ends with
// </speak> at the end of a line. A reply that mentions the tag in code or
// in a sentence keeps it, and so does a block that never ends. Tags may be
// split across chunks.
type SpeakFilter struct {
	inside bool
	// fence is the marker of the open code fence, such as ``` or ~~~~.
	fence string
	// midLine is set once the current line has text that can't start a
	// block or a fence.
	midLine bool
	// head is the start of the current line while it may still start a
	// block or a fence.
	head  string
	block strings.Builder
	// closing is set when the block ends with </speak>, which closes it
	// at the end of the line.
	closing bool
	spoken  string
	found   bool
}

// Write returns the text of chunk outside <speak> blocks. The start of a
// line that may start a block or a fence is held back until the next chunk
// tells.
func (f *SpeakFilter) Write(chunk string) string {
	var out strings.Builder
	for i := 0; i < len(chunk); i++ {
		c := chunk[i]
		if f.inside {
			f.inBlock(&out, c)
			continue
		}
		if c == '\n' {
			f.endHead(&out)
			out.WriteByte(c)
			f.midLine = false
			continue
		}
		if f.midLine {
			out.WriteByte(c)
			continue
		}
		f.head += chunk[i : i+1]
		f.readHead(&out)
	}
	return out.String()
}

// readHead decides what the start of the line is once it can tell.
func (f *SpeakFilter) readHead(out *strings.Builder) {
	start := strings.TrimLeft(f.head, " ")
	indent := len(f.head) - len(start)
	switch {
	case indent > 3 || strings.HasPrefix(start, "\t"):
		f.endHead(out)
	case start == "":
	case f.fence == "" && start == speakOpen:
		f.head, f.inside, f.midLine = "", true, true
		f.block.Reset()
	case f.fence == "" && strings.HasPrefix(speakOpen, start):
	case strings.Trim(start, "`") == "" || strings.Trim(start, "~") == "":
		// A fence marker may still grow.
	default:
		f.endHead(out)
	}
}

// endHead shows the held start of the line, and opens or closes a code
// fence if the line starts with one.
func (f *SpeakFilter) endHead(out *strings.Builder) {
	if f.head == "" {
		return
	}
	start := strings.TrimLeft(f.head, " ")
	if len(f.head)-len(start) <= 3 && len(start) > 0 && (start[0] == '`' || start[0] == '~') {
		marker := start[:len(start)-len(strings.TrimLeft(start, start[:1]))]
		switch {
		case f.fence == "" && len(marker) >= 3:
			f.fence = marker
		case f.fence != "" && marker[0] == f.fence[0] && len(marker) >= len(f.fence) && strings.TrimSpace(start[len(marker):]) == "":
			f.fence = ""
		}
	}
	out.WriteString(f.head)
	f.head, f.midLine = "", true
}

// inBlock adds c to the open block. The block closes at the end of a line
// that ends with </speak>, and turns back into text when it grows past
// maxSpeakBlock.
func (f *SpeakFilter) inBlock(out *strings.Builder, c byte) {
	if f.closing && c == '\n' {
		f.close()
		out.WriteByte(c)
		f.midLine = false
		return
	}
	f.closing = false
	f.block.WriteByte(c)
	if f.block.Len() > maxSpeakBlock {
		out.WriteString(speakOpen + f.block.String())
		f.inside = false
		f.block.Reset()
		return
	}
	f.closing = strings.HasSuffix(f.block.String()[max(f.block.Len()-len(speakClose), 0):], speakClose)
}

func (f *SpeakFilter) close() {
	f.spoken, f.found = strings.TrimSuffix(f.block.String(), speakClose), true
	f.inside, f.closing = false, false
	f.block.Reset()
}

// Flush returns the text held back at the end of the stream or of a turn
// that stops for an answer: the start of a line that didn't become a
// block, or a block that never ended, which isn't one.
func (f *SpeakFilter) Flush() string {
	var out strings.Builder
	if f.inside && f.closing {
		f.close()
	}
	f.endHead(&out)
	f.midLine = false
	if f.inside {
		out.WriteString(speakOpen + f.block.String())
		f.inside = false
		f.block.Reset()
	}
	return out.String()
}

// Spoken returns the last complete <speak> block, and whether there was
// one.
func (f *SpeakFilter) Spoken() (string, bool) { return f.spoken, f.found }
