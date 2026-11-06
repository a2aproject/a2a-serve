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

package channeltest

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/a2aproject/a2a-serve/internal/channel"
)

// Channel is an in-memory [channel.Channel] that records what it sends and
// lets tests deliver messages.
type Channel struct {
	name string
	// Buttons is whether the channel supports buttons.
	Buttons bool
	// MaxText is the message length limit. Zero means 4000.
	MaxText int
	// Failures is how many times Run fails before it connects.
	Failures int
	// SendDelay is how long Send returns after the message is visible.
	SendDelay time.Duration

	mu    sync.Mutex
	in    chan<- channel.Inbound
	msgs  map[string]channel.Outbound
	order []string
	ready chan struct{}
}

var (
	_ channel.Channel   = (*Channel)(nil)
	_ channel.Describer = (*Channel)(nil)
)

// seq numbers the messages of all channels.
var seq atomic.Int64

func nextID() string { return strconv.FormatInt(seq.Add(1), 10) }

// NewChannel returns a Channel with buttons that is ready to run.
func NewChannel(name string) *Channel {
	return &Channel{name: name, Buttons: true, msgs: map[string]channel.Outbound{}, ready: make(chan struct{})}
}

// Name implements [channel.Channel].
func (c *Channel) Name() string { return c.name }

// Caps implements [channel.Channel].
func (c *Channel) Caps() channel.Caps {
	maxText := c.MaxText
	if maxText == 0 {
		maxText = 4000
	}
	return channel.Caps{Edit: true, Buttons: c.Buttons, MaxText: maxText, Stream: true}
}

// Describe implements [channel.Describer].
func (c *Channel) Describe() string { return "@fake_bot" }

// LinkHint implements [channel.Describer].
func (c *Channel) LinkHint(code string) string { return "/start " + code }

// Run implements [channel.Channel].
func (c *Channel) Run(ctx context.Context, in chan<- channel.Inbound, _ func(error)) error {
	c.mu.Lock()
	if c.Failures > 0 {
		c.Failures--
		c.mu.Unlock()
		return errors.New("messenger unreachable")
	}
	c.in = in
	c.mu.Unlock()
	close(c.ready)
	<-ctx.Done()
	return nil
}

// Send implements [channel.Channel].
func (c *Channel) Send(_ context.Context, m channel.Outbound) (channel.Ref, error) {
	c.mu.Lock()
	id := nextID()
	c.msgs[id] = m
	c.order = append(c.order, id)
	c.mu.Unlock()
	time.Sleep(c.SendDelay)
	return channel.Ref{Conv: m.Conv, ID: id}, nil
}

// Edit implements [channel.Channel].
func (c *Channel) Edit(_ context.Context, ref channel.Ref, m channel.Outbound) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.msgs[ref.ID] = m
	return nil
}

// Write delivers a message from identity in conversation conv.
func (c *Channel) Write(identity, conv, text string) {
	c.Deliver(c.Message(identity, conv, text))
}

// Message returns a direct message from identity in conversation conv
// without delivering it.
func (c *Channel) Message(identity, conv, text string) channel.Inbound {
	return channel.Inbound{Channel: c.name, MessageID: "m" + nextID(), Identity: identity, Name: identity, Conv: channel.ConvKey(conv), Direct: true, Text: text, Authentic: true}
}

// Deliver waits until the channel runs and delivers in.
func (c *Channel) Deliver(in channel.Inbound) {
	<-c.ready
	c.in <- in
}

// WaitReady waits until the channel runs.
func (c *Channel) WaitReady(t testing.TB) {
	t.Helper()
	select {
	case <-c.ready:
	case <-time.After(10 * time.Second):
		t.Fatal("channel did not start")
	}
}

// Click presses option of the question message ref.
func (c *Channel) Click(identity, conv string, ref string, q *channel.Question, option int) {
	<-c.ready
	c.in <- channel.Inbound{Channel: c.name, MessageID: "cb" + nextID(), Identity: identity, Conv: channel.ConvKey(conv), Direct: true,
		Action: &channel.Action{QuestionID: q.ID, Option: option}, Authentic: true, ReplyTo: channel.Ref{Conv: channel.ConvKey(conv), ID: ref}}
}

// WaitFor waits until a message in conv satisfies pred and returns its ID.
func (c *Channel) WaitFor(t testing.TB, conv string, pred func(channel.Outbound) bool) (string, channel.Outbound) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		for _, id := range c.order {
			m := c.msgs[id]
			if string(m.Conv) == conv && pred(m) {
				c.mu.Unlock()
				return id, m
			}
		}
		c.mu.Unlock()
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no matching message in %s; messages:\n%s", conv, c.Dump())
	return "", channel.Outbound{}
}

// WaitEdited waits until the message with ID id contains text. The message
// may have been sent by an earlier channel of the same agent.
func (c *Channel) WaitEdited(t testing.TB, id, text string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		c.mu.Lock()
		got := c.msgs[id].Text
		c.mu.Unlock()
		if strings.Contains(got, text) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("message %s = %q, want it to contain %q; messages:\n%s", id, got, text, c.Dump())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Questions counts the messages with buttons.
func (c *Channel) Questions() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, id := range c.order {
		if c.msgs[id].Question != nil {
			n++
		}
	}
	return n
}

// Transcript returns the messages sent in the order they were sent, as
// edits left them.
func (c *Channel) Transcript() []channel.Outbound {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]channel.Outbound, len(c.order))
	for i, id := range c.order {
		out[i] = c.msgs[id]
	}
	return out
}

// Dump formats the messages for failure output.
func (c *Channel) Dump() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var all []string
	for _, id := range slices.Sorted(maps.Keys(c.msgs)) {
		all = append(all, fmt.Sprintf("%s[%s]: %q q=%v", id, c.msgs[id].Conv, c.msgs[id].Text, c.msgs[id].Question))
	}
	return strings.Join(all, "\n")
}

// HasText matches messages that contain s.
func HasText(s string) func(channel.Outbound) bool {
	return func(m channel.Outbound) bool { return strings.Contains(m.Text, s) }
}

// HasQuestion matches messages with buttons.
func HasQuestion(m channel.Outbound) bool { return m.Question != nil }
