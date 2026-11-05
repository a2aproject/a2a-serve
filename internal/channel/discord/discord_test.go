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

package discord

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/channel"
	"github.com/a2aproject/a2a-serve/internal/channel/channeltest"
)

const botID = "99"

// fakeAPI serves a channel's history like Discord: the oldest messages after
// afterID, or the newest ones before beforeID, newest first.
type fakeAPI struct {
	channel *discordgo.Channel
	msgs    []*discordgo.Message
	// failBefore, if set, fails requests for messages before it.
	failBefore string
	// commandsCalled, if set, is closed when commands are registered, which
	// waits for commandsRelease if it is set.
	commandsCalled  chan struct{}
	commandsRelease chan struct{}
}

func (f *fakeAPI) ChannelMessages(channelID string, limit int, beforeID, afterID, _ string, _ ...discordgo.RequestOption) ([]*discordgo.Message, error) {
	if f.failBefore != "" && beforeID != "" && compareSnowflakes(beforeID, f.failBefore) <= 0 {
		return nil, errors.New("discord unreachable")
	}
	var page []*discordgo.Message
	for _, m := range f.msgs {
		if m.ChannelID != channelID {
			continue
		}
		if afterID != "" && compareSnowflakes(m.ID, afterID) <= 0 {
			continue
		}
		if beforeID != "" && compareSnowflakes(m.ID, beforeID) >= 0 {
			continue
		}
		page = append(page, m)
	}
	if afterID != "" {
		page = page[:min(limit, len(page))]
	} else {
		page = page[max(len(page)-limit, 0):]
	}
	slices.Reverse(page)
	return page, nil
}

func (f *fakeAPI) Channel(string, ...discordgo.RequestOption) (*discordgo.Channel, error) {
	if f.channel == nil {
		return nil, errors.New("unknown channel")
	}
	return f.channel, nil
}

func (f *fakeAPI) MessageThreadStartComplex(string, string, *discordgo.ThreadStart, ...discordgo.RequestOption) (*discordgo.Channel, error) {
	return nil, errors.New("not implemented")
}

func (f *fakeAPI) ApplicationCommandBulkOverwrite(string, string, []*discordgo.ApplicationCommand, ...discordgo.RequestOption) ([]*discordgo.ApplicationCommand, error) {
	if f.commandsCalled != nil {
		close(f.commandsCalled)
	}
	if f.commandsRelease != nil {
		<-f.commandsRelease
	}
	return nil, nil
}

func message(id int, mention bool) *discordgo.Message {
	m := &discordgo.Message{ID: strconv.Itoa(id), ChannelID: "c1", Content: "hi " + strconv.Itoa(id), Author: &discordgo.User{ID: "42", Username: "anna"}}
	if mention {
		m.Content = "<@" + botID + "> " + m.Content
		m.Mentions = []*discordgo.User{{ID: botID}}
	}
	return m
}

type delivered struct {
	MessageID string
	Direct    bool
	Text      string
}

func TestCatchUp(t *testing.T) {
	t.Parallel()
	var many []*discordgo.Message
	var manyWant []delivered
	for i := 1001; i <= 1120; i++ {
		many = append(many, message(i, false))
		manyWant = append(manyWant, delivered{MessageID: strconv.Itoa(i), Direct: true, Text: "hi " + strconv.Itoa(i)})
	}
	var outage []*discordgo.Message
	var outageWant []delivered
	for i := 1001; i <= 3500; i++ {
		outage = append(outage, message(i, false))
		if i > 3500-maxCatchUpMessages {
			outageWant = append(outageWant, delivered{MessageID: strconv.Itoa(i), Direct: true, Text: "hi " + strconv.Itoa(i)})
		}
	}
	dm := &discordgo.Channel{ID: "c1", Type: discordgo.ChannelTypeDM}
	guild := &discordgo.Channel{ID: "c1", GuildID: "g1", Type: discordgo.ChannelTypeGuildText}
	tests := []struct {
		name     string
		api      *fakeAPI
		kv       map[string]string
		want     []delivered
		wantLast string
	}{
		{
			name:     "direct messages over several pages",
			api:      &fakeAPI{channel: dm, msgs: many},
			want:     manyWant,
			wantLast: "1120",
		},
		{
			name:     "guild channel missing from the state",
			api:      &fakeAPI{channel: guild, msgs: []*discordgo.Message{message(1001, false), message(1002, true)}},
			want:     []delivered{{MessageID: "1002", Direct: false, Text: "hi 1002"}},
			wantLast: "1002",
		},
		{
			name:     "unknown channel",
			api:      &fakeAPI{msgs: []*discordgo.Message{message(1001, false), message(1002, true)}},
			want:     []delivered{{MessageID: "1002", Direct: false, Text: "hi 1002"}},
			wantLast: "1002",
		},
		{
			name:     "own thread",
			api:      &fakeAPI{channel: guild, msgs: []*discordgo.Message{message(1001, false)}},
			kv:       map[string]string{threadKey("c1"): "1"},
			want:     []delivered{{MessageID: "1001", Direct: false, Text: "hi 1001"}},
			wantLast: "1001",
		},
		{
			name:     "cursor passes messages the bot ignored",
			api:      &fakeAPI{channel: guild, msgs: []*discordgo.Message{message(1001, true), message(1002, false)}},
			want:     []delivered{{MessageID: "1001", Direct: false, Text: "hi 1001"}},
			wantLast: "1002",
		},
		{
			name:     "outage with more messages than the limit reads the newest",
			api:      &fakeAPI{channel: dm, msgs: outage},
			want:     outageWant,
			wantLast: "3500",
		},
		{
			name:     "nothing missed",
			api:      &fakeAPI{channel: dm},
			wantLast: "1000",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			init := map[string]string{lastKey("c1"): "1000"}
			maps.Copy(init, tc.kv)
			kv := channeltest.NewKV(init)
			in := make(chan channel.Inbound, len(tc.api.msgs))
			c := New("token", kv)
			c.me = &discordgo.User{ID: botID}
			c.in, c.ctx = in, t.Context()

			c.catchUp(tc.api, discordgo.NewState(), "c1", "1000")
			close(in)

			var got []delivered
			for m := range in {
				got = append(got, delivered{MessageID: m.MessageID, Direct: m.Direct, Text: m.Text})
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Fatalf("catchUp() wrong messages (-want +got) diff = %s", diff)
			}
			if got := kv.All()[lastKey("c1")]; got != tc.wantLast {
				t.Fatalf("catchUp() last message = %q, want %q", got, tc.wantLast)
			}
		})
	}
}

func TestThreadName(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		text string
		want string
	}{
		{name: "mention removed", text: "<@" + botID + "> fix the test", want: "fix the test"},
		{name: "only a mention", text: "<@" + botID + ">", want: "conversation"},
		{name: "long text", text: string(make([]rune, 100)), want: string(make([]rune, 80))},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := threadName(tc.text, botID); got != tc.want {
				t.Fatalf("threadName(%q) = %q, want %q", tc.text, got, tc.want)
			}
		})
	}
}

func TestComponents(t *testing.T) {
	t.Parallel()
	got := components(&channel.Question{ID: "q1", Options: []string{"Allow", "Always", "Deny"}})
	want := []discordgo.MessageComponent{discordgo.ActionsRow{Components: []discordgo.MessageComponent{
		discordgo.Button{Label: "Allow", Style: discordgo.PrimaryButton, CustomID: "h:q1:0"},
		discordgo.Button{Label: "Always", Style: discordgo.SecondaryButton, CustomID: "h:q1:1"},
		discordgo.Button{Label: "Deny", Style: discordgo.DangerButton, CustomID: "h:q1:2"},
	}}}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("components() wrong result (-want +got) diff = %s", diff)
	}
}

func TestMissed(t *testing.T) {
	t.Parallel()
	history := func(n int) []*discordgo.Message {
		var msgs []*discordgo.Message
		for i := 1001; i < 1001+n; i++ {
			msgs = append(msgs, message(i, false))
		}
		return msgs
	}
	tests := []struct {
		name        string
		api         *fakeAPI
		wantFirst   string
		wantLen     int
		wantSkipped int
		wantErr     bool
	}{
		{name: "fewer messages than the limit", api: &fakeAPI{msgs: history(120)}, wantFirst: "1001", wantLen: 120},
		{name: "exactly the limit", api: &fakeAPI{msgs: history(maxCatchUpMessages)}, wantFirst: "1001", wantLen: maxCatchUpMessages},
		{name: "a few over the limit", api: &fakeAPI{msgs: history(maxCatchUpMessages + 10)}, wantFirst: "1011", wantLen: maxCatchUpMessages, wantSkipped: 10},
		{name: "far over the limit", api: &fakeAPI{msgs: history(5 * maxCatchUpMessages)}, wantFirst: strconv.Itoa(1001 + 4*maxCatchUpMessages), wantLen: maxCatchUpMessages, wantSkipped: catchUpLimit},
		{name: "failure while reading", api: &fakeAPI{msgs: history(500), failBefore: "1200"}, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, skipped, err := missed(tc.api, "c1", "1000")

			if (err != nil) != tc.wantErr {
				t.Fatalf("missed() error = %v, want error: %v", err, tc.wantErr)
			}
			if len(got) != tc.wantLen {
				t.Fatalf("missed() returned %d messages, want %d", len(got), tc.wantLen)
			}
			if tc.wantLen > 0 && got[0].ID != tc.wantFirst {
				t.Fatalf("missed() first message = %s, want %s", got[0].ID, tc.wantFirst)
			}
			if !slices.IsSortedFunc(got, func(a, b *discordgo.Message) int { return compareSnowflakes(a.ID, b.ID) }) {
				t.Fatalf("missed() messages are not oldest first")
			}
			if skipped != tc.wantSkipped {
				t.Fatalf("missed() skipped = %d, want %d", skipped, tc.wantSkipped)
			}
		})
	}
}

func TestCursorOnlyAdvancesOnInboundMessages(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		init map[string]string
		do   func(c *Channel)
		want string
	}{
		{name: "message sent by the bot keeps the cursor", init: map[string]string{lastKey("c1"): "1000"}, do: func(c *Channel) { c.trackSent("c1", "1500") }, want: "1000"},
		{name: "message sent to a new conversation starts the cursor", do: func(c *Channel) { c.trackSent("c1", "1500") }, want: "1500"},
		{name: "inbound message moves the cursor", init: map[string]string{lastKey("c1"): "1000"}, do: func(c *Channel) { c.trackInbound("c1", "1100") }, want: "1100"},
		{name: "older inbound message keeps the cursor", init: map[string]string{lastKey("c1"): "1000"}, do: func(c *Channel) { c.trackInbound("c1", "999") }, want: "1000"},
		{name: "longer ID is newer", init: map[string]string{lastKey("c1"): "999"}, do: func(c *Channel) { c.trackInbound("c1", "1000") }, want: "1000"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			kv := channeltest.NewKV(tc.init)
			c := New("token", kv)

			tc.do(c)

			if got := kv.All()[lastKey("c1")]; got != tc.want {
				t.Fatalf("cursor = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestOnMessageKeepsArrivalOrderWithoutWaitingForTheRouter(t *testing.T) {
	t.Parallel()
	const n = 30
	c := New("token", channeltest.NewKV(nil))
	c.me = &discordgo.User{ID: botID}
	in := make(chan channel.Inbound)
	c.in, c.ctx = in, t.Context()

	returned := make(chan struct{})
	go func() {
		defer close(returned)
		for i := 1; i <= n; i++ {
			c.onMessage(nil, &discordgo.MessageCreate{Message: message(i, false)})
		}
	}()

	select {
	case <-returned:
	case <-time.After(10 * time.Second):
		t.Fatal("onMessage() blocked until the router read the message")
	}
	var got []string
	for range n {
		select {
		case m := <-in:
			got = append(got, m.MessageID)
		case <-time.After(10 * time.Second):
			t.Fatalf("delivered %d messages, want %d", len(got), n)
		}
	}
	var want []string
	for i := 1; i <= n; i++ {
		want = append(want, strconv.Itoa(i))
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("onMessage() delivered wrong order (-want +got) diff = %s", diff)
	}
}

func TestReadyCatchesUpBeforeLiveMessagesMoveTheCursor(t *testing.T) {
	t.Parallel()
	api := &fakeAPI{
		channel:         &discordgo.Channel{ID: "c1", Type: discordgo.ChannelTypeDM},
		msgs:            []*discordgo.Message{message(1001, false), message(1002, false)},
		commandsCalled:  make(chan struct{}),
		commandsRelease: make(chan struct{}),
	}
	kv := channeltest.NewKV(map[string]string{lastKey("c1"): "1000"})
	c := New("token", kv)
	c.me = &discordgo.User{ID: botID}
	in := make(chan channel.Inbound, 10)
	c.in, c.ctx, c.convs = in, t.Context(), []string{"c1"}

	go c.ready(api, discordgo.NewState(), botID)
	<-api.commandsCalled
	c.onMessage(nil, &discordgo.MessageCreate{Message: message(1500, false)})
	close(api.commandsRelease)

	var got []string
	for len(got) < 3 {
		select {
		case m := <-in:
			got = append(got, m.MessageID)
		case <-time.After(10 * time.Second):
			t.Fatalf("delivered %v, want 3 messages", got)
		}
	}
	if diff := cmp.Diff([]string{"1001", "1002", "1500"}, got); diff != "" {
		t.Fatalf("ready() delivered wrong messages (-want +got) diff = %s", diff)
	}
}

func TestCursorStaysWhenDeliveryIsCutOff(t *testing.T) {
	t.Parallel()
	kv := channeltest.NewKV(map[string]string{lastKey("c1"): "1000"})
	c := New("token", kv)
	c.me = &discordgo.User{ID: botID}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	c.in, c.ctx = make(chan channel.Inbound), ctx

	c.handleMessage(nil, message(1001, false), true)

	if got := kv.All()[lastKey("c1")]; got != "1000" {
		t.Fatalf("cursor = %q, want 1000", got)
	}
}

func TestRememberDropsRowsOfEvictedConversations(t *testing.T) {
	t.Parallel()
	kv := channeltest.NewKV(nil)
	c := New("token", kv)
	for i := range maxConvs {
		conv := "c" + strconv.Itoa(i)
		c.trackInbound(conv, "1000")
		if err := kv.Set(t.Context(), threadKey(conv), "1"); err != nil {
			t.Fatalf("KV.Set() error = %v", err)
		}
	}
	c.trackInbound("c0", "1001")

	c.trackInbound("new", "1000")

	got := kv.All()
	for _, key := range []string{lastKey("c1"), threadKey("c1")} {
		if _, ok := got[key]; ok {
			t.Errorf("%s is kept after c1 was evicted, want it removed", key)
		}
	}
	for _, key := range []string{lastKey("c0"), threadKey("c0"), lastKey("c2"), lastKey("new")} {
		if _, ok := got[key]; !ok {
			t.Errorf("%s is removed, want it kept", key)
		}
	}
	convs := strings.Split(got[convsKey], ",")
	if len(convs) != maxConvs || slices.Contains(convs, "c1") {
		t.Errorf("conversations = %d without c1: %v, want %d without c1", len(convs), !slices.Contains(convs, "c1"), maxConvs)
	}
}

func TestConnectionEventsReportState(t *testing.T) {
	t.Parallel()
	c := New("token", channeltest.NewKV(nil))
	var got []bool
	c.report = func(err error) { got = append(got, err == nil) }

	c.onDisconnect(nil, &discordgo.Disconnect{})
	c.onConnect(nil, &discordgo.Connect{})

	if diff := cmp.Diff([]bool{false, true}, got); diff != "" {
		t.Fatalf("reported connected wrong result (-want +got) diff = %s", diff)
	}
}

func TestOrderedQueueRunsConversationsIndependently(t *testing.T) {
	t.Parallel()
	q := newOrderedQueue()
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	q.do("a", func() { <-release })
	done := make(chan struct{})

	q.do("b", func() { close(done) })

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("a stalled conversation held up another one")
	}
}

// failingStarter fails to start threads the first failures times.
type failingStarter struct {
	failures int32
	calls    atomic.Int32
}

func (f *failingStarter) MessageThreadStartComplex(string, string, *discordgo.ThreadStart, ...discordgo.RequestOption) (*discordgo.Channel, error) {
	if f.calls.Add(1) <= f.failures {
		return nil, errors.New("missing access")
	}
	return &discordgo.Channel{ID: "th1"}, nil
}

func TestPrepareThreadStart(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		failures int32
		wantConv channel.ConvKey
		wantDrop bool
	}{
		{name: "thread starts", wantConv: "th1"},
		{name: "thread starts on the second try", failures: 1, wantConv: "th1"},
		{name: "thread fails to start", failures: 100, wantConv: "c1", wantDrop: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := New("token", channeltest.NewKV(nil))
			c.me = &discordgo.User{ID: botID}
			c.threadRetry = time.Millisecond
			in := make(chan channel.Inbound, 1)
			c.in, c.ctx = in, t.Context()
			c.handleMessage(&failingStarter{failures: tc.failures}, message(1001, true), false)
			m := <-in

			err := m.Prepare(t.Context(), &m)

			var dropped *channel.DroppedError
			if got := errors.As(err, &dropped); got != tc.wantDrop {
				t.Fatalf("Inbound.Prepare() error = %v, want a *channel.DroppedError: %v", err, tc.wantDrop)
			}
			if !tc.wantDrop && err != nil {
				t.Fatalf("Inbound.Prepare() error = %v, want nil", err)
			}
			if m.Conv != tc.wantConv {
				t.Fatalf("Inbound.Prepare() conversation = %q, want %q", m.Conv, tc.wantConv)
			}
		})
	}
}

func TestPrepareReportsFailedAttachments(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ok.txt" {
			if _, err := w.Write([]byte("hello")); err != nil {
				t.Errorf("writing the attachment: %v", err)
			}
			return
		}
		http.Error(w, "gone", http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	c := New("token", channeltest.NewKV(nil))
	c.me = &discordgo.User{ID: botID}
	in := make(chan channel.Inbound, 1)
	c.in, c.ctx = in, t.Context()
	msg := message(1001, false)
	msg.Attachments = []*discordgo.MessageAttachment{
		{Filename: "ok.txt", URL: srv.URL + "/ok.txt"},
		{Filename: "gone.txt", URL: srv.URL + "/gone.txt?hm=secret"},
	}
	c.handleMessage(nil, msg, true)
	m := <-in

	err := m.Prepare(t.Context(), &m)

	failed := channel.FailedAttachments(err)
	if len(failed) != 1 || failed[0].Name != "gone.txt" {
		t.Fatalf("Inbound.Prepare() error = %v, want one failed attachment, gone.txt", err)
	}
	if strings.Contains(err.Error(), "secret") {
		t.Fatalf("Inbound.Prepare() error = %v, want it without the URL query", err)
	}
	if len(m.Files) != 1 || m.Files[0].Name != "ok.txt" {
		t.Fatalf("Inbound.Prepare() files = %+v, want ok.txt", m.Files)
	}
}

func TestEscapeMarkdown(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		text string
		want string
	}{
		{name: "masked link", text: "Run [rm -rf /](https://example.com/ok)", want: `Run \[rm -rf /\]\(https://example.com/ok\)`},
		{name: "spoiler", text: "||hidden||", want: `\|\|hidden\|\|`},
		{name: "code and emphasis", text: "`a` *b* _c_ ~d~", want: "\\`a\\` \\*b\\* \\_c\\_ \\~d\\~"},
		{name: "plain words", text: "Run git push 42", want: "Run git push 42"},
		{name: "non-ASCII text", text: "Запуск ✓", want: "Запуск ✓"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			want := strings.ReplaceAll(tc.want, `\`, `\`)
			if got := escapeMarkdown(tc.text); got != want {
				t.Fatalf("escapeMarkdown(%q) = %q, want %q", tc.text, got, want)
			}
		})
	}
}

func TestLiteralPiecesFitTheLimitAndKeepTheText(t *testing.T) {
	t.Parallel()
	text := strings.Repeat("[x](y) ", 600)

	pieces := messagePieces(channel.Outbound{Text: text, Literal: true})

	var unescaped strings.Builder
	for _, p := range pieces {
		if len(p) > maxText {
			t.Fatalf("messagePieces() made a piece of %d bytes, want at most %d", len(p), maxText)
		}
		unescaped.WriteString(strings.NewReplacer(`\[`, "[", `\]`, "]", `\(`, "(", `\)`, ")").Replace(p))
	}
	if got := strings.Join(strings.Fields(unescaped.String()), " "); got != strings.Join(strings.Fields(text), " ") {
		t.Fatalf("messagePieces() pieces don't add up to the text")
	}
}
