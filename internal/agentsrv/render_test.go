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

package agentsrv

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/channel"
)

func TestRendererSplitsStreamToChannelLimit(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		maxText int
		text    string
	}{
		{name: "default limit", maxText: 400, text: strings.Repeat("line of text\n", 100)},
		{name: "limit below the progress reserve", maxText: 150, text: strings.Repeat("line of text\n", 50)},
		{name: "one byte limit", maxText: 1, text: "hello\nworld"},
		{name: "rune wider than the limit", maxText: 3, text: "héllo wörld"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ch := newFakeChannel("fake")
			ch.maxText = tc.maxText
			rd := newRenderer(&ChatApprovals{}, ch, "conv", channel.Ref{}, auth.Principal{})
			done := make(chan struct{})
			go func() {
				defer close(done)
				rd.onArtifact(&a2a.Artifact{ID: "a", Parts: a2a.ContentParts{a2a.NewTextPart(tc.text)}}, false)
				rd.flush(context.Background(), true)
			}()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Fatalf("flush() did not return")
			}
			var got strings.Builder
			for _, id := range ch.order {
				text := ch.msgs[id].Text
				if len(text) > tc.maxText {
					t.Fatalf("flush() sent %d bytes, want at most %d", len(text), tc.maxText)
				}
				got.WriteString(text)
			}
			if got.String() != tc.text {
				t.Fatalf("flush() sent %q, want %q", got.String(), tc.text)
			}
		})
	}
}

// flakyChannel fails edits, and the first rateLimited sends with a rate limit.
type flakyChannel struct {
	*fakeChannel
	editErr     error
	rateLimited int
}

func (f *flakyChannel) Edit(context.Context, channel.Ref, channel.Outbound) error { return f.editErr }

func (f *flakyChannel) Send(ctx context.Context, m channel.Outbound) (channel.Ref, error) {
	f.mu.Lock()
	limited := f.rateLimited > 0
	if limited {
		f.rateLimited--
	}
	f.mu.Unlock()
	if limited {
		return channel.Ref{}, &channel.RateLimitError{RetryAfter: time.Millisecond, Err: errors.New("too many requests")}
	}
	return f.fakeChannel.Send(ctx, m)
}

func TestRendererSendsFinalTextWhenFinalEditFails(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		editErr     error
		rateLimited int
		want        []string
	}{
		{name: "edit rejected", editErr: errors.New("message is too long"), want: []string{"partial", "partial and the end"}},
		{name: "edit rate limited", editErr: &channel.RateLimitError{RetryAfter: time.Millisecond, Err: errors.New("429")}, want: []string{"partial", "partial and the end"}},
		{name: "send rate limited once", editErr: errors.New("message to edit not found"), rateLimited: 1, want: []string{"partial", "partial and the end"}},
		{name: "send rate limited twice", editErr: errors.New("message to edit not found"), rateLimited: 2, want: []string{"partial"}},
		{name: "edit succeeds", want: []string{"partial"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			ch := &flakyChannel{fakeChannel: newFakeChannel("fake")}
			rd := newRenderer(&ChatApprovals{agentName: func() string { return "coder" }}, ch, "conv", channel.Ref{}, auth.Principal{})
			rd.onArtifact(&a2a.Artifact{ID: "a", Parts: a2a.ContentParts{a2a.NewTextPart("partial")}}, false)
			rd.flush(ctx, true)
			ch.editErr, ch.rateLimited = tc.editErr, tc.rateLimited
			rd.onArtifact(&a2a.Artifact{ID: "a", Parts: a2a.ContentParts{a2a.NewTextPart(" and the end")}}, true)
			rd.final(ctx, a2a.TaskStatus{State: a2a.TaskStateCompleted})
			var got []string
			for _, id := range ch.order {
				got = append(got, ch.msgs[id].Text)
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Fatalf("renderer.final() sent wrong messages (-want +got) diff = %s", diff)
			}
		})
	}
}
