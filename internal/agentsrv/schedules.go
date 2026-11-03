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
	"strings"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/a2aproject/a2a-serve/internal/agentsrv/approvals"
	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/channel"
	"github.com/a2aproject/a2a-serve/internal/controlapi"
	"github.com/a2aproject/a2a-serve/internal/schedule"
	"github.com/a2aproject/a2a-serve/internal/store"
)

// maxScheduleWait is the longest the scheduler sleeps between checks.
var maxScheduleWait = time.Minute

// schedules runs the agent's schedules, and adds and removes them for the
// owner and for the harness.
type schedules struct {
	// ctx ends when the server stops; runs happen in it.
	ctx    context.Context
	store  *store.Store
	router *Router
	now    func() time.Time
	// wake makes the scheduler check the schedules again after a change.
	wake chan struct{}
}

func newSchedules(ctx context.Context, st *store.Store, router *Router) *schedules {
	return &schedules{ctx: ctx, store: st, router: router, now: time.Now, wake: make(chan struct{}, 1)}
}

// run runs due schedules until ctx ends. A run missed while the agent was
// stopped runs once when it starts.
func (sc *schedules) run(ctx context.Context) {
	for {
		wait := sc.runDue(ctx)
		select {
		case <-ctx.Done():
			return
		case <-sc.wake:
		case <-time.After(wait):
		}
	}
}

func (sc *schedules) changed() {
	select {
	case sc.wake <- struct{}{}:
	default:
	}
}

// runDue runs the schedules which are due and returns how long until the
// next one.
func (sc *schedules) runDue(ctx context.Context) time.Duration {
	wait := maxScheduleWait
	all, err := sc.store.Schedules(ctx)
	if err != nil {
		slog.Error("listing schedules", "error", err)
		return wait
	}
	now := sc.now()
	for _, s := range all {
		if s.NextRun.IsZero() {
			continue
		}
		if s.NextRun.After(now) {
			wait = min(wait, s.NextRun.Sub(now))
			continue
		}
		runErr := sc.start(ctx, s)
		next, ok, err := whenOf(s).Next(now)
		if err != nil || !ok {
			if err := sc.store.DeleteSchedule(ctx, s.Name); err != nil && !errors.Is(err, store.ErrNotFound) {
				slog.Error("removing a finished schedule", "schedule", s.Name, "error", err)
			}
			continue
		}
		if err := sc.store.RecordScheduleRun(ctx, s.Name, now, next, errText(runErr)); err != nil {
			slog.Error("recording a schedule run", "schedule", s.Name, "error", err)
		}
		wait = min(wait, next.Sub(now))
	}
	return max(wait, scheduleMinWait)
}

// scheduleMinWait is the shortest wait between checks of the schedules.
var scheduleMinWait = time.Second

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func whenOf(s store.Schedule) schedule.When {
	return schedule.When{Cron: s.Cron, At: s.At, TZ: s.TZ}
}

// start posts a run of s into its conversation as a new turn. The turn runs
// in the background, as an ordinary task of the schedule's user.
func (sc *schedules) start(ctx context.Context, s store.Schedule) error {
	contextID, err := sc.contextFor(ctx, s)
	if err != nil {
		slog.Warn("running a schedule", "schedule", s.Name, "error", err)
		return err
	}
	text := fmt.Sprintf("Scheduled run of %q (no one is waiting at the keyboard; your answer is delivered to the user):\n%s", s.Name, s.Prompt)
	if err := sc.router.injectTurn(sc.ctx, contextID, userPrincipal(ctx, sc.store, s.UserID), text); err != nil {
		slog.Warn("running a schedule", "schedule", s.Name, "error", err)
		return err
	}
	return nil
}

// contextFor returns the context a run of s happens in. A run delivered to
// a chat becomes the chat's conversation, so replying there continues it.
func (sc *schedules) contextFor(ctx context.Context, s store.Schedule) (string, error) {
	if s.DeliverChannel == "" {
		if s.Conversation == schedule.ThreadSame && s.ContextID != "" {
			return s.ContextID, nil
		}
		return a2a.NewContextID(), nil
	}
	if sc.router.channel(s.DeliverChannel) == nil {
		return "", fmt.Errorf("channel %s is not running", s.DeliverChannel)
	}
	conv, err := directConv(ctx, sc.store, s.DeliverChannel, s.DeliverUser)
	if err != nil {
		return "", err
	}
	if s.Conversation == schedule.ThreadSame {
		return sc.router.contextOf(ctx, s.DeliverChannel, channel.ConvKey(conv))
	}
	id := a2a.NewContextID()
	return id, sc.store.SetContext(ctx, s.DeliverChannel, conv, id)
}

// directConv returns the direct chat of user on a channel.
func directConv(ctx context.Context, st *store.Store, ch, user string) (string, error) {
	links, err := st.LinksOf(ctx, user)
	if err != nil {
		return "", err
	}
	for _, l := range links {
		if l.Channel == ch && l.Conv != "" {
			return l.Conv, nil
		}
	}
	return "", fmt.Errorf("%s has no chat on %s: link it with a code from `a2a serve pair invite`", approvals.DisplayPrincipal(user), ch)
}

// userPrincipal is the principal of a stored user, with the roles it has now.
func userPrincipal(ctx context.Context, st *store.Store, id string) auth.Principal {
	if id == "" || id == auth.OwnerID {
		return auth.Owner()
	}
	who := auth.Principal{ID: id}
	u, err := st.User(ctx, id)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			slog.Warn("loading a user", "user", id, "error", err)
		}
		return who
	}
	who.Roles = u.Roles
	return who
}

// resolveUser turns owner or a pairing name into a principal ID which has
// a user. Principal IDs pass through.
func resolveUser(ctx context.Context, st *store.Store, who string) (string, error) {
	switch {
	case who == "" || who == "owner" || who == auth.OwnerID:
		return auth.OwnerID, nil
	case !strings.Contains(who, ":"):
		who = "pair:" + who
	}
	if _, err := st.User(ctx, who); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return "", fmt.Errorf("unknown user %s: pair them first with `a2a serve pair invite`", approvals.DisplayPrincipal(who))
		}
		return "", err
	}
	return who, nil
}

// add creates a schedule made by by. A schedule made in a conversation
// (contextID set) runs as the conversation's user and continues it.
func (sc *schedules) add(ctx context.Context, req schedule.Request, by, contextID string) (store.Schedule, error) {
	if err := req.Validate(); err != nil {
		return store.Schedule{}, err
	}
	when, err := req.When()
	if err != nil {
		return store.Schedule{}, err
	}
	next, ok, err := when.Next(sc.now())
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
	} else if s.UserID, err = resolveUser(ctx, sc.store, req.As); err != nil {
		return store.Schedule{}, err
	}
	if req.Chat != "" {
		ch, who, _ := strings.Cut(req.Chat, ":")
		s.DeliverChannel, s.DeliverUser = ch, s.UserID
		if who != "" {
			if s.DeliverUser, err = resolveUser(ctx, sc.store, who); err != nil {
				return store.Schedule{}, err
			}
		}
		if _, err := directConv(ctx, sc.store, s.DeliverChannel, s.DeliverUser); err != nil {
			return store.Schedule{}, err
		}
	}
	if s.Conversation == schedule.ThreadSame && s.ContextID == "" && s.DeliverChannel == "" {
		s.ContextID = a2a.NewContextID()
	}
	if err := sc.store.AddSchedule(ctx, s); err != nil {
		if errors.Is(err, store.ErrExists) {
			return store.Schedule{}, fmt.Errorf("a schedule named %s already exists", req.Name)
		}
		return store.Schedule{}, err
	}
	sc.changed()
	return s, nil
}

// runNow starts a run of a schedule at once, without changing when it runs
// next.
func (sc *schedules) runNow(ctx context.Context, name string) (store.Schedule, error) {
	s, err := sc.store.Schedule(ctx, name)
	if err != nil {
		return store.Schedule{}, err
	}
	runErr := sc.start(ctx, s)
	s.LastRun, s.LastError = sc.now(), errText(runErr)
	if err := sc.store.RecordScheduleRun(ctx, s.Name, s.LastRun, s.NextRun, s.LastError); err != nil {
		return store.Schedule{}, err
	}
	return s, runErr
}

// remove removes a schedule. Only the owner and the user a schedule runs as
// or who made it can remove it.
func (sc *schedules) remove(ctx context.Context, name string, by auth.Principal) error {
	s, err := sc.store.Schedule(ctx, name)
	if err != nil {
		return err
	}
	if !canManage(s, by) {
		return store.ErrNotFound
	}
	if err := sc.store.DeleteSchedule(ctx, name); err != nil {
		return err
	}
	sc.changed()
	return nil
}

// visible lists the schedules by can see: all for the owner, otherwise the
// ones they run as or made.
func (sc *schedules) visible(ctx context.Context, by auth.Principal) ([]store.Schedule, error) {
	all, err := sc.store.Schedules(ctx)
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

func scheduleInfo(s store.Schedule) controlapi.ScheduleInfo {
	info := controlapi.ScheduleInfo{
		Name: s.Name, Cron: s.Cron, At: s.At, TZ: s.TZ, Prompt: s.Prompt, User: s.UserID,
		Thread: s.Conversation, CreatedBy: s.CreatedBy, NextRun: s.NextRun, LastRun: s.LastRun, LastError: s.LastError,
	}
	if s.DeliverChannel != "" {
		info.Chat = s.DeliverChannel + ":" + approvals.DisplayPrincipal(s.DeliverUser)
	}
	return info
}
