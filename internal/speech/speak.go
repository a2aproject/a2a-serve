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
// last complete one. Tags may be split across chunks.
type SpeakFilter struct {
	inside bool
	// pending is the end of a chunk that may start a tag.
	pending string
	block   strings.Builder
	spoken  string
	found   bool
}

// Write returns the text of chunk outside <speak> blocks. The end of chunk
// that may start a tag is held back until the next chunk tells.
func (f *SpeakFilter) Write(chunk string) string {
	s := f.pending + chunk
	f.pending = ""
	var out strings.Builder
	for s != "" {
		tag := speakOpen
		if f.inside {
			tag = speakClose
		}
		i := strings.Index(s, tag)
		if i < 0 {
			keep := partialTag(s, tag)
			f.emit(&out, s[:len(s)-keep])
			f.pending = s[len(s)-keep:]
			break
		}
		f.emit(&out, s[:i])
		s = s[i+len(tag):]
		if f.inside {
			f.spoken, f.found = f.block.String(), true
		}
		f.block.Reset()
		f.inside = !f.inside
	}
	return out.String()
}

func (f *SpeakFilter) emit(out *strings.Builder, s string) {
	if f.inside {
		// A block longer than what is spoken is cut later anyway.
		if f.block.Len() < 4*MaxSpeechText {
			f.block.WriteString(s)
		}
		return
	}
	out.WriteString(s)
}

// Flush returns the text held back at the end of the stream: the start of
// a tag that never completed. An unclosed block counts as complete.
func (f *SpeakFilter) Flush() string {
	pending := f.pending
	f.pending = ""
	if !f.inside {
		return pending
	}
	f.block.WriteString(pending)
	f.spoken, f.found, f.inside = f.block.String(), true, false
	f.block.Reset()
	return ""
}

// Spoken returns the last complete <speak> block, and whether there was
// one.
func (f *SpeakFilter) Spoken() (string, bool) { return f.spoken, f.found }

// partialTag returns the length of the longest end of s that starts tag.
func partialTag(s, tag string) int {
	for n := min(len(s), len(tag)-1); n > 0; n-- {
		if strings.HasSuffix(s, tag[:n]) {
			return n
		}
	}
	return 0
}
