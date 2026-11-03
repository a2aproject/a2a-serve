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
			got, ok, err := tc.when.Next(after)
			if err != nil {
				t.Fatalf("When.Next() error = %v", err)
			}
			if !got.Equal(tc.want) || ok != tc.wantOK {
				t.Fatalf("When.Next() = %v, %v, want %v, %v", got, ok, tc.want, tc.wantOK)
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
		{name: "at", req: Request{Name: "friday", At: "2026-11-06 17:00", Prompt: "Remind me.", Conversation: ConversationSame}},
		{name: "at in RFC 3339", req: Request{Name: "friday", At: "2026-11-06T17:00:00+01:00", Prompt: "Remind me."}},
		{name: "invalid name", req: Request{Name: "Morning Brief", Cron: "@daily", Prompt: "x"}, wantErr: true},
		{name: "no prompt", req: Request{Name: "x", Cron: "@daily"}, wantErr: true},
		{name: "neither cron nor at", req: Request{Name: "x", Prompt: "x"}, wantErr: true},
		{name: "both cron and at", req: Request{Name: "x", Cron: "@daily", At: "2026-11-06 17:00", Prompt: "x"}, wantErr: true},
		{name: "invalid cron", req: Request{Name: "x", Cron: "61 * * * *", Prompt: "x"}, wantErr: true},
		{name: "time zone in cron", req: Request{Name: "x", Cron: "CRON_TZ=UTC 0 7 * * *", Prompt: "x"}, wantErr: true},
		{name: "invalid at", req: Request{Name: "x", At: "friday at 5", Prompt: "x"}, wantErr: true},
		{name: "unknown time zone", req: Request{Name: "x", Cron: "@daily", TZ: "Mars/Olympus", Prompt: "x"}, wantErr: true},
		{name: "invalid conversation", req: Request{Name: "x", Cron: "@daily", Prompt: "x", Conversation: "old"}, wantErr: true},
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
