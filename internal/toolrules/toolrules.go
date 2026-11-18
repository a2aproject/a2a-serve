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

// Package toolrules matches the tool calls that an agent asks permission
// for against the entries of --allow-tools and --deny-tools: ACP tool
// kinds, MCP tools (tool:<server>.<tool>) and shell commands
// (command:<command>). Preferences and grants use the same entries.
package toolrules

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

// Prefixes of the entries that don't match a tool kind.
const (
	PrefixTool    = "tool:"
	PrefixCommand = "command:"
)

// Kinds are the ACP tool kinds that kind entries match.
var Kinds = []string{"read", "edit", "delete", "move", "search", "execute", "think", "fetch", "switch_mode", "other"}

// Tool kinds with a meaning of their own here.
const (
	KindExecute = "execute"
	KindOther   = "other"
)

// MaxSuggestion is the longest rule that [Suggest] offers.
const MaxSuggestion = 100

// form is the kind of call an entry matches.
type form int

const (
	formKind form = iota
	formTool
	formCommand
)

// Entry is a parsed entry of --allow-tools or --deny-tools.
type Entry struct {
	form    form
	pattern string
}

// Parse reads an entry: a tool kind glob, tool:<pattern> or
// command:<pattern>.
func Parse(s string) (Entry, error) {
	switch {
	case strings.HasPrefix(s, PrefixTool):
		p := strings.TrimPrefix(s, PrefixTool)
		if p == "" || strings.ContainsFunc(p, isSpace) {
			return Entry{}, errors.New("want tool:<server>.<tool>, such as tool:grafana.*")
		}
		return Entry{form: formTool, pattern: p}, nil
	case strings.HasPrefix(s, PrefixCommand):
		p := normalizeSpace(strings.TrimPrefix(s, PrefixCommand))
		if p == "" {
			return Entry{}, errors.New("want command:<command>, such as command:kubectl get *")
		}
		if !Simple(p) {
			return Entry{}, errors.New("a command rule can't match a command with ; & | ` $( < > or a line break, so such commands always ask")
		}
		return Entry{form: formCommand, pattern: p}, nil
	}
	if !slices.ContainsFunc(Kinds, func(k string) bool { return glob(s, k) }) {
		return Entry{}, fmt.Errorf("want ACP tool kinds (%s) or a glob such as *, tool:<server>.<tool> or command:<command>", strings.Join(Kinds, ", "))
	}
	return Entry{form: formKind, pattern: s}, nil
}

// Validate reports whether s is a valid entry.
func Validate(s string) error {
	_, err := Parse(s)
	return err
}

// String returns the entry as it is written.
func (e Entry) String() string {
	switch e.form {
	case formTool:
		return PrefixTool + e.pattern
	case formCommand:
		return PrefixCommand + e.pattern
	}
	return e.pattern
}

// IsKind reports whether the entry matches tool kinds.
func (e Entry) IsKind() bool { return e.form == formKind }

// Matches reports whether the entry matches c.
func (e Entry) Matches(c Call) bool {
	switch e.form {
	case formTool:
		return c.Tool != "" && glob(e.pattern, c.Tool)
	case formCommand:
		if c.Command == "" || !Simple(c.Command) {
			return false
		}
		command := normalizeSpace(c.Command)
		if prefix, ok := strings.CutSuffix(e.pattern, " *"); ok && command == prefix {
			return true
		}
		return glob(e.pattern, command)
	}
	return glob(e.pattern, c.Kind)
}

// specificity orders entries that match the same call: tool and command
// entries before kind entries, then longer patterns, without their *s.
func (e Entry) specificity() (int, int) {
	rank := 1
	if e.form == formKind {
		rank = 0
	}
	return rank, len(strings.ReplaceAll(e.pattern, "*", ""))
}

// moreSpecific reports whether a decides before b.
func moreSpecific(a, b Entry) bool {
	ar, al := a.specificity()
	br, bl := b.specificity()
	if ar != br {
		return ar > br
	}
	return al > bl
}

// Rules are the entries of --allow-tools and --deny-tools.
type Rules struct {
	Allow []string
	Deny  []string
}

// Decision is how [Rules] decide a call.
type Decision struct {
	Allow bool
	// Entry is the entry that decided.
	Entry string
}

// Decide returns the decision of the most specific entry that matches c,
// or false if none does. Between equally specific entries, deny decides.
// Invalid entries match nothing.
func (r Rules) Decide(c Call) (Decision, bool) {
	var best Entry
	var out Decision
	found := false
	for _, list := range []struct {
		entries []string
		allow   bool
	}{{r.Deny, false}, {r.Allow, true}} {
		for _, s := range list.entries {
			e, err := Parse(s)
			if err != nil || !e.Matches(c) {
				continue
			}
			if found && !moreSpecific(e, best) {
				continue
			}
			best, out, found = e, Decision{Allow: list.allow, Entry: s}, true
		}
	}
	return out, found
}

// Empty reports whether there are no entries.
func (r Rules) Empty() bool { return len(r.Allow) == 0 && len(r.Deny) == 0 }

// DeniesCommands reports whether an entry of deny can deny a shell command:
// a command entry, or a kind entry that matches execute.
func DeniesCommands(deny []string) bool {
	return slices.ContainsFunc(deny, func(s string) bool {
		e, err := Parse(s)
		return err == nil && (e.form == formCommand || e.form == formKind && glob(e.pattern, KindExecute))
	})
}

// Match returns the first of entries that matches c.
func Match(entries []string, c Call) (string, bool) {
	for _, s := range entries {
		if e, err := Parse(s); err == nil && e.Matches(c) {
			return s, true
		}
	}
	return "", false
}

// Redact returns entry with only the program of a command entry, for logs:
// an entry that comes from a command, such as a grant, can hold secrets.
func Redact(entry string) string {
	p, ok := strings.CutPrefix(entry, PrefixCommand)
	if !ok {
		return entry
	}
	words := strings.FieldsFunc(p, isSpace)
	if len(words) <= 1 {
		return entry
	}
	return PrefixCommand + words[0] + " ..."
}

// Simple reports whether a command has a single part that a command entry
// can match: no ; & | ` $( < > or line break, since `git status; curl … |
// sh` starts with `git status`.
func Simple(command string) bool {
	return !strings.ContainsAny(command, ";&|`<>\n\r") && !strings.Contains(command, "$(")
}

// Suggest returns the narrowest useful entry that covers c and calls like
// it, which "Always allow" grants: the MCP tool, the program of a command
// with its next word, or the kind. It returns "" for calls that no such
// entry covers well: commands with several parts, that start with a path or
// whose next word isn't plain, such as a path or a flag, and calls of the
// kinds execute and other.
func Suggest(c Call) string {
	var s string
	switch {
	case c.MCP:
		s = PrefixTool + c.Tool
	case c.Command != "":
		s = suggestCommand(c.Command)
	case c.Kind != KindExecute && c.Kind != KindOther && slices.Contains(Kinds, c.Kind):
		s = c.Kind
	}
	if len(s) > MaxSuggestion {
		return ""
	}
	return s
}

func suggestCommand(command string) string {
	if !Simple(command) {
		return ""
	}
	words := strings.FieldsFunc(command, isSpace)
	if len(words) == 0 || !plainWord(words[0]) {
		return ""
	}
	if runsAnything[words[0]] {
		return ""
	}
	if len(words) > 1 {
		if !plainWord(words[1]) {
			return ""
		}
		return PrefixCommand + words[0] + " " + words[1] + " *"
	}
	if runsArguments[words[0]] {
		return ""
	}
	return PrefixCommand + words[0] + " *"
}

// runsAnything are programs that run a command given in any of their
// arguments, so that no rule of their first words is narrow.
var runsAnything = map[string]bool{
	"env": true, "sudo": true, "doas": true, "su": true, "xargs": true, "find": true, "awk": true, "nohup": true,
	"nice": true, "timeout": true, "time": true, "exec": true, "eval": true, "command": true, "watch": true,
	"flock": true, "chroot": true, "setsid": true, "ssh": true,
}

// runsArguments are programs whose flags can run any code or command, such
// as bash -c, so that a rule of the program alone would allow anything.
// A rule with their subcommand or script stays narrow.
var runsArguments = map[string]bool{
	"bash": true, "sh": true, "zsh": true, "dash": true, "fish": true, "ksh": true,
	"python": true, "python3": true, "node": true, "perl": true, "ruby": true, "php": true, "deno": true, "bun": true,
	"npx": true, "uvx": true, "docker": true, "kubectl": true, "git": true,
}

// normalizeSpace collapses the runs of spaces and tabs of a command, which
// a shell splits words at, into single spaces.
func normalizeSpace(s string) string {
	return strings.Join(strings.FieldsFunc(s, isSpace), " ")
}

// plainWord reports whether w holds only letters, digits, ., _ and -, and
// doesn't start with -. Such a word can't be a URL, a path, a quoted string
// or an assignment, any of which could carry a secret.
func plainWord(w string) bool {
	if w == "" || w[0] == '-' {
		return false
	}
	for _, r := range w {
		ok := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-'
		if !ok {
			return false
		}
	}
	return true
}

func isSpace(r rune) bool { return r == ' ' || r == '\t' }

// glob reports whether s matches pattern, in which * matches any run of
// characters and every other character matches itself.
func glob(pattern, s string) bool {
	parts := strings.Split(pattern, "*")
	if len(parts) == 1 {
		return pattern == s
	}
	first, last := parts[0], parts[len(parts)-1]
	if !strings.HasPrefix(s, first) {
		return false
	}
	s = s[len(first):]
	for _, mid := range parts[1 : len(parts)-1] {
		i := strings.Index(s, mid)
		if i < 0 {
			return false
		}
		s = s[i+len(mid):]
	}
	return strings.HasSuffix(s, last)
}
