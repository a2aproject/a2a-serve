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
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient/agentcard"
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
	injectTurn(ctx context.Context, t postedTurn) error
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
	key := w.Contact + "|" + w.TaskID
	ws.mu.Lock()
	if ws.watching[key] {
		ws.mu.Unlock()
		return
	}
	ws.watching[key] = true
	ws.mu.Unlock()
	go ws.follow(w, key)
}

func (ws *Watches) follow(w store.Watch, key string) {
	ctx := ws.ctx
	// A result waiting to be posted keeps the watch, so that watching the
	// task again doesn't post it twice.
	posting := false
	defer func() {
		if !posting {
			ws.done(key)
		}
	}()
	user, err := ws.watchPrincipal(ctx, w)
	if err != nil {
		user = auth.Principal{ID: w.OnBehalfOf}
	}
	claims := callerClaims{ContextID: w.OriginContextID, User: user}
	interval := ws.timings.WatchInterval
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
		if interval < ws.timings.WatchMaxInterval {
			interval += ws.timings.WatchInterval
		}
		// The watch is read before the poll, so that a follow-up after it
		// starts a new generation and posting the result watches the task
		// again.
		cur, err := ws.store.Watch(ctx, w.Contact, w.TaskID)
		if errors.Is(err, store.ErrNotFound) {
			return
		}
		if err != nil {
			slog.Warn("reading a watch", "agent", w.Contact, "task", w.TaskID, "error", err)
			continue
		}
		w = cur
		text, end := ws.check(ctx, &w, claims)
		switch end {
		case watchContinues:
			continue
		case watchDropped:
			ws.deleteWatch(ctx, w)
			return
		}
		user, err := ws.watchPrincipal(ctx, w)
		if err != nil {
			if time.Since(w.Created) < ws.timings.WatchMaxAge {
				continue
			}
			slog.Error("dropped a watched result: its user can't be resolved", "contact", w.Contact, "task", w.TaskID, "user", w.OnBehalfOf, "error", err)
			ws.deleteWatch(ctx, w)
			return
		}
		posting = ws.finishWatch(ctx, w, key, user, text)
		return
	}
}

// watchEnd is what a poll of a watched task leads to.
type watchEnd int

const (
	watchContinues watchEnd = iota
	// watchFinished posts a text about the task.
	watchFinished
	// watchDropped deletes the watch without a word.
	watchDropped
)

// check polls a watched task once and reports whether the watch ends, and
// the text to post if it does.
func (ws *Watches) check(ctx context.Context, w *store.Watch, claims callerClaims) (string, watchEnd) {
	task, err := ws.poll(ctx, *w, claims.User)
	if errors.Is(err, store.ErrNotFound) {
		slog.Info("stopped watching a task of a removed agent", "agent", w.Contact, "task", w.TaskID)
		return "", watchDropped
	}
	if reason, ok := permanentPollError(err); ok {
		slog.Warn("stopped watching a task", "agent", w.Contact, "task", w.TaskID, "reason", reason)
		return stoppedWatching(*w, reason), watchFinished
	}
	if err != nil {
		if w.FailingSince.IsZero() {
			w.FailingSince = time.Now()
			ws.setWatchFailing(ctx, *w, w.FailingSince)
		}
		if time.Since(w.FailingSince) >= watchGiveUp {
			return fmt.Sprintf("%s has been unreachable for a day; giving up on task %s: %v", w.Contact, w.TaskID, err), watchFinished
		}
		return ws.checkAge(*w)
	}
	if !w.FailingSince.IsZero() {
		w.FailingSince = time.Time{}
		ws.setWatchFailing(ctx, *w, w.FailingSince)
	}
	state := task.Status.State
	ws.noteState(ctx, w, state)
	if state == a2a.TaskStateInputRequired {
		if q := approvals.FromStatus(task.Status); q != nil && q.Type == approvals.TypePermission {
			ws.relay(ctx, claims, w.Contact, task, q)
			return ws.checkAge(*w)
		}
	}
	if !state.Terminal() && state != a2a.TaskStateInputRequired {
		if time.Since(w.StateChanged) >= ws.timings.WatchStale {
			return stoppedWatching(*w, fmt.Sprintf("it has been %s for %s", a2atext.StateName(state), describeWait(ws.timings.WatchStale))), watchFinished
		}
		return ws.checkAge(*w)
	}
	text := fmt.Sprintf("[%s finished task %s: %s]\n%s", w.Contact, w.TaskID, a2atext.StateName(state), resultText(task))
	if state == a2a.TaskStateInputRequired {
		text = fmt.Sprintf("[%s asks, on task %s]\n%s\nAnswer with: a2a send -a <agents URL>/%s --task-id %s \"...\"",
			w.Contact, w.TaskID, a2atext.Message(task.Status.Message), w.Contact, w.TaskID)
	}
	return text, watchFinished
}

// checkAge ends a watch older than the longest a task is watched.
func (ws *Watches) checkAge(w store.Watch) (string, watchEnd) {
	if time.Since(w.Created) < ws.timings.WatchMaxAge {
		return "", watchContinues
	}
	return stoppedWatching(w, fmt.Sprintf("it hasn't finished after %s", describeWait(ws.timings.WatchMaxAge))), watchFinished
}

// noteState records when the watched task changed to state.
func (ws *Watches) noteState(ctx context.Context, w *store.Watch, state a2a.TaskState) {
	if string(state) == w.State {
		return
	}
	w.State, w.StateChanged = string(state), time.Now()
	if err := ws.store.SetWatchState(ctx, w.Contact, w.TaskID, w.State, w.StateChanged); err != nil {
		slog.Warn("recording the state of a watched task", "agent", w.Contact, "task", w.TaskID, "error", err)
	}
}

func stoppedWatching(w store.Watch, reason string) string {
	return fmt.Sprintf("[%s: stopped watching task %s: %s]", w.Contact, w.TaskID, reason)
}

// describeWait describes d in whole days or hours where it can.
func describeWait(d time.Duration) string {
	const day = 24 * time.Hour
	switch {
	case d >= 2*day && d%day == 0:
		return fmt.Sprintf("%d days", d/day)
	case d >= 2*time.Hour && d%time.Hour == 0:
		return fmt.Sprintf("%d hours", d/time.Hour)
	}
	return d.String()
}

func (ws *Watches) done(key string) {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	delete(ws.watching, key)
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
	if err != nil && !transientPollError(err) {
		ws.clients.forgetClient(w.Contact)
	}
	return task, err
}

// transientPollError reports whether a poll failed because the agent
// couldn't be reached or failed, which keeps its client for the next poll.
func transientPollError(err error) bool {
	var netErr net.Error
	if errors.Is(err, context.DeadlineExceeded) || errors.As(err, &netErr) {
		return true
	}
	return errorStatus(err) >= http.StatusInternalServerError
}

// httpStatusMessage matches the error of the JSON-RPC client for a response
// with a status other than 200, which has no type to match.
var httpStatusMessage = regexp.MustCompile(`unexpected HTTP status: (\d{3})`)

// errorStatus returns the HTTP status behind an error of a call to an
// agent, or 0.
func errorStatus(err error) int {
	var notOK *agentcard.ErrStatusNotOK
	switch {
	case errors.Is(err, a2a.ErrTaskNotFound):
		return http.StatusNotFound
	case errors.Is(err, a2a.ErrUnauthenticated):
		return http.StatusUnauthorized
	case errors.Is(err, a2a.ErrUnauthorized):
		return http.StatusForbidden
	case errors.As(err, &notOK):
		return notOK.StatusCode
	}
	m := httpStatusMessage.FindStringSubmatch(err.Error())
	if m == nil {
		return 0
	}
	status, err := strconv.Atoi(m[1])
	if err != nil {
		return 0
	}
	return status
}

// permanentPollError reports whether polling a task can't succeed again, and
// why: the agent doesn't know the task, or refuses to talk to this one.
func permanentPollError(err error) (string, bool) {
	if err == nil {
		return "", false
	}
	switch errorStatus(err) {
	case http.StatusNotFound:
		return fmt.Sprintf("the agent doesn't know the task (%v)", err), true
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Sprintf("the agent refused access (%v)", err), true
	}
	return "", false
}

// finishWatch posts the result of a watched task as its user, if they may
// still call this agent. Once the agent accepted the result, the watch is
// deleted if no call watched the task again meanwhile, and the task is
// watched again otherwise. A restart before then posts the result again.
// It reports whether the result waits to be posted; the watch of key is
// done once it is.
func (ws *Watches) finishWatch(ctx context.Context, w store.Watch, key string, user auth.Principal, text string) bool {
	if user.Role == "" {
		slog.Warn("dropped a watched result: its user can no longer call this agent", "contact", w.Contact, "task", w.TaskID, "user", w.OnBehalfOf)
		ws.deleteWatch(ctx, w)
		return false
	}
	accepted := func(err error) {
		ctx := context.WithoutCancel(ctx)
		if err != nil {
			slog.Error("posting watched result; it is posted again on the next start", "contact", w.Contact, "task", w.TaskID, "error", err)
		} else {
			err := ws.store.DeleteWatch(ctx, w.Contact, w.TaskID, w.Generation)
			if !errors.Is(err, store.ErrNotFound) {
				if err != nil {
					slog.Error("deleting watch", "error", err)
				}
				ws.done(key)
				return
			}
		}
		ws.rewatch(ctx, w, key)
	}
	if err := ws.turns.injectTurn(ctx, postedTurn{ContextID: w.OriginContextID, User: user, Text: text, Accepted: accepted}); err != nil {
		slog.Error("posting watched result", "contact", w.Contact, "task", w.TaskID, "error", err)
		return false
	}
	return true
}

// rewatch ends the watch of key and watches its task again if a call
// watched it again since w was read.
func (ws *Watches) rewatch(ctx context.Context, w store.Watch, key string) {
	ws.done(key)
	cur, err := ws.store.Watch(ctx, w.Contact, w.TaskID)
	if errors.Is(err, store.ErrNotFound) {
		return
	}
	if err != nil {
		slog.Error("reading a watch; it is watched again on the next start", "contact", w.Contact, "task", w.TaskID, "error", err)
		return
	}
	if cur.Generation != w.Generation {
		ws.watch(cur)
	}
}

func (ws *Watches) deleteWatch(ctx context.Context, w store.Watch) {
	err := ws.store.DeleteWatch(ctx, w.Contact, w.TaskID, w.Generation)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		slog.Error("deleting watch", "error", err)
	}
}

// watchPrincipal is the user a watched task's result is posted as, with
// the role they have now; no role if they can no longer call this agent.
// An error, such as a failed lookup on the private network, says nothing
// about whether they still may.
func (ws *Watches) watchPrincipal(ctx context.Context, w store.Watch) (auth.Principal, error) {
	user, err := storedPrincipal(ctx, ws.chain, w.OnBehalfOf, w.Identity)
	if err != nil {
		slog.Warn("resolving the user of a watch", "contact", w.Contact, "task", w.TaskID, "user", w.OnBehalfOf, "error", err)
		return auth.Principal{}, err
	}
	return user, nil
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
