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

// Package schedulesvc adds, lists and removes the schedules of an agent. The
// control socket of a running agent and the CLI of a stopped one share it.
package schedulesvc

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/a2aproject/a2a-serve/internal/agenterr"
	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/pairings"
	"github.com/a2aproject/a2a-serve/internal/schedule"
	"github.com/a2aproject/a2a-serve/internal/store"
)

// Service adds, lists and removes schedules.
type Service struct {
	Store *store.Store
	// Now returns the current time; nil is time.Now.
	Now func() time.Time
	// Changed is called after a schedule is added or removed. Optional.
	Changed func()
	// Limits bound the schedules Add creates. The zero value has none.
	Limits Limits
	// Removed is called with the name of a removed schedule. Optional.
	Removed func(name string)
}

// Limits bound the schedules of a user.
type Limits struct {
	// PerUser is how many schedules one user may make.
	PerUser int
	// MinInterval is the shortest time allowed between two runs.
	MinInterval time.Duration
}

// AgentLimits bound the schedules the agent makes through the agents URL.
var AgentLimits = Limits{PerUser: 50, MinInterval: 5 * time.Minute}

// check refuses runs closer together than l allows.
func (l Limits) check(when schedule.When) error {
	if l.MinInterval == 0 || when.Cron == "" {
		return nil
	}
	interval, err := when.MinInterval()
	if err != nil {
		return agenterr.Invalid(err)
	}
	if interval < l.MinInterval {
		return agenterr.Invalidf("the runs of a schedule must be at least %d minutes apart", int(l.MinInterval.Minutes()))
	}
	return nil
}

func (m Service) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

func (m Service) changed() {
	if m.Changed != nil {
		m.Changed()
	}
}

// Add creates a schedule made by by. A schedule made in a conversation
// (contextID set) runs as the conversation's user and continues it.
func (m Service) Add(ctx context.Context, req schedule.Request, by auth.Principal, contextID string) (store.Schedule, error) {
	if err := req.Validate(); err != nil {
		return store.Schedule{}, agenterr.Invalid(err)
	}
	when, err := req.When()
	if err != nil {
		return store.Schedule{}, agenterr.Invalid(err)
	}
	next, ok, err := when.Next(time.Time{}, m.now())
	if err != nil {
		return store.Schedule{}, agenterr.Invalid(err)
	}
	if !ok {
		return store.Schedule{}, agenterr.Invalidf("--at %s is in the past", req.At)
	}
	if err := m.Limits.check(when); err != nil {
		return store.Schedule{}, err
	}
	s := store.Schedule{
		Name: req.Name, Cron: when.Cron, At: when.At, TZ: when.TZ, Prompt: req.Prompt,
		Conversation: req.Thread, ContextID: contextID, CreatedBy: by.ID, NextRun: next,
	}
	if s.Conversation == "" {
		s.Conversation = schedule.ThreadNew
	}
	if contextID != "" {
		identity, err := json.Marshal(by.Identity)
		if err != nil {
			return store.Schedule{}, err
		}
		s.UserID, s.UserIdentity, s.Conversation = by.ID, string(identity), schedule.ThreadSame
	} else if s.UserID, err = ResolveUser(ctx, m.Store, req.As); err != nil {
		return store.Schedule{}, err
	}
	if req.Chat != "" {
		ch, who, _ := strings.Cut(req.Chat, ":")
		s.DeliverMessenger, s.DeliverUser = ch, s.UserID
		if who != "" {
			if s.DeliverUser, err = ResolveUser(ctx, m.Store, who); err != nil {
				return store.Schedule{}, err
			}
		}
		if _, err := pairings.DirectConv(ctx, m.Store, s.DeliverMessenger, s.DeliverUser); err != nil {
			return store.Schedule{}, err
		}
	}
	if s.Conversation == schedule.ThreadSame && s.ContextID == "" && s.DeliverMessenger == "" {
		s.ContextID = a2a.NewContextID()
	}
	if err := m.Store.AddScheduleCapped(ctx, s, m.Limits.PerUser); err != nil {
		if errors.Is(err, store.ErrExists) {
			return store.Schedule{}, agenterr.Conflictf("a schedule named %s already exists", req.Name)
		}
		if errors.Is(err, store.ErrLimit) {
			return store.Schedule{}, agenterr.Conflictf("a user can make at most %d schedules here; remove one first", m.Limits.PerUser)
		}
		return store.Schedule{}, err
	}
	m.changed()
	return s, nil
}

// Remove removes a schedule. Only the owner and the user a schedule runs as
// or who made it can remove it.
func (m Service) Remove(ctx context.Context, name string, by auth.Principal) error {
	s, err := m.Store.Schedule(ctx, name)
	if errors.Is(err, store.ErrNotFound) || err == nil && !canManage(s, by) {
		return &agenterr.NotFoundError{Kind: "schedule", Name: name}
	}
	if err != nil {
		return err
	}
	if err := m.Store.DeleteSchedule(ctx, name); err != nil {
		return err
	}
	if m.Removed != nil {
		m.Removed(name)
	}
	m.changed()
	return nil
}

// Visible lists the schedules by can see: all for the owner, otherwise the
// ones they run as or made.
func (m Service) Visible(ctx context.Context, by auth.Principal) ([]store.Schedule, error) {
	all, err := m.Store.Schedules(ctx)
	if err != nil {
		return nil, err
	}
	var out []store.Schedule
	for _, s := range all {
		if canManage(s, by) {
			out = append(out, s)
		}
	}
	return out, nil
}

func canManage(s store.Schedule, by auth.Principal) bool {
	return by.IsOwner() || by.ID == s.UserID || by.ID == s.CreatedBy
}

// ResolveUser turns owner or a pairing name into the principal its tokens
// and chat links act as, which is the owner for the local pairing.
// Principal IDs of pairings resolve the same way.
func ResolveUser(ctx context.Context, st *store.Store, who string) (string, error) {
	switch {
	case who == "" || who == auth.OwnerName || who == auth.OwnerID:
		return auth.OwnerID, nil
	case !strings.Contains(who, ":"):
		who = auth.PairPrincipal(who)
	}
	name, ok := auth.PrincipalPairing(who)
	if !ok {
		return "", unknownUser(who)
	}
	if _, err := st.Pairing(ctx, name); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return "", unknownUser(who)
		}
		return "", err
	}
	return auth.PairingPrincipal(name), nil
}

func unknownUser(who string) error {
	err := agenterr.Invalidf("unknown user %s", auth.DisplayPrincipal(who))
	name, ok := auth.PrincipalPairing(who)
	if !ok {
		return err
	}
	return agenterr.WithHint(err, agenterr.HintPairInvite, name)
}
