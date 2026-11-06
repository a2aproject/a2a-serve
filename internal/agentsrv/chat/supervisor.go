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

package chat

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
	"github.com/a2aproject/a2a-serve/internal/controlapi"
	"github.com/a2aproject/a2a-serve/internal/messenger"
	"github.com/a2aproject/a2a-serve/internal/store"
)

// MessengerSupervisor runs the messengers and restarts the ones which fail,
// and hands their messages to whoever reads [MessengerSupervisor.inbound].
type MessengerSupervisor struct {
	store    *store.Store
	logf     func(format string, args ...any)
	retryMin time.Duration
	// ctx ends when the server stops; messengers run in it.
	ctx context.Context
	in  chan messenger.Inbound

	// change serializes add and remove, which stop a messenger before they
	// start its replacement.
	change sync.Mutex
	mu     sync.Mutex
	chans  map[string]*runningMessenger
}

// SupervisorDeps are the dependencies of [MessengerSupervisor].
type SupervisorDeps struct {
	Store *store.Store
	Logf  func(format string, args ...any)
	// RetryMin is the first delay before a failed messenger is restarted.
	RetryMin time.Duration
}

// NewMessengerSupervisor returns a supervisor whose messengers run until ctx
// ends.
func NewMessengerSupervisor(ctx context.Context, deps SupervisorDeps) *MessengerSupervisor {
	return &MessengerSupervisor{
		store:    deps.Store,
		logf:     deps.Logf,
		retryMin: deps.RetryMin,
		ctx:      ctx,
		in:       make(chan messenger.Inbound, 64),
		chans:    map[string]*runningMessenger{},
	}
}

// inbound receives the messages of every messenger.
func (sv *MessengerSupervisor) inbound() <-chan messenger.Inbound { return sv.in }

type runningMessenger struct {
	ch     messenger.Messenger
	cancel context.CancelFunc
	done   chan struct{}
	// err is the last error of Run or the last one it reported, cleared
	// when it runs again or reports that it is connected.
	err error
}

// Messenger restarts back off from SupervisorDeps.RetryMin to
// messengerRetryMax, and start over after a run of messengerHealthy.
const (
	messengerRetryMax = 5 * time.Minute
	messengerHealthy  = time.Minute
)

// Add starts a messenger, replacing a running one of the same name after it
// stopped. A failing messenger is restarted with backoff rather than stopping
// the server, so that a messenger outage doesn't take the agent down.
func (sv *MessengerSupervisor) Add(ch messenger.Messenger) {
	sv.change.Lock()
	defer sv.change.Unlock()
	sv.stop(ch.Name())
	cctx, cancel := context.WithCancel(sv.ctx)
	rc := &runningMessenger{ch: ch, cancel: cancel, done: make(chan struct{})}
	sv.mu.Lock()
	sv.chans[ch.Name()] = rc
	sv.mu.Unlock()
	go sv.run(cctx, rc)
}

func (sv *MessengerSupervisor) run(ctx context.Context, rc *runningMessenger) {
	defer close(rc.done)
	retry := backoff.New(sv.retryMin, messengerRetryMax, messengerHealthy)
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
		slog.Error("messenger stopped; restarting", "messenger", rc.ch.Name(), "in", delay, "error", err)
		sv.setError(rc, err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		sv.setError(rc, nil)
	}
}

func (sv *MessengerSupervisor) setError(rc *runningMessenger, err error) {
	sv.mu.Lock()
	defer sv.mu.Unlock()
	rc.err = err
}

// Remove stops a messenger and waits until it stopped, so that a replacement
// never polls the same account at the same time.
func (sv *MessengerSupervisor) Remove(name string) {
	sv.change.Lock()
	defer sv.change.Unlock()
	sv.stop(name)
}

// stop is remove for callers that hold sv.change.
func (sv *MessengerSupervisor) stop(name string) {
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

// StopAll stops every messenger and waits until they stopped or ctx ends.
func (sv *MessengerSupervisor) StopAll(ctx context.Context) error {
	sv.mu.Lock()
	running := make([]*runningMessenger, 0, len(sv.chans))
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

// Messenger returns the running messenger called name, or nil.
func (sv *MessengerSupervisor) Messenger(name string) messenger.Messenger {
	sv.mu.Lock()
	defer sv.mu.Unlock()
	if rc := sv.chans[name]; rc != nil {
		return rc.ch
	}
	return nil
}

func (sv *MessengerSupervisor) messengers() []messenger.Messenger {
	sv.mu.Lock()
	defer sv.mu.Unlock()
	out := make([]messenger.Messenger, 0, len(sv.chans))
	for _, rc := range sv.chans {
		out = append(out, rc.ch)
	}
	slices.SortFunc(out, func(a, b messenger.Messenger) int { return strings.Compare(a.Name(), b.Name()) })
	return out
}

// Status describes the running messengers.
func (sv *MessengerSupervisor) Status() []controlapi.MessengerStatus {
	var out []controlapi.MessengerStatus
	for _, ch := range sv.messengers() {
		out = append(out, *sv.MessengerStatus(ch, nil))
	}
	return out
}

// MessengerStatus describes ch, with link to link the owner on it if set.
func (sv *MessengerSupervisor) MessengerStatus(ch messenger.Messenger, link *controlapi.OwnerLink) *controlapi.MessengerStatus {
	st := &controlapi.MessengerStatus{Name: ch.Name(), OwnerLink: link}
	if d, ok := ch.(messenger.Describer); ok {
		st.Account = d.Describe()
	}
	sv.mu.Lock()
	if rc := sv.chans[ch.Name()]; rc != nil && rc.err != nil {
		st.Error = rc.err.Error()
	}
	sv.mu.Unlock()
	return st
}

// Announce prints the messengers, and how to link the owner if nobody is
// linked as owner yet. One owner code serves every messenger.
func (sv *MessengerSupervisor) Announce(ctx context.Context) error {
	var code mintedCode
	for _, ch := range sv.messengers() {
		line := "✓ " + ch.Name()
		if d, ok := ch.(messenger.Describer); ok {
			line += " " + d.Describe()
		}
		sv.logf("%s", line)
		if link := sv.ownerLinkWith(ctx, ch, &code); link != nil {
			sv.logf("%s", link.Line())
		}
	}
	return nil
}

// mintedCode is an owner code minted for the owner links of messengers.
type mintedCode struct {
	code    string
	expires time.Time
}

// OwnerLink mints an owner code for a messenger nobody is linked on as owner.
func (sv *MessengerSupervisor) OwnerLink(ctx context.Context, ch messenger.Messenger) *controlapi.OwnerLink {
	var code mintedCode
	return sv.ownerLinkWith(ctx, ch, &code)
}

// ownerLinkWith is ownerLink which mints *code if it is empty, and reuses
// it otherwise. A new owner code replaces the pending one, so that restarts
// don't pile up codes.
func (sv *MessengerSupervisor) ownerLinkWith(ctx context.Context, ch messenger.Messenger, code *mintedCode) *controlapi.OwnerLink {
	links, err := sv.store.LinksOf(ctx, auth.LocalPairing)
	if err != nil {
		slog.Error("listing owner links", "error", err)
		return nil
	}
	if slices.ContainsFunc(links, func(l store.Link) bool { return l.Messenger == ch.Name() }) {
		return nil
	}
	d, ok := ch.(messenger.Describer)
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

// LinkHints tell how to link an account with code on each messenger.
func (sv *MessengerSupervisor) LinkHints(code string) []string {
	var out []string
	for _, ch := range sv.messengers() {
		if d, ok := ch.(messenger.Describer); ok {
			out = append(out, ch.Name()+": "+d.LinkHint(code))
		}
	}
	return out
}
