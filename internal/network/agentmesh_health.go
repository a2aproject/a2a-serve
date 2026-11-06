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

//go:build agentmesh

package network

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

const (
	// healthCheckEvery is how often the server checks that it is attached
	// to a router of the relay.
	healthCheckEvery = 10 * time.Second
	// healthProbeEvery is how often the server asks the relay why it is
	// detached, which costs the relay a credential refresh.
	healthProbeEvery   = time.Minute
	healthProbeTimeout = 10 * time.Second
)

// Health implements [HealthReporter].
func (a *agentMesh) Health() Health {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.health
}

// watchHealth checks the connection to the relay until ctx is done.
func (a *agentMesh) watchHealth(ctx context.Context, done chan<- struct{}) {
	defer close(done)
	t := time.NewTicker(healthCheckEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			a.checkHealth(ctx, now)
		}
	}
}

// checkHealth updates the health from the routers the session is attached
// to. A detached session asks the relay why, at most every
// healthProbeEvery: a relay that removed the agent refuses its credential.
func (a *agentMesh) checkHealth(ctx context.Context, now time.Time) {
	srv := a.session()
	if srv == nil {
		return
	}
	var h Health
	probed := false
	if len(srv.Status().AttachedRouters) == 0 {
		a.mu.Lock()
		prev, probedAt := a.health, a.probedAt
		a.mu.Unlock()
		h = prev
		if prev.OK() || now.Sub(probedAt) >= healthProbeEvery {
			pctx, cancel := context.WithTimeout(ctx, healthProbeTimeout)
			h = a.relayHealth(srv.Refresh(pctx))
			cancel()
			probed = true
		}
	}
	if ctx.Err() != nil {
		return
	}
	a.mu.Lock()
	prev := a.health
	a.health = h
	if probed {
		a.probedAt = now
	}
	a.mu.Unlock()
	if prev != h {
		a.logHealth(h)
	}
}

// relayHealth is the health of a session detached from the relay, whose
// credential refresh returned err.
func (a *agentMesh) relayHealth(err error) Health {
	if relayRefused(err) {
		return Health{
			State:   HealthRemoved,
			Problem: fmt.Sprintf("the relay %s removed this agent", a.cfg.URL),
			Fix:     "ask the relay's operator to let it back, then `a2a-serve restart`",
		}
	}
	h := Health{
		State:   HealthDisconnected,
		Problem: fmt.Sprintf("not connected to the relay %s; reconnecting", a.cfg.URL),
		Fix:     "check that the relay runs and that this machine reaches it",
	}
	if err != nil {
		h.Problem = fmt.Sprintf("not connected to the relay %s (%v); reconnecting", a.cfg.URL, err)
	}
	return h
}

// relayRefused reports whether a credential refresh failed because the
// relay refused the agent. meshnet doesn't export the error type, and a
// relay answers 403 only to a node it banned.
func relayRefused(err error) bool {
	return err != nil && strings.Contains(err.Error(), "403 Forbidden")
}

func (a *agentMesh) logHealth(h Health) {
	switch h.State {
	case HealthOK:
		slog.Info("reconnected to the relay", "relay", a.cfg.URL)
		a.logf("✓ relay: reconnected to %s", a.cfg.URL)
	case HealthRemoved:
		slog.Error("the relay removed this agent", "relay", a.cfg.URL)
		a.logf("✗ relay: %v", h.Err())
	default:
		slog.Warn("not connected to the relay", "relay", a.cfg.URL, "problem", h.Problem)
		a.logf("! relay: %s", h.Problem)
	}
}
