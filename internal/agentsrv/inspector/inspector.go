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

// Package inspector serves the owner's web inspector of the agent.
package inspector

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/a2aproject/a2a-serve/internal/a2atext"
	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/agentsrv/approvals"
	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/controlapi"
	"github.com/a2aproject/a2a-serve/internal/cryptoutil"
	"github.com/a2aproject/a2a-serve/internal/pairings"
	"github.com/a2aproject/a2a-serve/internal/store"
	"github.com/a2aproject/a2a-serve/internal/webpage"
)

// Path is where the owner's web inspector is served.
const Path = "/inspect"

const (
	codeTTL    = 5 * time.Minute
	sessionTTL = 12 * time.Hour
	cookieName = "a2a_inspect"
	// recentTasks is how many recent tasks the overview groups into
	// conversations.
	recentTasks = 100
	// fullHistory asks the task store for the whole history of a task.
	fullHistory = math.MaxInt32
)

// Deps are what the inspector reads and calls.
type Deps struct {
	Store     *store.Store
	Tasks     *store.TaskStore
	Approvals *approvals.Service
	// Config returns the current config of the agent.
	Config  func() *agentdir.Config
	BaseURL func() string
	// ListPairings describes the agent's pairings.
	ListPairings func(ctx context.Context) ([]pairings.Pairing, error)
	// Status describes the running server.
	Status func(ctx context.Context) (*controlapi.Status, error)
}

// Inspector serves a page where the owner sees what the agent is doing.
// Only someone with access to the control socket can mint the one-time
// login code which opens a session, so only the owner gets in.
type Inspector struct {
	deps Deps

	mu       sync.Mutex
	codes    map[string]time.Time
	sessions map[string]session
}

type session struct {
	expires time.Time
	csrf    string
}

// New returns an inspector with no sessions.
func New(deps Deps) *Inspector {
	return &Inspector{deps: deps, codes: map[string]time.Time{}, sessions: map[string]session{}}
}

// LoginCode mints a one-time code for /inspect/login.
func (in *Inspector) LoginCode() string {
	code := cryptoutil.RandomString(32, cryptoutil.Alphanumeric)
	in.mu.Lock()
	defer in.mu.Unlock()
	now := time.Now()
	for c, exp := range in.codes {
		if now.After(exp) {
			delete(in.codes, c)
		}
	}
	in.codes[code] = now.Add(codeTTL)
	return code
}

// Register serves the inspector on mux.
func (in *Inspector) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET "+Path+"/login", in.handleLogin)
	mux.HandleFunc("GET "+Path+"/{$}", in.page(in.handleOverview))
	mux.HandleFunc("GET "+Path+"/conversations/{id}", in.page(in.handleConversation))
	mux.HandleFunc("POST "+Path+"/questions/{id}", in.page(in.handleAnswer))
	mux.HandleFunc("POST "+Path+"/tasks/{id}/answer", in.page(in.handleTaskAnswer))
}

func (in *Inspector) handleLogin(w http.ResponseWriter, r *http.Request) {
	webpage.SetSecurityHeaders(w)
	code := r.URL.Query().Get("code")
	in.mu.Lock()
	exp, ok := in.codes[code]
	delete(in.codes, code)
	var token string
	if ok && time.Now().Before(exp) {
		in.dropExpiredSessions(time.Now())
		token = cryptoutil.RandomString(40, cryptoutil.Alphanumeric)
		in.sessions[token] = session{expires: time.Now().Add(sessionTTL), csrf: cryptoutil.RandomString(32, cryptoutil.Alphanumeric)}
	}
	in.mu.Unlock()
	if token == "" {
		http.Error(w, "This login link is invalid or was used already. Run `a2a-serve inspect` for a new one.", http.StatusUnauthorized)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: token, Path: Path, HttpOnly: true, Secure: r.TLS != nil,
		SameSite: http.SameSiteStrictMode, MaxAge: int(sessionTTL.Seconds()),
	})
	http.Redirect(w, r, Path+"/", http.StatusSeeOther)
}

// dropExpiredSessions forgets the sessions that ended. The caller holds in.mu.
func (in *Inspector) dropExpiredSessions(now time.Time) {
	for token, sess := range in.sessions {
		if now.After(sess.expires) {
			delete(in.sessions, token)
		}
	}
}

// page requires a session, and for POSTs its CSRF token.
func (in *Inspector) page(h func(http.ResponseWriter, *http.Request, session)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		webpage.SetSecurityHeaders(w)
		c, err := r.Cookie(cookieName)
		if err != nil {
			http.Error(w, "Open the inspector with `a2a-serve inspect`.", http.StatusUnauthorized)
			return
		}
		in.mu.Lock()
		sess, ok := in.sessions[c.Value]
		if ok && time.Now().After(sess.expires) {
			delete(in.sessions, c.Value)
			ok = false
		}
		in.mu.Unlock()
		if !ok {
			http.Error(w, "Your session ended. Open the inspector again with `a2a-serve inspect`.", http.StatusUnauthorized)
			return
		}
		if r.Method == http.MethodPost && subtle.ConstantTimeCompare([]byte(r.FormValue("csrf")), []byte(sess.csrf)) != 1 {
			http.Error(w, "Invalid form. Reload the page and try again.", http.StatusForbidden)
			return
		}
		h(w, r, sess)
	}
}

// Approval is a question waiting for the owner's answer.
type Approval struct {
	// Action is where the answer is posted.
	Action string
	// Question is the ID of the task's question that the answer is for,
	// and Seq its number, set when the answer is posted on the task.
	Question string
	Seq      int
	Text     string
	// ContextID is this agent's conversation the question belongs to, or
	// empty if the inspector doesn't know it.
	ContextID string
	Contact   string
	Options   []approvals.Option
	Created   time.Time
}

type conversation struct {
	ContextID string
	User      string
	Chat      string
	State     string
	Snippet   string
	Tasks     int
	Updated   time.Time
}

type overview struct {
	Agent         string
	CSRF          string
	Status        *controlapi.Status
	Approvals     []Approval
	Conversations []conversation
	Pairings      []pairings.Pairing
	Nodes         []pairings.Node
	Contacts      []store.Contact
}

func (in *Inspector) handleOverview(w http.ResponseWriter, r *http.Request, sess session) {
	ctx := r.Context()
	page, err := in.overview(ctx)
	if err != nil {
		slog.Error("rendering the inspector", "error", err)
		http.Error(w, "Couldn't load the agent's state: "+err.Error(), http.StatusInternalServerError)
		return
	}
	page.CSRF = sess.csrf
	render(w, "overview", page)
}

func (in *Inspector) overview(ctx context.Context) (*overview, error) {
	st, err := in.deps.Status(ctx)
	if err != nil {
		return nil, err
	}
	page := &overview{Agent: st.Name, Status: st}
	if page.Approvals, err = in.PendingApprovals(ctx); err != nil {
		return nil, err
	}
	if page.Conversations, err = in.conversations(ctx); err != nil {
		return nil, err
	}
	if page.Pairings, err = in.deps.ListPairings(ctx); err != nil {
		return nil, err
	}
	nodes, err := in.deps.Store.Nodes(ctx)
	if err != nil {
		return nil, err
	}
	for _, n := range nodes {
		page.Nodes = append(page.Nodes, pairings.NodeInfo(in.deps.BaseURL(), n))
	}
	if page.Contacts, err = in.deps.Store.Contacts(ctx); err != nil {
		return nil, err
	}
	return page, nil
}

// PendingApprovals are the questions of this agent's tasks waiting for
// input, and the unanswered questions of remote agents relayed here. A
// question which was sent to a chat has a record, answered like the chat
// would; one asked of an A2A caller is answered on its task.
func (in *Inspector) PendingApprovals(ctx context.Context) ([]Approval, error) {
	recs, err := in.deps.Store.UnansweredQuestions(ctx)
	if err != nil {
		return nil, err
	}
	type taskQuestion struct{ taskID, hitlID string }
	// Records come newest first, and the newest record of a question wins.
	records := map[taskQuestion]store.Question{}
	var out []Approval
	for _, rec := range recs {
		if rec.Contact == "" {
			key := taskQuestion{rec.TaskID, rec.HitlID}
			if _, ok := records[key]; !ok {
				records[key] = rec
			}
			continue
		}
		q := approvals.FromRecord(rec)
		a := Approval{Action: Path + "/questions/" + rec.ID, Text: q.Display(), Contact: rec.Contact, Options: q.Options, Created: rec.Created}
		// The record has the other agent's context; the watch of its task
		// has the conversation here that called it.
		w, err := in.deps.Store.Watch(ctx, rec.Contact, rec.TaskID)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return nil, err
		}
		a.ContextID = w.OriginContextID
		out = append(out, a)
	}
	tasks, err := waitingTasks(ctx, in.deps.Tasks)
	if err != nil {
		return nil, err
	}
	for _, t := range tasks {
		q := approvals.FromStatus(t.Status)
		a := Approval{Action: Path + "/tasks/" + string(t.ID) + "/answer", Question: q.ID, Seq: q.Seq, Text: q.Display(), ContextID: t.ContextID, Options: q.Options}
		if t.Status.Timestamp != nil {
			a.Created = *t.Status.Timestamp
		}
		if rec, ok := records[taskQuestion{string(t.ID), q.ID}]; ok {
			a.Action, a.Question, a.Seq = Path+"/questions/"+rec.ID, "", 0
		}
		out = append(out, a)
	}
	return out, nil
}

// ApprovalCount is the number of [Inspector.PendingApprovals] of the agent
// with the store st and the task store tasks.
func ApprovalCount(ctx context.Context, st *store.Store, tasks *store.TaskStore) (int, error) {
	remote, err := st.UnansweredRemoteQuestions(ctx)
	if err != nil {
		return 0, err
	}
	waiting, err := waitingTasks(ctx, tasks)
	if err != nil {
		return 0, err
	}
	return remote + len(waiting), nil
}

// waitingTasks are the tasks waiting for an answer to a question, without
// their history and artifacts.
func waitingTasks(ctx context.Context, tasks *store.TaskStore) ([]*a2a.Task, error) {
	noHistory := 0
	resp, err := tasks.List(ctx, &a2a.ListTasksRequest{Status: a2a.TaskStateInputRequired, PageSize: 100, HistoryLength: &noHistory})
	if err != nil {
		return nil, err
	}
	var out []*a2a.Task
	for _, t := range resp.Tasks {
		if approvals.FromStatus(t.Status) != nil {
			out = append(out, t)
		}
	}
	return out, nil
}

// conversations groups the recent tasks by context, the most recent first.
func (in *Inspector) conversations(ctx context.Context) ([]conversation, error) {
	tasks, err := in.deps.Tasks.ListSummaries(ctx, &a2a.ListTasksRequest{PageSize: recentTasks})
	if err != nil {
		return nil, err
	}
	var out []conversation
	index := map[string]int{}
	for _, sum := range tasks {
		t := sum.Task
		if j, ok := index[t.ContextID]; ok {
			out[j].Tasks++
			if sum.Prompt != nil {
				out[j].Snippet = snippet(a2atext.Message(sum.Prompt))
			}
			continue
		}
		c := conversation{ContextID: t.ContextID, User: auth.DisplayPrincipal(sum.Owner), State: a2atext.StateName(t.Status.State), Tasks: 1}
		if t.Status.Timestamp != nil {
			c.Updated = *t.Status.Timestamp
		}
		if sum.Prompt != nil {
			c.Snippet = snippet(a2atext.Message(sum.Prompt))
		}
		if ch, _, err := in.deps.Store.ConvOf(ctx, t.ContextID); err == nil {
			c.Chat = ch
		} else if !errors.Is(err, store.ErrNotFound) {
			return nil, err
		}
		index[t.ContextID] = len(out)
		out = append(out, c)
	}
	return out, nil
}

func snippet(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > 100 {
		return string(r[:100]) + "…"
	}
	return s
}

type message struct {
	Role string
	Text string
}

type task struct {
	ID        string
	State     string
	User      string
	Updated   time.Time
	Messages  []message
	Artifacts []message
}

type conversationPage struct {
	Agent     string
	ContextID string
	Chat      string
	Tasks     []task
}

func (in *Inspector) handleConversation(w http.ResponseWriter, r *http.Request, _ session) {
	ctx := r.Context()
	id := r.PathValue("id")
	history := fullHistory
	resp, owners, err := in.deps.Tasks.ListWithOwners(ctx, &a2a.ListTasksRequest{ContextID: id, PageSize: 100, HistoryLength: &history, IncludeArtifacts: true})
	if err != nil {
		http.Error(w, "Couldn't load the conversation: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if len(resp.Tasks) == 0 {
		http.Error(w, "No such conversation.", http.StatusNotFound)
		return
	}
	page := conversationPage{Agent: in.deps.Config().Name, ContextID: id}
	if ch, conv, err := in.deps.Store.ConvOf(ctx, id); err == nil {
		page.Chat = ch + " " + conv
	}
	for i, t := range slices.Backward(resp.Tasks) {
		it := task{ID: string(t.ID), State: a2atext.StateName(t.Status.State), User: auth.DisplayPrincipal(owners[i])}
		if t.Status.Timestamp != nil {
			it.Updated = *t.Status.Timestamp
		}
		for _, m := range t.History {
			it.Messages = append(it.Messages, message{Role: roleName(m.Role), Text: partsText(m.Parts, "\n")})
		}
		if m := t.Status.Message; m != nil && !slices.ContainsFunc(t.History, func(h *a2a.Message) bool { return h.ID == m.ID }) {
			it.Messages = append(it.Messages, message{Role: roleName(m.Role), Text: partsText(m.Parts, "\n")})
		}
		for _, a := range t.Artifacts {
			it.Artifacts = append(it.Artifacts, message{Role: a.Name, Text: partsText(a.Parts, "")})
		}
		page.Tasks = append(page.Tasks, it)
	}
	render(w, "conversation", page)
}

func roleName(r a2a.MessageRole) string {
	if r == a2a.MessageRoleAgent {
		return "agent"
	}
	return "user"
}

// partsText shows parts as text: text as is, files by name and data as
// JSON, joined with sep. The parts of a streamed artifact are chunks, joined
// with "".
func partsText(parts []*a2a.Part, sep string) string {
	var out []string
	for _, p := range parts {
		switch {
		case p.Text() != "":
			out = append(out, p.Text())
		case p.URL() != "":
			out = append(out, fmt.Sprintf("[link %s]", p.URL()))
		case p.Filename != "" || p.MediaType != "":
			out = append(out, fmt.Sprintf("[file %s %s]", p.Filename, p.MediaType))
		case p.Data() != nil:
			data, err := json.Marshal(p.Data())
			if err != nil {
				data = []byte("[data]")
			}
			out = append(out, string(data))
		}
	}
	return strings.Join(out, sep)
}

func (in *Inspector) handleAnswer(w http.ResponseWriter, r *http.Request, _ session) {
	ctx := r.Context()
	rec, err := in.deps.Store.QuestionByID(ctx, r.PathValue("id"))
	if err != nil {
		http.Error(w, "This approval does not exist.", http.StatusNotFound)
		return
	}
	q := approvals.FromRecord(rec)
	opt, ok := q.Match(r.FormValue("option"))
	if !ok {
		http.Error(w, "Unknown option.", http.StatusBadRequest)
		return
	}
	out, err := in.deps.Approvals.Answer(ctx, rec, auth.Owner(), opt, "the inspector")
	in.answered(w, r, out, err, "This approval is no longer open.", "question", rec.ID)
}

// answered renders out, the outcome of an answer from the inspector, or
// err. closed is the text for a question that no longer waits.
func (in *Inspector) answered(w http.ResponseWriter, r *http.Request, out approvals.Outcome, err error, closed string, logArgs ...any) {
	switch {
	case err != nil:
		slog.Error("answering from the inspector", append(logArgs, "error", err)...)
		http.Error(w, "Couldn't send the answer: "+err.Error(), http.StatusInternalServerError)
	case out == approvals.Closed:
		http.Error(w, closed, http.StatusConflict)
	case out == approvals.InProgress:
		http.Error(w, inProgressText, http.StatusConflict)
	default:
		http.Redirect(w, r, Path+"/", http.StatusSeeOther)
	}
}

// inProgressText answers a post that lost to another answer of the task,
// such as one of a double click.
const inProgressText = "The task is already handling another answer. Reload the page to see where it stands."

// handleTaskAnswer answers the question of a task asked of its A2A caller,
// as the owner. It refuses an answer to a question the task no longer asks,
// so that a stale page or a double click can't answer the next question.
func (in *Inspector) handleTaskAnswer(w http.ResponseWriter, r *http.Request, _ session) {
	ctx := r.Context()
	stored, err := in.deps.Tasks.Header(ctx, a2a.TaskID(r.PathValue("id")))
	if err != nil {
		http.Error(w, "This task does not exist.", http.StatusNotFound)
		return
	}
	t := stored.Task
	q := approvals.FromStatus(t.Status)
	if t.Status.State != a2a.TaskStateInputRequired || q == nil {
		http.Error(w, "This task no longer waits for an answer. Reload the page.", http.StatusConflict)
		return
	}
	ref := approvals.Ref{ID: r.FormValue("question"), Seq: formSeq(r)}
	switch m := q.Check(ref); {
	case m == approvals.Earlier:
		http.Error(w, "This question was already answered, and the task asks another one now. Reload the page to see it.", http.StatusConflict)
		return
	case ref.ID == "" || m != approvals.Matches:
		http.Error(w, "This task asks a different question now. Reload the page to see it.", http.StatusConflict)
		return
	}
	opt, ok := q.Match(r.FormValue("option"))
	if !ok {
		http.Error(w, "Unknown option.", http.StatusBadRequest)
		return
	}
	out, err := in.deps.Approvals.AnswerTask(ctx, t.ID, t.ContextID, q, auth.Owner(), opt, "the inspector")
	in.answered(w, r, out, err, "This task no longer waits for an answer. Reload the page.", "task", t.ID)
}

// formSeq is the number of the question in the form of an answer: 0 when
// the form has none, as on a page loaded before questions had numbers, and
// negative when it isn't a number.
func formSeq(r *http.Request) int {
	v := r.FormValue("seq")
	if v == "" {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return -1
	}
	return n
}
