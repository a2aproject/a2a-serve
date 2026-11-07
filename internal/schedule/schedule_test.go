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

package schedule

import (
	"strings"
	"testing"
	"time"
)

func mustLocation(t *testing.T, tz string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(tz)
	if err != nil {
		t.Fatalf("time.LoadLocation(%q) error = %v", tz, err)
	}
	return loc
}

func TestWhenNext(t *testing.T) {
	t.Parallel()
	zurich := mustLocation(t, "Europe/Zurich")
	after := time.Date(2026, 11, 6, 8, 30, 0, 0, time.UTC) // a Friday, 09:30 in Zurich
	tests := []struct {
		name   string
		when   When
		want   time.Time
		wantOK bool
	}{
		{name: "weekday mornings in a time zone", when: When{Cron: "0 7 * * 1-5", TZ: "Europe/Zurich"}, want: time.Date(2026, 11, 9, 7, 0, 0, 0, zurich), wantOK: true},
		{name: "every two hours", when: When{Cron: "@every 2h"}, want: after.Add(2 * time.Hour), wantOK: true},
		{name: "daily in UTC", when: When{Cron: "@daily", TZ: "UTC"}, want: time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC), wantOK: true},
		{name: "once, later", when: When{At: after.Add(time.Hour)}, want: after.Add(time.Hour), wantOK: true},
		{name: "once, passed", when: When{At: after.Add(-time.Hour)}, want: after.Add(-time.Hour)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok, err := tc.when.Next(time.Time{}, after)
			if err != nil {
				t.Fatalf("When.Next() error = %v", err)
			}
			if !got.Equal(tc.want) || ok != tc.wantOK {
				t.Fatalf("When.Next() = %v, %v, want %v, %v", got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

func TestWhenNextAcrossDSTChanges(t *testing.T) {
	t.Parallel()
	newYork := mustLocation(t, "America/New_York")
	zurich := mustLocation(t, "Europe/Zurich")
	tests := []struct {
		name  string
		when  When
		last  time.Time
		after time.Time
		want  time.Time
	}{
		{
			name: "New York, daily in the repeated hour runs once",
			when: When{Cron: "30 1 * * *", TZ: "America/New_York"},
			last: time.Date(2026, 11, 1, 5, 30, 0, 0, time.UTC), after: time.Date(2026, 11, 1, 5, 30, 1, 0, time.UTC),
			want: time.Date(2026, 11, 2, 1, 30, 0, 0, newYork),
		},
		{
			name: "New York, after a restart in the repeated hour",
			when: When{Cron: "30 1 * * *", TZ: "America/New_York"},
			last: time.Date(2026, 11, 1, 5, 30, 0, 0, time.UTC), after: time.Date(2026, 11, 1, 6, 10, 0, 0, time.UTC),
			want: time.Date(2026, 11, 2, 1, 30, 0, 0, newYork),
		},
		{
			name: "New York, every 15 minutes in the repeated hour runs each time once",
			when: When{Cron: "*/15 1 * * *", TZ: "America/New_York"},
			last: time.Date(2026, 11, 1, 5, 45, 0, 0, time.UTC), after: time.Date(2026, 11, 1, 5, 45, 1, 0, time.UTC),
			want: time.Date(2026, 11, 2, 1, 0, 0, 0, newYork),
		},
		{
			name: "New York, hourly in the repeated hour",
			when: When{Cron: "0 * * * *", TZ: "America/New_York"},
			last: time.Date(2026, 11, 1, 5, 0, 0, 0, time.UTC), after: time.Date(2026, 11, 1, 5, 0, 1, 0, time.UTC),
			want: time.Date(2026, 11, 1, 7, 0, 0, 0, time.UTC),
		},
		{
			name: "New York, daily in the skipped hour runs right after it",
			when: When{Cron: "30 2 * * *", TZ: "America/New_York"},
			last: time.Date(2026, 3, 7, 2, 30, 0, 0, newYork), after: time.Date(2026, 3, 7, 2, 30, 1, 0, newYork),
			want: time.Date(2026, 3, 8, 7, 0, 0, 0, time.UTC),
		},
		{
			name: "New York, the day after the skipped hour",
			when: When{Cron: "30 2 * * *", TZ: "America/New_York"},
			last: time.Date(2026, 3, 8, 7, 0, 0, 0, time.UTC), after: time.Date(2026, 3, 8, 7, 0, 1, 0, time.UTC),
			want: time.Date(2026, 3, 9, 2, 30, 0, 0, newYork),
		},
		{
			name: "Zurich, daily in the repeated hour runs once",
			when: When{Cron: "30 2 * * *", TZ: "Europe/Zurich"},
			last: time.Date(2026, 10, 25, 0, 30, 0, 0, time.UTC), after: time.Date(2026, 10, 25, 0, 30, 1, 0, time.UTC),
			want: time.Date(2026, 10, 26, 2, 30, 0, 0, zurich),
		},
		{
			name: "Zurich, daily in the skipped hour runs right after it",
			when: When{Cron: "30 2 * * *", TZ: "Europe/Zurich"},
			last: time.Date(2026, 3, 28, 2, 30, 0, 0, zurich), after: time.Date(2026, 3, 28, 2, 30, 1, 0, zurich),
			want: time.Date(2026, 3, 29, 1, 0, 0, 0, time.UTC),
		},
		{
			name: "Zurich, every minute in the skipped hour runs once right after it",
			when: When{Cron: "* 2 * * *", TZ: "Europe/Zurich"},
			last: time.Date(2026, 3, 28, 2, 59, 0, 0, zurich), after: time.Date(2026, 3, 28, 2, 59, 1, 0, zurich),
			want: time.Date(2026, 3, 29, 1, 0, 0, 0, time.UTC),
		},
		{
			name:  "Zurich, a first run in the skipped hour",
			when:  When{Cron: "30 2 * * *", TZ: "Europe/Zurich"},
			after: time.Date(2026, 3, 28, 12, 0, 0, 0, zurich),
			want:  time.Date(2026, 3, 29, 1, 0, 0, 0, time.UTC),
		},
		{
			name: "Zurich, every hour of elapsed time ignores the change",
			when: When{Cron: "@every 1h", TZ: "Europe/Zurich"},
			last: time.Date(2026, 10, 25, 0, 30, 0, 0, time.UTC), after: time.Date(2026, 10, 25, 0, 30, 0, 0, time.UTC),
			want: time.Date(2026, 10, 25, 1, 30, 0, 0, time.UTC),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok, err := tc.when.Next(tc.last, tc.after)
			if err != nil {
				t.Fatalf("When.Next() error = %v", err)
			}
			if !got.Equal(tc.want) || !ok {
				t.Fatalf("When.Next() = %v, %v, want %v, true", got, ok, tc.want)
			}
		})
	}
}

func TestWhenNextOfStoredShortInterval(t *testing.T) {
	t.Parallel()
	after := time.Date(2026, 11, 6, 8, 30, 0, 0, time.UTC)
	got, ok, err := When{Cron: "@every 10s"}.Next(time.Time{}, after)
	if err != nil || !ok {
		t.Fatalf("When.Next() = %v, %v, %v, want a run: schedules stored before the floor keep running", got, ok, err)
	}
	if want := after.Add(10 * time.Second); !got.Equal(want) {
		t.Fatalf("When.Next() = %v, want %v", got, want)
	}
}

func TestParseCronMinimumInterval(t *testing.T) {
	t.Parallel()
	_, err := ParseCron("@every 30s")
	if err == nil || !strings.Contains(err.Error(), "at least 1 minute") {
		t.Fatalf("ParseCron(@every 30s) error = %v, want one naming the 1 minute minimum", err)
	}
}

func TestWhenMinInterval(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		when When
		want time.Duration
	}{
		{name: "once", when: When{At: time.Date(2026, 11, 6, 17, 0, 0, 0, time.UTC)}, want: 0},
		{name: "every", when: When{Cron: "@every 90s"}, want: 90 * time.Second},
		{name: "every minute", when: When{Cron: "* * * * *"}, want: time.Minute},
		{name: "every 5 minutes", when: When{Cron: "*/5 * * * *"}, want: 5 * time.Minute},
		{name: "two minutes close together", when: When{Cron: "0,2 9 * * *"}, want: 2 * time.Minute},
		{name: "across midnight", when: When{Cron: "0,57 0,23 * * *"}, want: 3 * time.Minute},
		{name: "end and start of consecutive hours", when: When{Cron: "0,58 9,10 * * *"}, want: 2 * time.Minute},
		{name: "twice a day", when: When{Cron: "0 9,17 * * 1-5"}, want: 8 * time.Hour},
		{name: "daily", when: When{Cron: "@daily"}, want: 24 * time.Hour},
		{name: "leap days", when: When{Cron: "0 12 29 2 *"}, want: 24 * time.Hour},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := tc.when.MinInterval()
			if err != nil {
				t.Fatalf("When.MinInterval() error = %v", err)
			}
			if got != tc.want {
				t.Fatalf("When.MinInterval() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestRequestValidate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		req     Request
		wantErr bool
	}{
		{name: "cron", req: Request{Name: "morning-brief", Cron: "0 7 * * 1-5", TZ: "Europe/Zurich", Prompt: "Summarize today's meetings."}},
		{name: "at", req: Request{Name: "friday", At: "2026-11-06 17:00", Prompt: "Remind me.", Thread: ThreadSame}},
		{name: "at in RFC 3339", req: Request{Name: "friday", At: "2026-11-06T17:00:00+01:00", Prompt: "Remind me."}},
		{name: "invalid name", req: Request{Name: "Morning Brief", Cron: "@daily", Prompt: "x"}, wantErr: true},
		{name: "no prompt", req: Request{Name: "x", Cron: "@daily"}, wantErr: true},
		{name: "neither cron nor at", req: Request{Name: "x", Prompt: "x"}, wantErr: true},
		{name: "both cron and at", req: Request{Name: "x", Cron: "@daily", At: "2026-11-06 17:00", Prompt: "x"}, wantErr: true},
		{name: "invalid cron", req: Request{Name: "x", Cron: "61 * * * *", Prompt: "x"}, wantErr: true},
		{name: "time zone in cron", req: Request{Name: "x", Cron: "CRON_TZ=UTC 0 7 * * *", Prompt: "x"}, wantErr: true},
		{name: "invalid at", req: Request{Name: "x", At: "friday at 5", Prompt: "x"}, wantErr: true},
		{name: "unknown time zone", req: Request{Name: "x", Cron: "@daily", TZ: "Mars/Olympus", Prompt: "x"}, wantErr: true},
		{name: "every minute", req: Request{Name: "x", Cron: "@every 1m", Prompt: "x"}},
		{name: "every 90 seconds", req: Request{Name: "x", Cron: "@every 90s", Prompt: "x"}},
		{name: "every zero", req: Request{Name: "x", Cron: "@every 0s", Prompt: "x"}, wantErr: true},
		{name: "every second", req: Request{Name: "x", Cron: "@every 1s", Prompt: "x"}, wantErr: true},
		{name: "every 59 seconds", req: Request{Name: "x", Cron: "@every 59s", Prompt: "x"}, wantErr: true},
		{name: "invalid thread", req: Request{Name: "x", Cron: "@daily", Prompt: "x", Thread: "old"}, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if err := tc.req.Validate(); (err != nil) != tc.wantErr {
				t.Fatalf("Request.Validate() error = %v, want error %v", err, tc.wantErr)
			}
		})
	}
}

func TestRequestWhenAtInTimeZone(t *testing.T) {
	t.Parallel()
	w, err := Request{Name: "x", At: "2026-11-06 17:00", TZ: "Europe/Zurich", Prompt: "x"}.When()
	if err != nil {
		t.Fatalf("Request.When() error = %v", err)
	}
	want := time.Date(2026, 11, 6, 16, 0, 0, 0, time.UTC)
	if !w.At.Equal(want) {
		t.Fatalf("Request.When().At = %v, want %v", w.At, want)
	}
}
