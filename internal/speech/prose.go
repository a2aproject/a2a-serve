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

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// MaxSpoken is about how many characters of a reply are spoken when the
// agent didn't choose what to speak.
const MaxSpoken = 1000

// restNote ends a reply cut at MaxSpoken.
const restNote = "The rest is in the text."

var (
	mdLink     = regexp.MustCompile(`!?\[([^\]]*)\]\([^)]*\)`)
	bareURL    = regexp.MustCompile(`https?://\S+`)
	inlineCode = regexp.MustCompile("`([^`]*)`")
	emphasis   = regexp.MustCompile(`(\*\*|__|\*|~~)`)
	listMark   = regexp.MustCompile(`^\s*(?:[-*+]|\d+[.)])\s+`)
	spaces     = regexp.MustCompile(`[ \t]+`)
)

// Prose returns the part of a markdown reply that is read aloud: its text
// without code blocks, tables, links and markup, cut at a sentence within
// limit characters and within the MaxSpeechText bytes the services speak.
func Prose(markdown string, limit int) string {
	var lines []string
	fence := ""
	for line := range strings.SplitSeq(markdown, "\n") {
		trimmed := strings.TrimSpace(line)
		if fence != "" {
			if strings.HasPrefix(trimmed, fence) {
				fence = ""
			}
			continue
		}
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			fence = trimmed[:3]
			continue
		}
		if strings.HasPrefix(trimmed, "|") || strings.HasPrefix(line, "    ") || strings.HasPrefix(line, "\t") {
			continue
		}
		trimmed = strings.NewReplacer(speakOpen, "", speakClose, "").Replace(trimmed)
		trimmed = strings.TrimLeft(trimmed, "#> ")
		trimmed = listMark.ReplaceAllString(trimmed, "")
		trimmed = mdLink.ReplaceAllString(trimmed, "$1")
		trimmed = bareURL.ReplaceAllString(trimmed, "")
		trimmed = inlineCode.ReplaceAllString(trimmed, "$1")
		trimmed = emphasis.ReplaceAllString(trimmed, "")
		trimmed = strings.TrimSpace(spaces.ReplaceAllString(trimmed, " "))
		if trimmed == "" {
			continue
		}
		if !strings.ContainsAny(trimmed[len(trimmed)-1:], ".!?:;,") {
			trimmed += "."
		}
		lines = append(lines, trimmed)
	}
	return cut(strings.Join(lines, " "), limit)
}

// cut shortens text to the last sentence that ends within limit
// characters and MaxSpeechText bytes, and says that the rest is in the
// text.
func cut(text string, limit int) string {
	end := len(text)
	if n := 0; utf8.RuneCountInString(text) > limit {
		for i := range text {
			if n == limit {
				end = i
				break
			}
			n++
		}
	}
	end = min(end, MaxSpeechText)
	if end == len(text) {
		return text
	}
	end = max(end-len(restNote)-1, 0)
	for end > 0 && !utf8.RuneStart(text[end]) {
		end--
	}
	head := text[:end]
	stop := max(strings.LastIndex(head, ". "), strings.LastIndex(head, "! "), strings.LastIndex(head, "? "))
	if stop > len(head)/2 {
		head = head[:stop+1]
	} else if sp := strings.LastIndex(head, " "); sp > 0 {
		head = head[:sp]
	}
	return head + " " + restNote
}
