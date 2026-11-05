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
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-serve/internal/channel"
)

// fakeChannel records what the router sends and lets tests send messages.
type fakeChannel struct {
	name    string
	buttons bool
	maxText int
	// failures is how many times Run fails before it connects.
	failures int
	// sendDelay is how long Send returns after the message is visible.
	sendDelay time.Duration

	mu    sync.Mutex
	in    chan<- channel.Inbound
	msgs  map[string]channel.Outbound
	order []string
	ready chan struct{}
}

func newFakeChannel(name string) *fakeChannel {
	return &fakeChannel{name: name, buttons: true, msgs: map[string]channel.Outbound{}, ready: make(chan struct{})}
}

func (f *fakeChannel) Name() string { return f.name }

func (f *fakeChannel) Caps() channel.Caps {
	maxText := f.maxText
	if maxText == 0 {
		maxText = 4000
	}
	return channel.Caps{Edit: true, Buttons: f.buttons, MaxText: maxText, Stream: true}
}

func (f *fakeChannel) Describe() string { return "@fake_bot" }

func (f *fakeChannel) LinkHint(code string) string { return "/start " + code }

func (f *fakeChannel) Run(ctx context.Context, in chan<- channel.Inbound, _ func(error)) error {
	f.mu.Lock()
	if f.failures > 0 {
		f.failures--
		f.mu.Unlock()
		return errors.New("messenger unreachable")
	}
	f.in = in
	f.mu.Unlock()
	close(f.ready)
	<-ctx.Done()
	return nil
}

func (f *fakeChannel) Send(_ context.Context, m channel.Outbound) (channel.Ref, error) {
	f.mu.Lock()
	msgSeq.Lock()
	msgSeq.n++
	id := strconv.Itoa(msgSeq.n)
	msgSeq.Unlock()
	f.msgs[id] = m
	f.order = append(f.order, id)
	f.mu.Unlock()
	time.Sleep(f.sendDelay)
	return channel.Ref{Conv: m.Conv, ID: id}, nil
}

func (f *fakeChannel) Edit(_ context.Context, ref channel.Ref, m channel.Outbound) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.msgs[ref.ID] = m
	return nil
}

var msgSeq struct {
	sync.Mutex
	n int
}

// write delivers a message from identity in conversation conv.
func (f *fakeChannel) write(identity, conv, text string) {
	f.deliver(f.message(identity, conv, text))
}

func (f *fakeChannel) message(identity, conv, text string) channel.Inbound {
	msgSeq.Lock()
	msgSeq.n++
	id := strconv.Itoa(msgSeq.n)
	msgSeq.Unlock()
	return channel.Inbound{Channel: f.name, MessageID: "m" + id, Identity: identity, Name: identity, Conv: channel.ConvKey(conv), Direct: true, Text: text, Authentic: true}
}

func (f *fakeChannel) deliver(in channel.Inbound) {
	<-f.ready
	f.in <- in
}

// waitReady waits until the router runs the channel.
func (f *fakeChannel) waitReady(t *testing.T) {
	t.Helper()
	select {
	case <-f.ready:
	case <-time.After(10 * time.Second):
		t.Fatal("channel did not start")
	}
}

// click presses option of a question message.
func (f *fakeChannel) click(identity, conv string, ref string, q *channel.Question, option int) {
	<-f.ready
	msgSeq.Lock()
	msgSeq.n++
	id := strconv.Itoa(msgSeq.n)
	msgSeq.Unlock()
	f.in <- channel.Inbound{Channel: f.name, MessageID: "cb" + id, Identity: identity, Conv: channel.ConvKey(conv), Direct: true,
		Action: &channel.Action{QuestionID: q.ID, Option: option}, Authentic: true, ReplyTo: channel.Ref{Conv: channel.ConvKey(conv), ID: ref}}
}

// waitFor waits until a message in conv satisfies pred and returns its ID.
func (f *fakeChannel) waitFor(t *testing.T, conv string, pred func(channel.Outbound) bool) (string, channel.Outbound) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		for _, id := range f.order {
			m := f.msgs[id]
			if string(m.Conv) == conv && pred(m) {
				f.mu.Unlock()
				return id, m
			}
		}
		f.mu.Unlock()
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no matching message in %s; messages:\n%s", conv, f.dump())
	return "", channel.Outbound{}
}

func (f *fakeChannel) dump() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var all []string
	for _, id := range slices.Sorted(maps.Keys(f.msgs)) {
		all = append(all, fmt.Sprintf("%s[%s]: %q q=%v", id, f.msgs[id].Conv, f.msgs[id].Text, f.msgs[id].Question))
	}
	return strings.Join(all, "\n")
}

func hasText(s string) func(channel.Outbound) bool {
	return func(m channel.Outbound) bool { return strings.Contains(m.Text, s) }
}

func hasQuestion(m channel.Outbound) bool { return m.Question != nil }
