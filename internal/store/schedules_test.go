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

package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

func TestSchedules(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	ctx := context.Background()
	next := time.Date(2026, 11, 9, 6, 0, 0, 0, time.UTC)
	brief := Schedule{
		Name: "brief", Cron: "0 7 * * 1-5", TZ: "Europe/Zurich", Prompt: "Summarize today's meetings.",
		UserID: "user:owner", DeliverChannel: "telegram", DeliverUser: "user:owner", Conversation: "new",
		CreatedBy: "user:owner", NextRun: next,
	}
	remind := Schedule{
		Name: "remind", At: next.Add(time.Hour), Prompt: "Remind me.", UserID: "pair:anna",
		Conversation: "same", ContextID: "ctx1", CreatedBy: "pair:anna", NextRun: next.Add(time.Hour),
	}
	for _, sc := range []Schedule{remind, brief} {
		if err := s.AddSchedule(ctx, sc); err != nil {
			t.Fatalf("AddSchedule(%s) error = %v", sc.Name, err)
		}
	}
	if err := s.AddSchedule(ctx, brief); !errors.Is(err, ErrExists) {
		t.Fatalf("AddSchedule() of an existing name error = %v, want %v", err, ErrExists)
	}

	ran := next.Add(time.Minute)
	if err := s.RecordScheduleRun(ctx, "brief", ran, next.Add(24*time.Hour), "no chat"); err != nil {
		t.Fatalf("RecordScheduleRun() error = %v", err)
	}
	got, err := s.Schedules(ctx)
	if err != nil {
		t.Fatalf("Schedules() error = %v", err)
	}
	brief.LastRun, brief.NextRun, brief.LastError = ran, next.Add(24*time.Hour), "no chat"
	ignoreCreated := cmpopts.IgnoreFields(Schedule{}, "Created")
	if diff := cmp.Diff([]Schedule{brief, remind}, got, ignoreCreated, cmpopts.EquateApproxTime(0)); diff != "" {
		t.Fatalf("Schedules() wrong result (-want +got) diff = %s", diff)
	}

	if err := s.DeleteSchedule(ctx, "remind"); err != nil {
		t.Fatalf("DeleteSchedule() error = %v", err)
	}
	if err := s.DeleteSchedule(ctx, "remind"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("DeleteSchedule() of a removed schedule error = %v, want %v", err, ErrNotFound)
	}
	if _, err := s.Schedule(ctx, "remind"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Schedule() of a removed schedule error = %v, want %v", err, ErrNotFound)
	}
}
