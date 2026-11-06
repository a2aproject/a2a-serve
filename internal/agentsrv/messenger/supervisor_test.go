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
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/channel"
	"github.com/a2aproject/a2a-serve/internal/channel/channeltest"
	"github.com/a2aproject/a2a-serve/internal/controlapi"
	"github.com/a2aproject/a2a-serve/internal/store"
)

func newTestSupervisor(t *testing.T, retryMin time.Duration, logf func(string, ...any)) *ChannelSupervisor {
	t.Helper()
	if logf == nil {
		logf = func(string, ...any) {}
	}
	ctx, cancel := context.WithCancel(context.Background())
	sv := NewChannelSupervisor(ctx, SupervisorDeps{Store: openTestStore(t), Logf: logf, RetryMin: retryMin})
	t.Cleanup(func() {
		cancel()
		stopCtx, stop := context.WithTimeout(context.Background(), waitTimeout)
		defer stop()
		if err := sv.StopAll(stopCtx); err != nil {
			t.Errorf("stopAll() error = %v", err)
		}
	})
	return sv
}

func channelNames(chs []channel.Channel) []string {
	var names []string
	for _, ch := range chs {
		names = append(names, ch.Name())
	}
	return names
}

func described(name string) *describedStub {
	return &describedStub{stubChannel{name: name, run: blockUntilDone}}
}

func plain(name string) *stubChannel { return &stubChannel{name: name, run: blockUntilDone} }

func TestChannelSupervisorDeliversInbound(t *testing.T) {
	t.Parallel()
	sv := newTestSupervisor(t, time.Millisecond, nil)
	ch := channeltest.NewChannel("fake")
	sv.Add(ch)
	ch.WaitReady(t)
	want := ch.Message("telegram:1", "dm1", "hello")

	ch.Deliver(want)

	select {
	case got := <-sv.inbound():
		if diff := cmp.Diff(want, got); diff != "" {
			t.Fatalf("sv.inbound() wrong result (-want +got) diff = %s", diff)
		}
	case <-time.After(waitTimeout):
		t.Fatal("sv.inbound() received nothing")
	}
}

func TestChannelSupervisorChannels(t *testing.T) {
	t.Parallel()
	sv := newTestSupervisor(t, time.Millisecond, nil)
	sv.Add(plain("telegram"))
	sv.Add(plain("discord"))
	sv.Add(plain("email"))

	if diff := cmp.Diff([]string{"discord", "email", "telegram"}, channelNames(sv.channels())); diff != "" {
		t.Fatalf("sv.channels() names wrong result (-want +got) diff = %s", diff)
	}
	if got := sv.Channel("email"); got == nil || got.Name() != "email" {
		t.Fatalf("sv.Channel(email) = %v, want the email channel", got)
	}
	if got := sv.Channel("sms"); got != nil {
		t.Fatalf("sv.Channel(sms) = %v, want nil", got)
	}
}

func TestChannelSupervisorRemoveStopsChannelBeforeReturning(t *testing.T) {
	t.Parallel()
	sv := newTestSupervisor(t, time.Millisecond, nil)
	started, stopped := make(chan struct{}), make(chan struct{})
	sv.Add(&stubChannel{name: "fake", run: func(ctx context.Context, _ chan<- channel.Inbound, _ func(error)) error {
		close(started)
		<-ctx.Done()
		close(stopped)
		return nil
	}})
	<-started

	sv.Remove("fake")

	select {
	case <-stopped:
	default:
		t.Fatal("sv.Remove() returned before Run ended")
	}
	if got := sv.Channel("fake"); got != nil {
		t.Fatalf("sv.Channel(fake) after remove = %v, want nil", got)
	}
	sv.Remove("fake")
}

func TestChannelSupervisorAddReplacesChannelAfterItStopped(t *testing.T) {
	t.Parallel()
	sv := newTestSupervisor(t, time.Millisecond, nil)
	var mu sync.Mutex
	var events []string
	record := func(event string) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, event)
	}
	snapshot := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(events)
	}
	newChannel := func(id string) *stubChannel {
		return &stubChannel{name: "fake", run: func(ctx context.Context, _ chan<- channel.Inbound, _ func(error)) error {
			record(id + " started")
			<-ctx.Done()
			record(id + " stopped")
			return nil
		}}
	}
	sv.Add(newChannel("first"))
	waitFor(t, "the first channel to start", func() bool { return len(snapshot()) == 1 })

	sv.Add(newChannel("second"))

	waitFor(t, "the second channel to start", func() bool { return len(snapshot()) == 3 })
	want := []string{"first started", "first stopped", "second started"}
	if diff := cmp.Diff(want, snapshot()); diff != "" {
		t.Fatalf("sv.Add() order of events wrong result (-want +got) diff = %s", diff)
	}
}

func TestChannelSupervisorStopAll(t *testing.T) {
	t.Parallel()
	sv := newTestSupervisor(t, time.Millisecond, nil)
	var running atomic.Int32
	newChannel := func(name string) *stubChannel {
		return &stubChannel{name: name, run: func(ctx context.Context, _ chan<- channel.Inbound, _ func(error)) error {
			running.Add(1)
			defer running.Add(-1)
			<-ctx.Done()
			return nil
		}}
	}
	for _, name := range []string{"a", "b", "c"} {
		sv.Add(newChannel(name))
	}
	waitFor(t, "the channels to start", func() bool { return running.Load() == 3 })

	if err := sv.StopAll(context.Background()); err != nil {
		t.Fatalf("sv.StopAll() error = %v", err)
	}

	if got := running.Load(); got != 0 {
		t.Errorf("running channels after sv.StopAll() = %d, want 0", got)
	}
	if got := sv.channels(); len(got) != 0 {
		t.Errorf("sv.channels() after sv.StopAll() = %v, want none", channelNames(got))
	}
}

func TestChannelSupervisorStopAllGivesUpWhenContextEnds(t *testing.T) {
	t.Parallel()
	sv := newTestSupervisor(t, time.Millisecond, nil)
	started, release := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() { close(release) })
	sv.Add(&stubChannel{name: "stuck", run: func(context.Context, chan<- channel.Inbound, func(error)) error {
		close(started)
		<-release
		return nil
	}})
	<-started
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := sv.StopAll(ctx)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("sv.StopAll() error = %v, want %v", err, context.Canceled)
	}
	if want := "waiting for stuck to stop"; !strings.Contains(err.Error(), want) {
		t.Fatalf("sv.StopAll() error = %q, want it to contain %q", err, want)
	}
}

func TestChannelSupervisorRestartsFailingChannel(t *testing.T) {
	t.Parallel()
	sv := newTestSupervisor(t, time.Millisecond, nil)
	const failures = 3
	var runs atomic.Int32
	connected := make(chan struct{})
	sv.Add(&stubChannel{name: "fake", run: func(ctx context.Context, _ chan<- channel.Inbound, _ func(error)) error {
		if runs.Add(1) <= failures {
			return errors.New("messenger unreachable")
		}
		close(connected)
		<-ctx.Done()
		return nil
	}})

	select {
	case <-connected:
	case <-time.After(waitTimeout):
		t.Fatalf("channel did not reconnect; Run() calls = %d", runs.Load())
	}

	if got := runs.Load(); got != failures+1 {
		t.Fatalf("Run() calls = %d, want %d", got, failures+1)
	}
}

func TestChannelSupervisorStatusReportsFailure(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		runErr  error
		wantErr string
	}{
		{name: "error of Run", runErr: errors.New("boom"), wantErr: "boom"},
		{name: "Run ended without an error", wantErr: "stopped unexpectedly"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sv := newTestSupervisor(t, time.Hour, nil)
			ch := described("fake")
			ch.run = func(context.Context, chan<- channel.Inbound, func(error)) error { return tc.runErr }
			sv.Add(ch)

			waitFor(t, "the failure to be reported", func() bool { return sv.Status()[0].Error != "" })

			want := []controlapi.ChannelStatus{{Name: "fake", Account: "@fake", Error: tc.wantErr}}
			if diff := cmp.Diff(want, sv.Status()); diff != "" {
				t.Fatalf("sv.Status() wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestChannelSupervisorStatusShowsConnectionStateReportedByRunningChannel(t *testing.T) {
	t.Parallel()
	sv := newTestSupervisor(t, time.Millisecond, nil)
	reports := make(chan error)
	ch := described("fake")
	ch.run = func(ctx context.Context, _ chan<- channel.Inbound, report func(error)) error {
		for {
			select {
			case <-ctx.Done():
				return nil
			case err := <-reports:
				report(err)
			}
		}
	}
	sv.Add(ch)

	reports <- errors.New("invalid bot token")
	waitFor(t, "the reported error", func() bool { return sv.Status()[0].Error != "" })
	want := []controlapi.ChannelStatus{{Name: "fake", Account: "@fake", Error: "invalid bot token"}}
	if diff := cmp.Diff(want, sv.Status()); diff != "" {
		t.Fatalf("sv.Status() wrong result (-want +got) diff = %s", diff)
	}

	reports <- nil
	waitFor(t, "the reported connection", func() bool { return sv.Status()[0].Error == "" })
}

func TestChannelSupervisorStatusClearsErrorWhenChannelRestarts(t *testing.T) {
	t.Parallel()
	sv := newTestSupervisor(t, time.Millisecond, nil)
	var runs atomic.Int32
	restarted := make(chan struct{})
	ch := described("fake")
	ch.run = func(ctx context.Context, _ chan<- channel.Inbound, _ func(error)) error {
		if runs.Add(1) == 1 {
			return errors.New("boom")
		}
		close(restarted)
		<-ctx.Done()
		return nil
	}
	sv.Add(ch)
	<-restarted

	want := []controlapi.ChannelStatus{{Name: "fake", Account: "@fake"}}
	if diff := cmp.Diff(want, sv.Status()); diff != "" {
		t.Fatalf("sv.Status() wrong result (-want +got) diff = %s", diff)
	}
}

func TestChannelSupervisorStatusListsChannelsByName(t *testing.T) {
	t.Parallel()
	sv := newTestSupervisor(t, time.Millisecond, nil)
	sv.Add(plain("telegram"))
	sv.Add(described("discord"))

	want := []controlapi.ChannelStatus{{Name: "discord", Account: "@discord"}, {Name: "telegram"}}
	if diff := cmp.Diff(want, sv.Status()); diff != "" {
		t.Fatalf("sv.Status() wrong result (-want +got) diff = %s", diff)
	}
}

func TestChannelSupervisorChannelStatusCarriesOwnerLink(t *testing.T) {
	t.Parallel()
	sv := newTestSupervisor(t, time.Millisecond, nil)
	link := &controlapi.OwnerLink{Hint: "/start ABCD2345", Code: "ABCD2345"}

	got := sv.ChannelStatus(described("fake"), link)

	want := &controlapi.ChannelStatus{Name: "fake", Account: "@fake", OwnerLink: link}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("sv.ChannelStatus() wrong result (-want +got) diff = %s", diff)
	}
}

func TestChannelSupervisorLinkHints(t *testing.T) {
	t.Parallel()
	sv := newTestSupervisor(t, time.Millisecond, nil)
	sv.Add(described("telegram"))
	sv.Add(plain("webhook"))
	sv.Add(described("discord"))

	got := sv.LinkHints("ABCD2345")

	want := []string{"discord: /start ABCD2345", "telegram: /start ABCD2345"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("sv.LinkHints() wrong result (-want +got) diff = %s", diff)
	}
}

func TestChannelSupervisorOwnerLink(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		channel  channel.Channel
		links    []linkSpec
		wantLink bool
	}{
		{name: "nobody is linked", channel: described("fake"), wantLink: true},
		{name: "owner is linked on the channel", channel: described("fake"), links: []linkSpec{{pairing: auth.LocalPairing, channel: "fake", identity: "telegram:1", conv: "dm1"}}},
		{name: "owner is linked on another channel", channel: described("fake"), links: []linkSpec{{pairing: auth.LocalPairing, channel: "other", identity: "telegram:1", conv: "dm1"}}, wantLink: true},
		{name: "member is linked on the channel", channel: described("fake"), links: []linkSpec{{pairing: "anna", role: auth.RoleAuthenticated, channel: "fake", identity: "telegram:2", conv: "dm2"}}, wantLink: true},
		{name: "channel can't tell how to link", channel: plain("fake")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sv := newTestSupervisor(t, time.Millisecond, nil)
			for _, l := range tc.links {
				putLink(t, sv.store, l)
			}

			got := sv.OwnerLink(context.Background(), tc.channel)

			if !tc.wantLink {
				if got != nil {
					t.Fatalf("sv.OwnerLink() = %+v, want nil", got)
				}
				return
			}
			if got == nil {
				t.Fatal("sv.OwnerLink() = nil, want a link")
			}
			if want := "/start " + got.Code; got.Hint != want {
				t.Errorf("sv.OwnerLink().Hint = %q, want %q", got.Hint, want)
			}
			if !got.Expires.After(time.Now()) {
				t.Errorf("sv.OwnerLink().Expires = %v, want a time in the future", got.Expires)
			}
			code, err := sv.store.TakeCode(context.Background(), auth.NormalizeCode(got.Code), store.AnyCode)
			if err != nil {
				t.Fatalf("store.TakeCode(%s) error = %v, want a pending code", got.Code, err)
			}
			if code.Pairing != auth.LocalPairing {
				t.Fatalf("store.TakeCode(%s).Pairing = %q, want %q", got.Code, code.Pairing, auth.LocalPairing)
			}
		})
	}
}

func TestChannelSupervisorOwnerLinkReusesMintedCode(t *testing.T) {
	t.Parallel()
	sv := newTestSupervisor(t, time.Millisecond, nil)
	var minted mintedCode

	first := sv.ownerLinkWith(context.Background(), described("a"), &minted)
	second := sv.ownerLinkWith(context.Background(), described("b"), &minted)

	if first == nil || second == nil {
		t.Fatalf("sv.ownerLinkWith() = %+v, %+v, want links", first, second)
	}
	if diff := cmp.Diff(first, second); diff != "" {
		t.Fatalf("sv.ownerLinkWith() of the second channel wrong result (-want +got) diff = %s", diff)
	}
}

func TestChannelSupervisorOwnerLinkReplacesPendingCode(t *testing.T) {
	t.Parallel()
	sv := newTestSupervisor(t, time.Millisecond, nil)
	ctx := context.Background()
	first := sv.OwnerLink(ctx, described("fake"))
	second := sv.OwnerLink(ctx, described("fake"))

	if first == nil || second == nil {
		t.Fatalf("sv.OwnerLink() = %+v, %+v, want links", first, second)
	}
	if first.Code == second.Code {
		t.Fatalf("sv.OwnerLink() codes = %q, %q, want a new code", first.Code, second.Code)
	}
	if _, err := sv.store.TakeCode(ctx, auth.NormalizeCode(first.Code), store.AnyCode); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("store.TakeCode(first) error = %v, want %v", err, store.ErrNotFound)
	}
	if _, err := sv.store.TakeCode(ctx, auth.NormalizeCode(second.Code), store.AnyCode); err != nil {
		t.Fatalf("store.TakeCode(second) error = %v, want nil", err)
	}
}

func TestChannelSupervisorAnnounce(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var lines []string
	logf := func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, fmt.Sprintf(format, args...))
	}
	sv := newTestSupervisor(t, time.Millisecond, logf)
	putLink(t, sv.store, linkSpec{pairing: auth.LocalPairing, channel: "b", identity: "telegram:1", conv: "dm1"})
	for _, ch := range []channel.Channel{described("a"), described("b"), plain("c"), described("d")} {
		sv.Add(ch)
	}

	if err := sv.Announce(context.Background()); err != nil {
		t.Fatalf("sv.Announce() error = %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	const linkPrefix = "→ link yourself as owner: /start "
	if len(lines) != 6 || !strings.HasPrefix(lines[1], linkPrefix) {
		t.Fatalf("sv.Announce() lines = %q, want a channel line and a link line for a and d, and a channel line for b and c", lines)
	}
	want := []string{"✓ a @a", lines[1], "✓ b @b", "✓ c", "✓ d @d", lines[1]}
	if diff := cmp.Diff(want, lines); diff != "" {
		t.Fatalf("sv.Announce() lines wrong result (-want +got) diff = %s", diff)
	}
}

func TestChannelSupervisorConcurrentAddsRunOneChannel(t *testing.T) {
	t.Parallel()
	sv := newTestSupervisor(t, time.Millisecond, nil)
	var active, maxActive atomic.Int32
	run := func(ctx context.Context, _ chan<- channel.Inbound, _ func(error)) error {
		n := active.Add(1)
		defer active.Add(-1)
		for {
			if m := maxActive.Load(); n <= m || maxActive.CompareAndSwap(m, n) {
				break
			}
		}
		<-ctx.Done()
		return nil
	}
	const adds = 8
	var wg sync.WaitGroup
	for range adds {
		wg.Go(func() { sv.Add(&stubChannel{name: "fake", run: run}) })
	}
	wg.Wait()

	deadline := time.Now().Add(waitTimeout)
	for active.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	if got := maxActive.Load(); got != 1 {
		t.Fatalf("channels running at once = %d, want 1", got)
	}
	if got := active.Load(); got != 1 {
		t.Fatalf("channels running after the adds = %d, want 1", got)
	}
}
