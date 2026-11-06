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

// Package cardinfo keeps what the card of another agent says about it, so
// that the server can tell its own agent. The card comes from the other
// agent, so the text is bounded and kept to one line of plain text.
package cardinfo

import (
	"slices"
	"strings"
	"unicode"

	"github.com/a2aproject/a2a-go/v2/a2a"
)

// Summary is the description and skills of another agent, from its card.
type Summary struct {
	Description string  `json:"description,omitempty"`
	Skills      []Skill `json:"skills,omitempty"`
}

// Skill is a skill of another agent.
type Skill struct {
	ID          string `json:"id"`
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
}

// Bounds of a summary, in characters.
const (
	MaxDescription      = 300
	MaxSkills           = 10
	MaxSkillID          = 64
	MaxSkillName        = 60
	MaxSkillDescription = 150
)

// markdown are the characters left out of card text, which could format
// the instruction files or end the section the server manages in them.
const markdown = "`*#[]<>|~\\\""

// Of returns the summary of card, bounded and as plain text on one line.
func Of(card *a2a.AgentCard) Summary {
	if card == nil {
		return Summary{}
	}
	s := Summary{Description: Clean(card.Description, MaxDescription)}
	for _, sk := range card.Skills {
		if len(s.Skills) == MaxSkills {
			break
		}
		out := Skill{ID: cleanID(sk.ID), Name: Clean(sk.Name, MaxSkillName), Description: Clean(sk.Description, MaxSkillDescription)}
		if out.ID == "" {
			continue
		}
		if out.Name == out.ID {
			out.Name = ""
		}
		s.Skills = append(s.Skills, out)
	}
	return s
}

// IsZero reports whether s says nothing.
func (s Summary) IsZero() bool { return s.Description == "" && len(s.Skills) == 0 }

// Equal reports whether s and o say the same.
func (s Summary) Equal(o Summary) bool {
	return s.Description == o.Description && slices.Equal(s.Skills, o.Skills)
}

// Text describes the agent on one line, with its own words in quotes:
// says of itself: "Writes Go services." Skills: review (Code review) "Reviews pull requests."
func (s Summary) Text() string {
	var parts []string
	if s.Description != "" {
		parts = append(parts, `says of itself: "`+s.Description+`"`)
	}
	if len(s.Skills) > 0 {
		skills := make([]string, len(s.Skills))
		for i, sk := range s.Skills {
			t := sk.ID
			if sk.Name != "" {
				t += " (" + sk.Name + ")"
			}
			if sk.Description != "" {
				t += ` "` + sk.Description + `"`
			}
			skills[i] = t
		}
		parts = append(parts, "Skills: "+strings.Join(skills, "; "))
	}
	return strings.Join(parts, " ")
}

// Clean returns s as one line of plain text of at most max characters:
// control characters and line breaks become spaces, markdown characters
// are left out, and runs of spaces become one.
func Clean(s string, max int) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case unicode.IsControl(r) || unicode.IsSpace(r) || r == '\u2028' || r == '\u2029':
			return ' '
		case strings.ContainsRune(markdown, r):
			return -1
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > max {
		s = strings.TrimSpace(string(r[:max-1])) + "…"
	}
	return s
}

// cleanID keeps the letters, digits, dots, dashes and underscores of id.
func cleanID(id string) string {
	id = strings.Map(func(r rune) rune {
		if r < unicode.MaxASCII && (unicode.IsLetter(r) || unicode.IsDigit(r) || strings.ContainsRune("._-", r)) {
			return r
		}
		return -1
	}, id)
	if len(id) > MaxSkillID {
		id = id[:MaxSkillID]
	}
	return id
}
