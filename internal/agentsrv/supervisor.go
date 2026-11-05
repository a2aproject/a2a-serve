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
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/backoff"
	"github.com/a2aproject/a2a-serve/internal/channel"
	"github.com/a2aproject/a2a-serve/internal/controlapi"
	"github.com/a2aproject/a2a-serve/internal/store"
)

// ChannelSupervisor runs the channels and restarts the ones which fail,
// and hands their messages to whoever reads [ChannelSupervisor.inbound].
type ChannelSupervisor struct {
	store    *store.Store
	logf     func(format string, args ...any)
	retryMin time.Duration
	// ctx ends when the server stops; channels run in it.
	ctx context.Context
	in  chan channel.Inbound

	// change serializes add and remove, which stop a channel before they
	// start its replacement.
	change sync.Mutex
	mu     sync.Mutex
	chans  map[string]*runningChannel
}

// SupervisorDeps are the dependencies of [ChannelSupervisor].
type SupervisorDeps struct {
	Store *store.Store
	Logf  func(format string, args ...any)
	// RetryMin is the first delay before a failed channel is restarted.
	RetryMin time.Duration
}

func newChannelSupervisor(ctx context.Context, deps SupervisorDeps) *ChannelSupervisor {
	return &ChannelSupervisor{
		store:    deps.Store,
		logf:     deps.Logf,
		retryMin: deps.RetryMin,
		ctx:      ctx,
		in:       make(chan channel.Inbound, 64),
		chans:    map[string]*runningChannel{},
	}
}

// inbound receives the messages of every channel.
func (sv *ChannelSupervisor) inbound() <-chan channel.Inbound { return sv.in }

type runningChannel struct {
	ch     channel.Channel
	cancel context.CancelFunc
	done   chan struct{}
	// err is the last error of Run or the last one it reported, cleared
	// when it runs again or reports that it is connected.
	err error
}

// Channel restarts back off from SupervisorDeps.RetryMin to
// channelRetryMax, and start over after a run of channelHealthy.
const (
	channelRetryMax = 5 * time.Minute
	channelHealthy  = time.Minute
)

// add starts a channel, replacing a running one of the same name after it
// stopped. A failing channel is restarted with backoff rather than stopping
// the server, so that a messenger outage doesn't take the agent down.
func (sv *ChannelSupervisor) add(ch channel.Channel) {
	sv.change.Lock()
	defer sv.change.Unlock()
	sv.stop(ch.Name())
	cctx, cancel := context.WithCancel(sv.ctx)
	rc := &runningChannel{ch: ch, cancel: cancel, done: make(chan struct{})}
	sv.mu.Lock()
	sv.chans[ch.Name()] = rc
	sv.mu.Unlock()
	go sv.run(cctx, rc)
}

func (sv *ChannelSupervisor) run(ctx context.Context, rc *runningChannel) {
	defer close(rc.done)
	retry := backoff.New(sv.retryMin, channelRetryMax, channelHealthy)
	for {
		started := time.Now()
		err := rc.ch.Run(ctx, sv.in, func(err error) { sv.setError(rc, err) })
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			err = errors.New("stopped unexpectedly")
		}
		delay := retry.Next(time.Since(started))
		slog.Error("channel stopped; restarting", "channel", rc.ch.Name(), "in", delay, "error", err)
		sv.setError(rc, err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		sv.setError(rc, nil)
	}
}

func (sv *ChannelSupervisor) setError(rc *runningChannel, err error) {
	sv.mu.Lock()
	defer sv.mu.Unlock()
	rc.err = err
}

// remove stops a channel and waits until it stopped, so that a replacement
// never polls the same account at the same time.
func (sv *ChannelSupervisor) remove(name string) {
	sv.change.Lock()
	defer sv.change.Unlock()
	sv.stop(name)
}

// stop is remove for callers that hold sv.change.
func (sv *ChannelSupervisor) stop(name string) {
	sv.mu.Lock()
	rc := sv.chans[name]
	delete(sv.chans, name)
	sv.mu.Unlock()
	if rc == nil {
		return
	}
	rc.cancel()
	<-rc.done
}

// stopAll stops every channel and waits until they stopped or ctx ends.
func (sv *ChannelSupervisor) stopAll(ctx context.Context) error {
	sv.mu.Lock()
	running := make([]*runningChannel, 0, len(sv.chans))
	for name, rc := range sv.chans {
		running = append(running, rc)
		delete(sv.chans, name)
	}
	sv.mu.Unlock()
	for _, rc := range running {
		rc.cancel()
	}
	for _, rc := range running {
		select {
		case <-rc.done:
		case <-ctx.Done():
			return fmt.Errorf("waiting for %s to stop: %w", rc.ch.Name(), ctx.Err())
		}
	}
	return nil
}

func (sv *ChannelSupervisor) channel(name string) channel.Channel {
	sv.mu.Lock()
	defer sv.mu.Unlock()
	if rc := sv.chans[name]; rc != nil {
		return rc.ch
	}
	return nil
}

func (sv *ChannelSupervisor) channels() []channel.Channel {
	sv.mu.Lock()
	defer sv.mu.Unlock()
	out := make([]channel.Channel, 0, len(sv.chans))
	for _, rc := range sv.chans {
		out = append(out, rc.ch)
	}
	slices.SortFunc(out, func(a, b channel.Channel) int { return strings.Compare(a.Name(), b.Name()) })
	return out
}

func (sv *ChannelSupervisor) status() []controlapi.ChannelStatus {
	var out []controlapi.ChannelStatus
	for _, ch := range sv.channels() {
		out = append(out, *sv.channelStatus(ch, nil))
	}
	return out
}

func (sv *ChannelSupervisor) channelStatus(ch channel.Channel, link *controlapi.OwnerLink) *controlapi.ChannelStatus {
	st := &controlapi.ChannelStatus{Name: ch.Name(), OwnerLink: link}
	if d, ok := ch.(channel.Describer); ok {
		st.Account = d.Describe()
	}
	sv.mu.Lock()
	if rc := sv.chans[ch.Name()]; rc != nil && rc.err != nil {
		st.Error = rc.err.Error()
	}
	sv.mu.Unlock()
	return st
}

// announce prints the channels, and how to link the owner if nobody is
// linked as owner yet. One owner code serves every channel.
func (sv *ChannelSupervisor) announce(ctx context.Context) error {
	var code mintedCode
	for _, ch := range sv.channels() {
		line := "✓ " + ch.Name()
		if d, ok := ch.(channel.Describer); ok {
			line += " " + d.Describe()
		}
		sv.logf("%s", line)
		if link := sv.ownerLinkWith(ctx, ch, &code); link != nil {
			sv.logf("%s", link.Line())
		}
	}
	return nil
}

// mintedCode is an owner code minted for the owner links of channels.
type mintedCode struct {
	code    string
	expires time.Time
}

// ownerLink mints an owner code for a channel nobody is linked on as owner.
func (sv *ChannelSupervisor) ownerLink(ctx context.Context, ch channel.Channel) *controlapi.OwnerLink {
	var code mintedCode
	return sv.ownerLinkWith(ctx, ch, &code)
}

// ownerLinkWith is ownerLink which mints *code if it is empty, and reuses
// it otherwise. A new owner code replaces the pending one, so that restarts
// don't pile up codes.
func (sv *ChannelSupervisor) ownerLinkWith(ctx context.Context, ch channel.Channel, code *mintedCode) *controlapi.OwnerLink {
	links, err := sv.store.LinksOf(ctx, auth.LocalPairing)
	if err != nil {
		slog.Error("listing owner links", "error", err)
		return nil
	}
	if slices.ContainsFunc(links, func(l store.Link) bool { return l.Channel == ch.Name() }) {
		return nil
	}
	d, ok := ch.(channel.Describer)
	if !ok {
		return nil
	}
	if code.code == "" {
		c, expires, err := auth.MintSoleCode(ctx, sv.store, auth.LocalPairing)
		if err != nil {
			slog.Error("minting owner code", "error", err)
			return nil
		}
		*code = mintedCode{code: c, expires: expires}
	}
	return &controlapi.OwnerLink{Hint: d.LinkHint(code.code), Code: code.code, Expires: code.expires}
}

func (sv *ChannelSupervisor) linkHints(code string) []string {
	var out []string
	for _, ch := range sv.channels() {
		if d, ok := ch.(channel.Describer); ok {
			out = append(out, ch.Name()+": "+d.LinkHint(code))
		}
	}
	return out
}
