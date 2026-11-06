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

// Threads a run can happen in.
const (
	// ThreadNew starts a new conversation for every run.
	ThreadNew = "new"
	// ThreadSame continues one conversation from run to run.
	ThreadSame = "same"
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
// one-time schedule whose time passed. last is the previous run, or zero.
// Cron fields match local wall time, so across a DST change a run never
// repeats a wall time up to that of last, and a run in the skipped hour
// happens right after it. A descriptor such as @every 2h counts elapsed
// time and ignores DST changes.
func (w When) Next(last, after time.Time) (time.Time, bool, error) {
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
	spec, ok := s.(*cron.SpecSchedule)
	if !ok {
		return next, !next.IsZero(), nil
	}
	for !last.IsZero() && !next.IsZero() && !wallClock(next, loc).After(wallClock(last, loc)) {
		next = s.Next(next)
	}
	if gap, ok := runInGap(spec, loc, after, next); ok {
		next = gap
	}
	return next, !next.IsZero(), nil
}

// wallClock is the local wall time of t in loc, as a time in UTC.
func wallClock(t time.Time, loc *time.Location) time.Time {
	t = t.In(loc)
	return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), time.UTC)
}

// runInGap returns the end of the first spring-forward gap in loc between
// after and before that has a wall time s matches.
func runInGap(s *cron.SpecSchedule, loc *time.Location, after, before time.Time) (time.Time, bool) {
	if before.IsZero() {
		return time.Time{}, false
	}
	t := after.In(loc)
	for {
		_, end := t.ZoneBounds()
		if end.IsZero() || !end.Before(before) {
			return time.Time{}, false
		}
		_, offBefore := end.Add(-time.Nanosecond).Zone()
		_, offAfter := end.Zone()
		if skipped := time.Duration(offAfter-offBefore) * time.Second; skipped > 0 {
			from := wallClock(end, loc).Add(-skipped)
			if run := s.Next(from.Add(-time.Second)); run.Before(from.Add(skipped)) {
				return end, true
			}
		}
		t = end
	}
}

// MinInterval returns the shortest time between two runs, or zero for a
// one-time schedule. It counts elapsed time in UTC, so DST changes don't
// shorten it.
func (w When) MinInterval() (time.Duration, error) {
	if w.Cron == "" {
		return 0, nil
	}
	s, err := ParseCron(w.Cron)
	if err != nil {
		return 0, err
	}
	if d, ok := s.(cron.ConstantDelaySchedule); ok {
		return d.Delay, nil
	}
	// Every day with runs has the same runs, so the runs of the first one
	// show the shortest interval, assuming the next day has runs too.
	first := s.Next(time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC).Add(-time.Second))
	if first.IsZero() {
		return 0, nil
	}
	dayEnd := time.Date(first.Year(), first.Month(), first.Day()+1, 0, 0, 0, 0, time.UTC)
	shortest, last := 24*time.Hour, first
	for next := s.Next(first); !next.IsZero() && next.Before(dayEnd); next = s.Next(next) {
		shortest, last = min(shortest, next.Sub(last)), next
	}
	return min(shortest, first.Add(24*time.Hour).Sub(last)), nil
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
	// Chat is <messenger>[:<who>]: the runs are rendered in who's direct
	// chat on the messenger. who defaults to As.
	Chat string `json:"chat,omitempty"`
	// Thread is ThreadNew (the default) or ThreadSame.
	Thread string `json:"thread,omitempty"`
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
	fs.StringVar(&r.Chat, "chat", "", "Show the runs, and ask their approvals, in a chat: `messenger[:who]`, such as telegram:owner (default: no chat; approvals go to the owner chat)")
	fs.StringVar(&r.Thread, "thread", ThreadNew, "Run in a new conversation each time, or continue the same one: `new|same`")
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
	switch r.Thread {
	case "", ThreadNew, ThreadSame:
	default:
		return fmt.Errorf("invalid --thread %q: want new or same", r.Thread)
	}
	_, err := r.When()
	return err
}
