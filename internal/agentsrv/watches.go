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
	"sync"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-serve/internal/a2atext"
	"github.com/a2aproject/a2a-serve/internal/agentsrv/approvals"
	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/channel"
	"github.com/a2aproject/a2a-serve/internal/store"
)

// Watches follow the tasks of other agents which the harness doesn't wait
// for: they relay their permission questions to the user and post their
// results into the conversation which started them.
type Watches struct {
	store   *store.Store
	clients *ContactClients
	records *approvals.Records
	chat    questionChat
	turns   turnInjector
	// chain gives principals stored to act later their role.
	chain   *auth.Chain
	timings Timings
	// ctx ends when the server stops; watches run in it.
	ctx context.Context

	mu       sync.Mutex
	watching map[string]bool
}

// WatchesDeps are the dependencies of [Watches].
type WatchesDeps struct {
	Store   *store.Store
	Clients *ContactClients
	Records *approvals.Records
	Chat    questionChat
	Turns   turnInjector
	Chain   *auth.Chain
	Timings Timings
}

// questionChat is where watches show relayed questions.
type questionChat interface {
	Ask(ctx context.Context, to approvals.Chat, header string, rec store.Question)
	OwnerChat(ctx context.Context) (approvals.Chat, bool)
}

// turnInjector is where watches post late results.
type turnInjector interface {
	injectTurn(ctx context.Context, contextID string, p auth.Principal, text string) error
}

func newWatches(ctx context.Context, deps WatchesDeps) *Watches {
	return &Watches{
		store:    deps.Store,
		clients:  deps.Clients,
		records:  deps.Records,
		chat:     deps.Chat,
		turns:    deps.Turns,
		chain:    deps.Chain,
		timings:  deps.Timings,
		ctx:      ctx,
		watching: map[string]bool{},
	}
}

// relay asks the user on whose behalf a remote task runs to answer its
// permission question, once per question, also across restarts: in the
// conversation the call came from, or else in the user's direct chat, or
// else in the owner chat.
func (ws *Watches) relay(ctx context.Context, claims callerClaims, agent string, task *a2a.Task, q *approvals.Question) {
	user := claims.User.ID
	if user == "" {
		user = auth.OwnerID
	}
	rec, err := ws.records.NewRecord(ctx, task.ID, task.ContextID, agent, user, q)
	if errors.Is(err, store.ErrExists) {
		return
	}
	if err != nil {
		slog.Error("storing remote question", "error", err)
		return
	}
	chName, conv, err := ws.store.ConvOf(ctx, claims.ContextID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		slog.Warn("finding the conversation of a relayed question", "context", claims.ContextID, "error", err)
	}
	header := fmt.Sprintf("%s wants to", agent)
	if q.Title != "" {
		header = fmt.Sprintf("%s wants to run %s", agent, q.Title)
	}
	if chName != "" {
		ws.chat.Ask(ctx, approvals.Chat{Channel: chName, Conv: channel.ConvKey(conv)}, header, rec)
		return
	}
	if to, ok := ws.directChat(ctx, user); ok {
		ws.chat.Ask(ctx, to, header, rec)
		return
	}
	to, ok := ws.chat.OwnerChat(ctx)
	if !ok {
		slog.Info("remote approval waits in the inspector: no chat", "agent", agent, "user", user, "question", rec.ID)
		return
	}
	ws.chat.Ask(ctx, to, header+fmt.Sprintf(" for %s", auth.DisplayPrincipal(user)), rec)
}

// directChat returns the first direct chat of user in a messenger.
func (ws *Watches) directChat(ctx context.Context, user string) (approvals.Chat, bool) {
	name, ok := auth.PrincipalPairing(user)
	if !ok {
		return approvals.Chat{}, false
	}
	links, err := ws.store.LinksOf(ctx, name)
	if err != nil {
		slog.Warn("listing links", "user", user, "error", err)
		return approvals.Chat{}, false
	}
	for _, l := range links {
		if l.Conv != "" {
			return approvals.Chat{Channel: l.Channel, Conv: channel.ConvKey(l.Conv)}, true
		}
	}
	return approvals.Chat{}, false
}

// resume restarts watches persisted by a previous run.
func (ws *Watches) resume(ctx context.Context) {
	all, err := ws.store.Watches(ctx)
	if err != nil {
		slog.Error("loading watches", "error", err)
		return
	}
	for _, w := range all {
		ws.watch(w)
	}
}

// watch polls a remote task until it finishes or the server stops, and posts
// the result into the originating context as a new turn.
func (ws *Watches) watch(w store.Watch) {
	ctx := ws.ctx
	key := w.Contact + "|" + w.TaskID
	ws.mu.Lock()
	if ws.watching[key] {
		ws.mu.Unlock()
		return
	}
	ws.watching[key] = true
	ws.mu.Unlock()
	go func() {
		defer func() {
			ws.mu.Lock()
			delete(ws.watching, key)
			ws.mu.Unlock()
		}()
		claims := callerClaims{ContextID: w.OriginContextID, User: ws.watchPrincipal(ctx, w)}
		interval := ws.timings.WatchInterval
		failingSince := w.FailingSince
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(interval):
			}
			if interval < ws.timings.WatchMaxInterval {
				interval += ws.timings.WatchInterval
			}
			task, err := ws.poll(ctx, w, claims.User)
			if errors.Is(err, store.ErrNotFound) {
				slog.Info("stopped watching a task of a removed agent", "agent", w.Contact, "task", w.TaskID)
				if err := ws.store.DeleteWatch(ctx, w.Contact, w.TaskID); err != nil {
					slog.Error("deleting watch", "error", err)
				}
				return
			}
			if err != nil {
				if failingSince.IsZero() {
					failingSince = time.Now()
					ws.setWatchFailing(ctx, w, failingSince)
				}
				if time.Since(failingSince) < watchGiveUp {
					continue
				}
				ws.finishWatch(ctx, w, fmt.Sprintf("%s has been unreachable for a day; giving up on task %s: %v", w.Contact, w.TaskID, err))
				return
			}
			if !failingSince.IsZero() {
				failingSince = time.Time{}
				ws.setWatchFailing(ctx, w, failingSince)
			}
			if task.Status.State == a2a.TaskStateInputRequired {
				if q := approvals.FromStatus(task.Status); q != nil && q.Type == approvals.TypePermission {
					ws.relay(ctx, claims, w.Contact, task, q)
					continue
				}
			}
			if !task.Status.State.Terminal() && task.Status.State != a2a.TaskStateInputRequired {
				continue
			}
			text := fmt.Sprintf("[%s finished task %s: %s]\n%s", w.Contact, w.TaskID, a2atext.StateName(task.Status.State), resultText(task))
			if task.Status.State == a2a.TaskStateInputRequired {
				text = fmt.Sprintf("[%s asks, on task %s]\n%s\nAnswer with: a2a send -a <agents URL>/%s --task-id %s \"...\"",
					w.Contact, w.TaskID, a2atext.Message(task.Status.Message), w.Contact, w.TaskID)
			}
			ws.finishWatch(ctx, w, text)
			return
		}
	}()
}

// watchGiveUp is how long a watched task's agent may be unreachable.
const watchGiveUp = 24 * time.Hour

func (ws *Watches) setWatchFailing(ctx context.Context, w store.Watch, since time.Time) {
	if err := ws.store.SetWatchFailing(ctx, w.Contact, w.TaskID, since); err != nil {
		slog.Warn("recording an unreachable watch", "agent", w.Contact, "task", w.TaskID, "error", err)
	}
}

func (ws *Watches) poll(ctx context.Context, w store.Watch, user auth.Principal) (*a2a.Task, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	t, err := ws.clients.target(ctx, w.Contact, user, 0)
	if err != nil {
		return nil, err
	}
	task, err := t.get(ctx, a2a.TaskID(w.TaskID))
	if err != nil {
		ws.clients.forgetClient(w.Contact)
	}
	return task, err
}

// finishWatch posts the result of a watched task as its user, if they may
// still call this agent.
func (ws *Watches) finishWatch(ctx context.Context, w store.Watch, text string) {
	if err := ws.store.DeleteWatch(ctx, w.Contact, w.TaskID); err != nil {
		slog.Error("deleting watch", "error", err)
	}
	user := ws.watchPrincipal(ctx, w)
	if user.Role == "" {
		slog.Warn("dropped a watched result: its user can no longer call this agent", "contact", w.Contact, "task", w.TaskID, "user", w.OnBehalfOf)
		return
	}
	if err := ws.turns.injectTurn(ctx, w.OriginContextID, user, text); err != nil {
		slog.Error("posting watched result", "contact", w.Contact, "task", w.TaskID, "error", err)
	}
}

// watchPrincipal is the user a watched task's result is posted as, with
// the role they have now; no role if they can no longer call this agent.
func (ws *Watches) watchPrincipal(ctx context.Context, w store.Watch) auth.Principal {
	user, err := storedPrincipal(ctx, ws.chain, w.OnBehalfOf, w.Identity)
	if err != nil {
		slog.Warn("resolving the user of a watch", "contact", w.Contact, "task", w.TaskID, "user", w.OnBehalfOf, "error", err)
		return auth.Principal{ID: w.OnBehalfOf}
	}
	return user
}

// resultText is the text of a finished task: its artifacts, or its status message.
func resultText(t *a2a.Task) string {
	var parts []string
	for _, a := range t.Artifacts {
		var sb strings.Builder
		for _, p := range a.Parts {
			sb.WriteString(p.Text())
		}
		if sb.Len() > 0 {
			parts = append(parts, sb.String())
		}
	}
	if len(parts) == 0 {
		return a2atext.Message(t.Status.Message)
	}
	return strings.Join(parts, "\n\n")
}
