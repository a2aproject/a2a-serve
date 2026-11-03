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

// Package manage adds, lists and removes what an agent keeps in its
// database: schedules and contacts. The control socket of a running agent
// and the CLI of a stopped one share it.
package manage

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/a2aproject/a2a-serve/internal/agentsrv/approvals"
	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/controlapi"
	"github.com/a2aproject/a2a-serve/internal/schedule"
	"github.com/a2aproject/a2a-serve/internal/store"
)

// Schedules adds, lists and removes schedules.
type Schedules struct {
	Store *store.Store
	// Now returns the current time; nil is time.Now.
	Now func() time.Time
	// Changed is called after a schedule is added or removed. Optional.
	Changed func()
}

func (m Schedules) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

func (m Schedules) changed() {
	if m.Changed != nil {
		m.Changed()
	}
}

// Add creates a schedule made by by. A schedule made in a conversation
// (contextID set) runs as the conversation's user and continues it.
func (m Schedules) Add(ctx context.Context, req schedule.Request, by, contextID string) (store.Schedule, error) {
	if err := req.Validate(); err != nil {
		return store.Schedule{}, err
	}
	when, err := req.When()
	if err != nil {
		return store.Schedule{}, err
	}
	next, ok, err := when.Next(m.now())
	if err != nil {
		return store.Schedule{}, err
	}
	if !ok {
		return store.Schedule{}, fmt.Errorf("--at %s is in the past", req.At)
	}
	s := store.Schedule{
		Name: req.Name, Cron: when.Cron, At: when.At, TZ: when.TZ, Prompt: req.Prompt,
		Conversation: req.Thread, ContextID: contextID, CreatedBy: by, NextRun: next,
	}
	if s.Conversation == "" {
		s.Conversation = schedule.ThreadNew
	}
	if contextID != "" {
		s.UserID, s.Conversation = by, schedule.ThreadSame
	} else if s.UserID, err = ResolveUser(ctx, m.Store, req.As); err != nil {
		return store.Schedule{}, err
	}
	if req.Chat != "" {
		ch, who, _ := strings.Cut(req.Chat, ":")
		s.DeliverChannel, s.DeliverUser = ch, s.UserID
		if who != "" {
			if s.DeliverUser, err = ResolveUser(ctx, m.Store, who); err != nil {
				return store.Schedule{}, err
			}
		}
		if _, err := DirectConv(ctx, m.Store, s.DeliverChannel, s.DeliverUser); err != nil {
			return store.Schedule{}, err
		}
	}
	if s.Conversation == schedule.ThreadSame && s.ContextID == "" && s.DeliverChannel == "" {
		s.ContextID = a2a.NewContextID()
	}
	if err := m.Store.AddSchedule(ctx, s); err != nil {
		if errors.Is(err, store.ErrExists) {
			return store.Schedule{}, fmt.Errorf("a schedule named %s already exists", req.Name)
		}
		return store.Schedule{}, err
	}
	m.changed()
	return s, nil
}

// Remove removes a schedule. Only the owner and the user a schedule runs as
// or who made it can remove it.
func (m Schedules) Remove(ctx context.Context, name string, by auth.Principal) error {
	s, err := m.Store.Schedule(ctx, name)
	if err != nil {
		return err
	}
	if !canManage(s, by) {
		return store.ErrNotFound
	}
	if err := m.Store.DeleteSchedule(ctx, name); err != nil {
		return err
	}
	m.changed()
	return nil
}

// Visible lists the schedules by can see: all for the owner, otherwise the
// ones they run as or made.
func (m Schedules) Visible(ctx context.Context, by auth.Principal) ([]store.Schedule, error) {
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
	return by.Has(auth.RoleOwner) || by.ID == s.UserID || by.ID == s.CreatedBy
}

// ScheduleInfo describes a schedule for the control API.
func ScheduleInfo(s store.Schedule) controlapi.ScheduleInfo {
	info := controlapi.ScheduleInfo{
		Name: s.Name, Cron: s.Cron, At: s.At, TZ: s.TZ, Prompt: s.Prompt, User: s.UserID,
		Thread: s.Conversation, CreatedBy: s.CreatedBy, NextRun: s.NextRun, LastRun: s.LastRun, LastError: s.LastError,
	}
	if s.DeliverChannel != "" {
		info.Chat = s.DeliverChannel + ":" + approvals.DisplayPrincipal(s.DeliverUser)
	}
	return info
}

// DirectConv returns the direct chat of user on a channel.
func DirectConv(ctx context.Context, st *store.Store, ch, user string) (string, error) {
	links, err := st.LinksOf(ctx, user)
	if err != nil {
		return "", err
	}
	for _, l := range links {
		if l.Channel == ch && l.Conv != "" {
			return l.Conv, nil
		}
	}
	return "", fmt.Errorf("%s has no chat on %s: link it with a code from `a2a-serve pair invite`", approvals.DisplayPrincipal(user), ch)
}

// ResolveUser turns owner or a pairing name into a principal ID which has
// a user. Principal IDs pass through.
func ResolveUser(ctx context.Context, st *store.Store, who string) (string, error) {
	switch {
	case who == "" || who == "owner" || who == auth.OwnerID:
		return auth.OwnerID, nil
	case !strings.Contains(who, ":"):
		who = "pair:" + who
	}
	if _, err := st.User(ctx, who); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return "", fmt.Errorf("unknown user %s: pair them first with `a2a-serve pair invite`", approvals.DisplayPrincipal(who))
		}
		return "", err
	}
	return who, nil
}
