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

// Package schedule computes when the schedules of an agent run: on a cron
// expression, or once at a time, in a time zone.
package schedule

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
	"github.com/spf13/pflag"
)

// Conversations a run can happen in.
const (
	// ConversationNew starts a new conversation for every run.
	ConversationNew = "new"
	// ConversationSame continues one conversation from run to run.
	ConversationSame = "same"
)

var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// ValidateName checks a schedule name: lowercase letters, digits, '.', '_'
// and '-', at most 64 characters.
func ValidateName(name string) error {
	if !nameRE.MatchString(name) {
		return fmt.Errorf("invalid schedule name %q: use lowercase letters, digits, '.', '_' and '-'", name)
	}
	return nil
}

// Location loads a time zone; empty is the local one.
func Location(tz string) (*time.Location, error) {
	if tz == "" {
		return time.Local, nil
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return nil, fmt.Errorf("unknown time zone %q: use an IANA name such as Europe/Zurich", tz)
	}
	return loc, nil
}

// ParseCron parses a standard five-field cron expression, or a descriptor
// such as @daily or @every 2h.
func ParseCron(expr string) (cron.Schedule, error) {
	if strings.Contains(expr, "TZ=") {
		return nil, errors.New("set the time zone with --tz, not in the cron expression")
	}
	s, err := cron.ParseStandard(expr)
	if err != nil {
		return nil, fmt.Errorf("invalid cron expression %q: %w", expr, err)
	}
	return s, nil
}

// atLayouts are the layouts --at accepts, besides RFC 3339.
var atLayouts = []string{"2006-01-02 15:04", "2006-01-02T15:04", "2006-01-02 15:04:05", "2006-01-02T15:04:05"}

// ParseAt parses the time of a one-time schedule: RFC 3339, or a date and
// time such as 2026-11-06 17:00 in loc.
func ParseAt(v string, loc *time.Location) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t, nil
	}
	for _, layout := range atLayouts {
		if t, err := time.ParseInLocation(layout, v, loc); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("invalid time %q: use 2006-01-02 15:04 or RFC 3339", v)
}

// When is when a schedule runs: on Cron, or once At.
type When struct {
	Cron string
	At   time.Time
	TZ   string
}

// Validate checks that exactly one of Cron and At is set and that both
// parse.
func (w When) Validate() error {
	if (w.Cron == "") == w.At.IsZero() {
		return errors.New("set either --cron or --at")
	}
	if _, err := Location(w.TZ); err != nil {
		return err
	}
	if w.Cron != "" {
		_, err := ParseCron(w.Cron)
		return err
	}
	return nil
}

// Next returns the first run after after, or false when there is none: a
// one-time schedule whose time passed.
func (w When) Next(after time.Time) (time.Time, bool, error) {
	if w.Cron == "" {
		return w.At, w.At.After(after), nil
	}
	loc, err := Location(w.TZ)
	if err != nil {
		return time.Time{}, false, err
	}
	s, err := ParseCron(w.Cron)
	if err != nil {
		return time.Time{}, false, err
	}
	next := s.Next(after.In(loc))
	return next, !next.IsZero(), nil
}

// Request is what a schedule is created from, by the CLI or the harness.
type Request struct {
	Name   string `json:"name"`
	Cron   string `json:"cron,omitempty"`
	At     string `json:"at,omitempty"`
	TZ     string `json:"tz,omitempty"`
	Prompt string `json:"prompt"`
	// As is who the runs are for: owner or a pairing name. Default owner.
	As string `json:"as,omitempty"`
	// Deliver is <channel>[:<who>]: the runs are rendered in who's direct
	// chat on the channel. who defaults to As.
	Deliver string `json:"deliver,omitempty"`
	// Conversation is ConversationNew (the default) or ConversationSame.
	Conversation string `json:"conversation,omitempty"`
}

// RegisterFlags adds the flags of a schedule to fs. owner adds the flags
// only the owner can set.
func (r *Request) RegisterFlags(fs *pflag.FlagSet, owner bool) {
	fs.StringVar(&r.Cron, "cron", "", "Run on a cron `expression` of five fields, such as \"0 7 * * 1-5\", or a descriptor such as @daily or @every 2h")
	fs.StringVar(&r.At, "at", "", "Run once, at a `time` such as \"2026-11-06 17:00\" or RFC 3339")
	fs.StringVar(&r.TZ, "tz", "", "Time `zone` of --cron and --at, such as Europe/Zurich (default: the server's)")
	fs.StringVar(&r.Prompt, "prompt", "", "The prompt each run sends to the agent")
	if !owner {
		return
	}
	fs.StringVar(&r.As, "as", "", "Run as the owner or a pairing name (default: owner)")
	fs.StringVar(&r.Deliver, "deliver", "", "Show the runs in a chat: `channel[:who]`, such as telegram:owner (default: no chat)")
	fs.StringVar(&r.Conversation, "conversation", ConversationNew, "Run in a new conversation each time, or continue the same one: new or same")
}

// When returns when r runs.
func (r Request) When() (When, error) {
	w := When{Cron: r.Cron, TZ: r.TZ}
	if r.At != "" {
		loc, err := Location(r.TZ)
		if err != nil {
			return When{}, err
		}
		if w.At, err = ParseAt(r.At, loc); err != nil {
			return When{}, err
		}
	}
	return w, w.Validate()
}

// Validate checks everything of r but the users it names.
func (r Request) Validate() error {
	if err := ValidateName(r.Name); err != nil {
		return err
	}
	if strings.TrimSpace(r.Prompt) == "" {
		return errors.New("--prompt is required")
	}
	switch r.Conversation {
	case "", ConversationNew, ConversationSame:
	default:
		return fmt.Errorf("invalid --conversation %q: want new or same", r.Conversation)
	}
	_, err := r.When()
	return err
}
