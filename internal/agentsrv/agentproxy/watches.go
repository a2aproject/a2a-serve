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

package agentproxy

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
	"github.com/a2aproject/a2a-serve/internal/agentsrv/chat"
	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/messenger"
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
	turns   TurnInjector
	asked   QuestionTasks
	// chain gives principals stored to act later their role.
	chain   *auth.Chain
	timings Timings
	// ctx ends when the server stops; watches run in it.
	ctx context.Context

	mu       sync.Mutex
	watching map[string]bool
	// callers are the contexts whose A2A caller waits for their task, which
	// is asked the questions of the agents it calls.
	callers map[string]*callerWait
}

// WatchesDeps are the dependencies of [Watches].
type WatchesDeps struct {
	Store   *store.Store
	Clients *ContactClients
	Records *approvals.Records
	Chat    questionChat
	Turns   TurnInjector
	Asked   QuestionTasks
	Chain   *auth.Chain
	Timings Timings
}

// questionChat is where watches show relayed questions.
type questionChat interface {
	Ask(ctx context.Context, to approvals.Chat, header string, rec store.Question)
	OwnerChat(ctx context.Context) (approvals.Chat, bool)
}

// TurnInjector is where watches post late results.
type TurnInjector interface {
	// InjectTurn posts a turn into its context.
	InjectTurn(ctx context.Context, t chat.PostedTurn) error
}

// QuestionTasks ask relayed questions in the conversations they come from,
// each on a task of its own that the A2A caller answers.
type QuestionTasks interface {
	// Ask asks q on a new task in contextID.
	Ask(ctx context.Context, contextID string, q *CallerQuestion) error
	// End ends the tasks in contextID that ask a question of agent's task,
	// once that task no longer waits for an answer.
	End(ctx context.Context, contextID, agent, taskID string)
}

// NewWatches returns the watches of deps, which poll until ctx ends.
func NewWatches(ctx context.Context, deps WatchesDeps) *Watches {
	return &Watches{
		store:    deps.Store,
		clients:  deps.Clients,
		records:  deps.Records,
		chat:     deps.Chat,
		turns:    deps.Turns,
		asked:    deps.Asked,
		chain:    deps.Chain,
		timings:  deps.Timings,
		ctx:      ctx,
		watching: map[string]bool{},
		callers:  map[string]*callerWait{},
	}
}

// relay asks the user on whose behalf a remote task runs to answer its
// permission question, once per question, also across restarts: in the
// conversation the call came from, or else on the task of the A2A caller
// who waits for the turn that called the agent, or else in the user's
// direct chat, or else in the owner chat, or else on a new task in the
// conversation.
func (ws *Watches) relay(ctx context.Context, claims Claims, agent string, task *a2a.Task, q *approvals.Question) {
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
		ws.chat.Ask(ctx, approvals.Chat{Messenger: chName, Conv: messenger.ConvKey(conv)}, header, rec)
		return
	}
	cq := &CallerQuestion{Rec: rec, Header: header, Msg: task.Status.Message}
	if ws.toCaller(claims.ContextID, cq) {
		return
	}
	ws.askInChat(ctx, claims.ContextID, cq)
}

// askInChat asks the question of a remote task in the direct chat of the
// user on whose behalf it runs, or else in the owner chat, or else on a new
// task in contextID, the conversation without a chat it comes from.
func (ws *Watches) askInChat(ctx context.Context, contextID string, q *CallerQuestion) {
	user, rec := q.Rec.User, q.Rec
	if to, ok := ws.directChat(ctx, user); ok {
		ws.chat.Ask(ctx, to, q.Header, rec)
		return
	}
	if to, ok := ws.chat.OwnerChat(ctx); ok {
		ws.chat.Ask(ctx, to, q.Header+fmt.Sprintf(" for %s", auth.DisplayPrincipal(user)), rec)
		return
	}
	if contextID == "" {
		slog.Info("remote approval waits in the inspector: no chat", "agent", rec.Contact, "user", user, "question", rec.ID)
		return
	}
	if err := ws.asked.Ask(ctx, contextID, q); err != nil {
		slog.Warn("remote approval waits in the inspector: asking it in the conversation", "agent", rec.Contact, "user", user, "question", rec.ID, "context", contextID, "error", err)
	}
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
			return approvals.Chat{Messenger: l.Messenger, Conv: messenger.ConvKey(l.Conv)}, true
		}
	}
	return approvals.Chat{}, false
}

// Resume restarts watches persisted by a previous run.
func (ws *Watches) Resume(ctx context.Context) {
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
	claims := Claims{ContextID: w.OriginContextID, User: user}
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
			ws.endToCaller(w, reasonRemoved)
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
			ws.endQuestions(ctx, w)
			ws.deleteWatch(ctx, w)
			ws.endToCaller(w, "the agent was removed or may no longer be called")
			return
		}
		user, err := ws.watchPrincipal(ctx, w)
		if err != nil {
			if time.Since(w.Created) < ws.timings.WatchMaxAge {
				continue
			}
			slog.Error("dropped a watched result: its user can't be resolved", "contact", w.Contact, "task", w.TaskID, "user", w.OnBehalfOf, "error", err)
			ws.endQuestions(ctx, w)
			ws.deleteWatch(ctx, w)
			ws.endToCaller(w, "the user it ran for can't be resolved")
			return
		}
		ws.endQuestions(ctx, w)
		posting = ws.finishWatch(ctx, w, key, user, text, interval, false)
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
func (ws *Watches) check(ctx context.Context, w *store.Watch, claims Claims) (string, watchEnd) {
	task, err := ws.poll(ctx, *w, claims.User)
	if errors.Is(err, store.ErrNotFound) {
		if !ws.clients.CallsPeers() {
			slog.Info("stopped watching a task of a removed agent", "agent", w.Contact, "task", w.TaskID)
			return "", watchDropped
		}
		// An agent called by name is listed only while it is online, so it
		// may be back soon. A removed contact takes the same path.
		err = errors.New("no agent by this name is online on the private network")
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
	if w.State == string(a2a.TaskStateInputRequired) && state != a2a.TaskStateInputRequired {
		ws.endQuestions(ctx, *w)
	}
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
	text := fmt.Sprintf("[%s finished task %s: %s]\n%s", w.Contact, w.TaskID, a2atext.StateName(state), a2atext.Result(task))
	if state == a2a.TaskStateInputRequired {
		text = fmt.Sprintf("[%s asks, on task %s]\n%s\nAnswer with: a2a send -a <agents URL>/%s --task-id %s \"...\"",
			w.Contact, w.TaskID, a2atext.Message(task.Status.Message), w.Contact, w.TaskID)
	}
	return text, watchFinished
}

// endQuestions ends the tasks that ask a question of the watched task in
// its conversation. A task whose caller answered the question waits for the
// outcome, and runs on.
func (ws *Watches) endQuestions(ctx context.Context, w store.Watch) {
	if ws.awaited(w.OriginContextID) {
		return
	}
	ws.asked.End(ctx, w.OriginContextID, w.Contact, w.TaskID)
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

// reasonRemoved is why a watch whose row is gone ends: its contact, node or
// user was removed.
const reasonRemoved = "the agent or the user it ran for was removed"

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
// watched again otherwise. A result the agent refused is posted again
// after interval. A restart before then posts the result again. It reports
// whether the result waits to be posted; the watch of key is done once it
// is. A result posted again doesn't show the refusal in the chat again.
func (ws *Watches) finishWatch(ctx context.Context, w store.Watch, key string, user auth.Principal, text string, interval time.Duration, again bool) bool {
	if user.Role == "" {
		slog.Warn("dropped a watched result: its user can no longer call this agent", "contact", w.Contact, "task", w.TaskID, "user", w.OnBehalfOf)
		ws.deleteWatch(ctx, w)
		ws.endToCaller(w, "the user it ran for can no longer call this agent")
		return false
	}
	accepted := func(err error) {
		ctx := context.WithoutCancel(ctx)
		if err != nil {
			slog.Warn("the agent refused a watched result; it is posted again", "contact", w.Contact, "task", w.TaskID, "in", interval, "error", err)
			go ws.retryPost(w, key, text, interval)
			return
		}
		err = ws.store.DeleteWatch(ctx, w.Contact, w.TaskID, w.Generation)
		if !errors.Is(err, store.ErrNotFound) {
			if err != nil {
				slog.Error("deleting watch", "error", err)
			}
			ws.done(key)
			return
		}
		ws.rewatch(ctx, w, key)
	}
	turnText, files := resultTurn(text)
	t := chat.PostedTurn{ContextID: w.OriginContextID, User: user, Text: turnText, Files: files, Accepted: accepted, Quiet: again}
	if ws.resultToCaller(t) {
		return true
	}
	if err := ws.turns.InjectTurn(ctx, t); err != nil {
		slog.Error("posting watched result; it is posted again", "contact", w.Contact, "task", w.TaskID, "in", interval, "error", err)
		go ws.retryPost(w, key, text, interval)
	}
	return true
}

// retryPost posts the result of the watch of key again after interval,
// until the agent accepts it, the user can no longer call this agent, or
// the watch is older than the longest a task is watched. A call that
// watched the task again meanwhile watches it anew, which posts its next
// result.
func (ws *Watches) retryPost(w store.Watch, key, text string, interval time.Duration) {
	ctx := ws.ctx
	for {
		select {
		case <-ctx.Done():
			ws.done(key)
			return
		case <-time.After(interval):
		}
		cur, err := ws.store.Watch(ctx, w.Contact, w.TaskID)
		switch {
		case errors.Is(err, store.ErrNotFound):
			ws.done(key)
			ws.endToCaller(w, reasonRemoved)
			return
		case err != nil:
			slog.Warn("reading a watch", "agent", w.Contact, "task", w.TaskID, "error", err)
			continue
		case cur.Generation != w.Generation:
			ws.done(key)
			ws.watch(cur)
			return
		case time.Since(cur.Created) >= ws.timings.WatchMaxAge:
			slog.Error("dropped a watched result: the agent refused it until the watch got too old", "contact", w.Contact, "task", w.TaskID, "user", w.OnBehalfOf)
			ws.deleteWatch(ctx, cur)
			ws.done(key)
			ws.endToCaller(w, fmt.Sprintf("its result wasn't accepted within %s", describeWait(ws.timings.WatchMaxAge)))
			return
		}
		user, err := ws.watchPrincipal(ctx, cur)
		if err != nil {
			continue
		}
		if !ws.finishWatch(ctx, cur, key, user, text, interval, true) {
			ws.done(key)
		}
		return
	}
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
	user, err := ws.chain.Stored(ctx, w.OnBehalfOf, w.Identity)
	if err != nil {
		slog.Warn("resolving the user of a watch", "contact", w.Contact, "task", w.TaskID, "user", w.OnBehalfOf, "error", err)
		return auth.Principal{}, err
	}
	return user, nil
}

// maxInlineResult is the size of the longest watched result posted as
// text. A longer one is posted as a file, with its first lines inline, so
// that a contact can't flood the conversation.
const maxInlineResult = 16 << 10

// The inline part of a result posted as a file.
const (
	previewLines = 20
	previewBytes = 2 << 10
)

const resultFile = "result.txt"

// resultTurn returns the text and the files of a turn that posts text.
func resultTurn(text string) (string, []messenger.File) {
	if len(text) <= maxInlineResult {
		return text, nil
	}
	lines := strings.SplitAfterN(text, "\n", previewLines+1)
	preview := strings.Join(lines[:min(len(lines), previewLines)], "")
	if len(preview) > previewBytes {
		preview = strings.ToValidUTF8(preview[:previewBytes], "")
	}
	note := fmt.Sprintf("[The full result, %d bytes, is attached as %s.]", len(text), resultFile)
	return strings.TrimRight(preview, "\n") + "\n…\n" + note, []messenger.File{{Name: resultFile, MediaType: "text/plain", Data: []byte(text)}}
}
