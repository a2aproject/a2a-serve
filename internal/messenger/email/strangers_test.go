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

package email

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/messenger/messengertest"
)

func TestMessengerAllowStrangerReply(t *testing.T) {
	t.Parallel()
	c := New(Config{Address: "agent@example.com"}, messengertest.NewKV(nil))

	got := []bool{c.AllowStrangerReply("a@example.com"), c.AllowStrangerReply("a@example.com")}

	if diff := cmp.Diff([]bool{true, false}, got); diff != "" {
		t.Fatalf("Messenger.AllowStrangerReply() wrong result (-want +got) diff = %s", diff)
	}
}

func TestStrangerReplies(t *testing.T) {
	t.Parallel()

	type reply struct {
		after    time.Duration
		identity string
	}
	strangers := func(n int) []reply {
		var replies []reply
		for i := range n {
			replies = append(replies, reply{identity: fmt.Sprintf("stranger%d@example.com", i)})
		}
		return replies
	}
	tests := []struct {
		name    string
		replies []reply
		want    []bool
	}{
		{
			name:    "first mail of a sender is answered",
			replies: []reply{{identity: "a@example.com"}},
			want:    []bool{true},
		},
		{
			name: "sender gets one reply an hour",
			replies: []reply{
				{identity: "a@example.com"},
				{after: 59 * time.Minute, identity: "a@example.com"},
				{after: time.Minute, identity: "a@example.com"},
			},
			want: []bool{true, false, true},
		},
		{
			name: "sender address is compared without case",
			replies: []reply{
				{identity: "a@example.com"},
				{identity: "A@Example.COM"},
			},
			want: []bool{true, false},
		},
		{
			name: "other senders are answered",
			replies: []reply{
				{identity: "a@example.com"},
				{identity: "b@example.com"},
			},
			want: []bool{true, true},
		},
		{
			name:    "twenty replies an hour in all",
			replies: append(strangers(20), reply{identity: "late@example.com"}, reply{after: time.Hour, identity: "late@example.com"}),
			want:    append(slices.Repeat([]bool{true}, 20), false, true),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
			s := newStrangerReplies(func() time.Time { return now })

			var got []bool
			for _, r := range tc.replies {
				now = now.Add(r.after)
				got = append(got, s.allow(r.identity))
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Fatalf("strangerReplies.allow() wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestStrangerRepliesForgetsExpiredSenders(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	s := newStrangerReplies(func() time.Time { return now })
	for i := range strangerRepliesPerWindow {
		s.allow(fmt.Sprintf("stranger%d@example.com", i))
	}

	now = now.Add(strangerReplyWindow)
	s.allow("next@example.com")

	if got := s.senders(); got != 1 {
		t.Fatalf("strangerReplies.senders() = %d, want 1", got)
	}
}
