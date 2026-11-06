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

package messenger

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
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

// flakyChannel fails edits, the first rateLimited sends with a rate limit and
// then the next brokenSends with a network error.
type flakyChannel struct {
	*fakeChannel
	editErr     error
	rateLimited int
	brokenSends int
}

func (f *flakyChannel) Edit(context.Context, channel.Ref, channel.Outbound) error { return f.editErr }

func (f *flakyChannel) Send(ctx context.Context, m channel.Outbound) (channel.Ref, error) {
	f.mu.Lock()
	limited, broken := f.rateLimited > 0, f.rateLimited <= 0 && f.brokenSends > 0
	switch {
	case limited:
		f.rateLimited--
	case broken:
		f.brokenSends--
	}
	f.mu.Unlock()
	if limited {
		return channel.Ref{}, &channel.RateLimitError{RetryAfter: time.Millisecond, Err: errors.New("too many requests")}
	}
	if broken {
		return channel.Ref{}, errors.New("connection reset")
	}
	return f.fakeChannel.Send(ctx, m)
}

func TestRendererSendsFinalTextWhenFinalEditFails(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		editErr     error
		rateLimited int
		brokenSends int
		want        []string
	}{
		{name: "edit rejected", editErr: errors.New("message is too long"), want: []string{"partial", "partial and the end"}},
		{name: "edit rate limited", editErr: &channel.RateLimitError{RetryAfter: time.Millisecond, Err: errors.New("429")}, want: []string{"partial", "partial and the end"}},
		{name: "send rate limited once", editErr: errors.New("message to edit not found"), rateLimited: 1, want: []string{"partial", "partial and the end"}},
		{name: "send rate limited twice", editErr: errors.New("message to edit not found"), rateLimited: 2, want: []string{"partial", "partial and the end"}},
		{name: "send fails with a network error twice", editErr: errors.New("message to edit not found"), brokenSends: 2, want: []string{"partial", "partial and the end"}},
		{name: "send keeps failing", editErr: errors.New("message to edit not found"), brokenSends: 100, want: []string{"partial"}},
		{name: "edit succeeds", want: []string{"partial"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			ch := &flakyChannel{fakeChannel: newFakeChannel("fake")}
			a := &ChatApprovals{config: func() *agentdir.Config { return &agentdir.Config{Name: "coder"} }, timings: Timings{SendRetry: time.Millisecond}}
			rd := newRenderer(a, ch, "conv", channel.Ref{}, auth.Principal{})
			rd.onArtifact(&a2a.Artifact{ID: "a", Parts: a2a.ContentParts{a2a.NewTextPart("partial")}}, false)
			rd.flush(ctx, true)
			ch.editErr, ch.rateLimited, ch.brokenSends = tc.editErr, tc.rateLimited, tc.brokenSends
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

// editLimitedChannel answers the first edit with a rate limit and records
// when edits happen.
type editLimitedChannel struct {
	*fakeChannel
	retryAfter time.Duration

	editMu sync.Mutex
	edits  []time.Time
}

func (c *editLimitedChannel) Edit(ctx context.Context, ref channel.Ref, m channel.Outbound) error {
	c.editMu.Lock()
	c.edits = append(c.edits, time.Now())
	first := len(c.edits) == 1
	c.editMu.Unlock()
	if first {
		return &channel.RateLimitError{RetryAfter: c.retryAfter, Err: errors.New("too many requests")}
	}
	return c.fakeChannel.Edit(ctx, ref, m)
}

func (c *editLimitedChannel) editTimes() []time.Time {
	c.editMu.Lock()
	defer c.editMu.Unlock()
	return slices.Clone(c.edits)
}

func TestRendererHoldsEditsUntilRateLimitEnds(t *testing.T) {
	t.Parallel()
	const retryAfter = 300 * time.Millisecond
	ctx := t.Context()
	ch := &editLimitedChannel{fakeChannel: newFakeChannel("fake"), retryAfter: retryAfter}
	a := &ChatApprovals{timings: Timings{EditInterval: time.Millisecond}}
	rd := newRenderer(a, ch, "conv", channel.Ref{}, auth.Principal{})
	write := func(text string) {
		rd.onArtifact(&a2a.Artifact{ID: "a", Parts: a2a.ContentParts{a2a.NewTextPart(text)}}, true)
		rd.flush(ctx, false)
	}
	write("one")
	time.Sleep(10 * time.Millisecond)
	write(" two")
	time.Sleep(10 * time.Millisecond)
	write(" three")

	deadline := time.Now().Add(waitTimeout)
	for len(ch.editTimes()) < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}

	edits := ch.editTimes()
	if len(edits) < 2 {
		t.Fatalf("renderer edited %d times, want at least 2", len(edits))
	}
	if gap := edits[1].Sub(edits[0]); gap < retryAfter {
		t.Fatalf("renderer edited %v after the rate limit, want at least %v", gap, retryAfter)
	}
}

// gatedChannel blocks edits until release is closed.
type gatedChannel struct {
	*fakeChannel
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (c *gatedChannel) Edit(ctx context.Context, ref channel.Ref, m channel.Outbound) error {
	c.once.Do(func() { close(c.entered) })
	select {
	case <-c.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	return c.fakeChannel.Edit(ctx, ref, m)
}

func TestRendererConsumesEventsWhileMessengerCallIsPending(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	ch := &gatedChannel{fakeChannel: newFakeChannel("fake"), entered: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(func() { close(ch.release) })
	a := &ChatApprovals{timings: Timings{EditInterval: time.Millisecond}}
	rd := newRenderer(a, ch, "conv", channel.Ref{}, auth.Principal{})
	rd.onArtifact(&a2a.Artifact{ID: "a", Parts: a2a.ContentParts{a2a.NewTextPart("one")}}, false)
	rd.flush(ctx, true)
	rd.onArtifact(&a2a.Artifact{ID: "a", Parts: a2a.ContentParts{a2a.NewTextPart(" two")}}, true)
	time.Sleep(10 * time.Millisecond)
	go rd.flush(ctx, false)
	select {
	case <-ch.entered:
	case <-time.After(waitTimeout):
		t.Fatal("renderer did not edit the message")
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		rd.onEvent(ctx, a2a.NewArtifactEvent(&a2a.Task{ID: "t1", ContextID: "c1"}, a2a.NewTextPart(" three")))
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("renderer.onEvent() blocked on a pending messenger call")
	}
}

func TestRendererShowsApprovalTextLiterally(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		text string
		want string
	}{
		{
			name: "markup",
			text: "Permission required: Run [rm -rf /](https://example.com/ok).",
			want: "Permission required: Run [rm -rf /](https://example.com/ok).",
		},
		{
			name: "bidi override",
			text: "Run rm -rf ~ #\u202Ehs.tset",
			want: "⚠ This request has hidden formatting characters, shown as [U+XXXX].\nRun rm -rf ~ #[U+202E]hs.tset",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ch := newFakeChannel("fake")
			fx := newChatFixture(t, "", ch)
			fx.putTask(t, "t1", "c1", a2a.TaskStateWorking)
			rd := newRenderer(fx.chat, ch, "dm1", channel.Ref{}, auth.Owner())
			rd.setTask("t1", "c1")
			msg := permissionMessage(t)
			msg.Parts[0] = a2a.NewTextPart(tc.text)

			rd.onStatus(fx.ctx, a2a.TaskStatus{State: a2a.TaskStateInputRequired, Message: msg})

			got := ch.transcript()
			if len(got) != 1 {
				t.Fatalf("renderer.onStatus() sent %d messages, want 1", len(got))
			}
			if got[0].Text != tc.want || !got[0].Literal {
				t.Fatalf("renderer.onStatus() sent text %q, literal = %v, want %q, true", got[0].Text, got[0].Literal, tc.want)
			}
		})
	}
}

func TestRendererRetriesQuestion(t *testing.T) {
	t.Parallel()
	ch := &flakyChannel{fakeChannel: newFakeChannel("fake"), rateLimited: 1, brokenSends: 1}
	fx := newChatFixture(t, "", ch)
	fx.putTask(t, "t1", "c1", a2a.TaskStateWorking)
	rd := newRenderer(fx.chat, ch, "dm1", channel.Ref{}, auth.Owner())
	rd.setTask("t1", "c1")

	rd.onStatus(fx.ctx, a2a.TaskStatus{State: a2a.TaskStateInputRequired, Message: permissionMessage(t)})

	got := ch.transcript()
	if len(got) != 1 || got[0].Question == nil {
		t.Fatalf("renderer.onStatus() sent %+v, want the question once", got)
	}
}
