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
	"errors"
	"maps"
	"slices"
	"strconv"
	"testing"

	"github.com/bwmarrin/discordgo"
	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/channel"
	"github.com/a2aproject/a2a-serve/internal/channel/channeltest"
)

const botID = "99"

// fakeAPI serves a channel's history like Discord: the oldest messages after
// the given one, newest first.
type fakeAPI struct {
	channel *discordgo.Channel
	msgs    []*discordgo.Message
}

func (f *fakeAPI) ChannelMessages(channelID string, limit int, _, afterID, _ string, _ ...discordgo.RequestOption) ([]*discordgo.Message, error) {
	after, err := strconv.ParseUint(afterID, 10, 64)
	if err != nil {
		return nil, err
	}
	var page []*discordgo.Message
	for _, m := range f.msgs {
		id, err := strconv.ParseUint(m.ID, 10, 64)
		if err != nil {
			return nil, err
		}
		if m.ChannelID == channelID && id > after && len(page) < limit {
			page = append(page, m)
		}
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

			c.catchUp(tc.api, discordgo.NewState(), "c1")
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
