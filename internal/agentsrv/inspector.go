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
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/a2aproject/a2a-serve/internal/agentsrv/approvals"
	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/controlapi"
	"github.com/a2aproject/a2a-serve/internal/cryptoutil"
	"github.com/a2aproject/a2a-serve/internal/store"
)

// InspectorPath is where the owner's web inspector is served.
const InspectorPath = "/inspect"

const (
	inspectorCodeTTL    = 5 * time.Minute
	inspectorSessionTTL = 12 * time.Hour
	inspectorCookie     = "a2a_inspect"
	// inspectorTasks is how many recent tasks the overview groups into
	// conversations.
	inspectorTasks = 100
)

// inspector serves a page where the owner sees what the agent is doing.
// Only someone with access to the control socket can mint the one-time
// login code which opens a session, so only the owner gets in.
type inspector struct {
	s *Server

	mu       sync.Mutex
	codes    map[string]time.Time
	sessions map[string]inspectorSession
}

type inspectorSession struct {
	expires time.Time
	csrf    string
}

func newInspector(s *Server) *inspector {
	return &inspector{s: s, codes: map[string]time.Time{}, sessions: map[string]inspectorSession{}}
}

// loginCode mints a one-time code for /inspect/login.
func (in *inspector) loginCode() string {
	code := cryptoutil.RandomString(32, cryptoutil.Alphanumeric)
	in.mu.Lock()
	defer in.mu.Unlock()
	now := time.Now()
	for c, exp := range in.codes {
		if now.After(exp) {
			delete(in.codes, c)
		}
	}
	in.codes[code] = now.Add(inspectorCodeTTL)
	return code
}

func (in *inspector) register(mux *http.ServeMux) {
	mux.HandleFunc("GET "+InspectorPath+"/login", in.handleLogin)
	mux.HandleFunc("GET "+InspectorPath+"/{$}", in.page(in.handleOverview))
	mux.HandleFunc("GET "+InspectorPath+"/conversations/{id}", in.page(in.handleConversation))
	mux.HandleFunc("POST "+InspectorPath+"/questions/{id}", in.page(in.handleAnswer))
	mux.HandleFunc("POST "+InspectorPath+"/tasks/{id}/answer", in.page(in.handleTaskAnswer))
}

func setInspectorHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; frame-ancestors 'none'")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Cache-Control", "no-store")
}

func (in *inspector) handleLogin(w http.ResponseWriter, r *http.Request) {
	setInspectorHeaders(w)
	code := r.URL.Query().Get("code")
	in.mu.Lock()
	exp, ok := in.codes[code]
	delete(in.codes, code)
	var token string
	if ok && time.Now().Before(exp) {
		token = cryptoutil.RandomString(40, cryptoutil.Alphanumeric)
		in.sessions[token] = inspectorSession{expires: time.Now().Add(inspectorSessionTTL), csrf: cryptoutil.RandomString(32, cryptoutil.Alphanumeric)}
	}
	in.mu.Unlock()
	if token == "" {
		http.Error(w, "This login link is invalid or was used already. Run `a2a-serve inspect` for a new one.", http.StatusUnauthorized)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: inspectorCookie, Value: token, Path: InspectorPath, HttpOnly: true, Secure: r.TLS != nil,
		SameSite: http.SameSiteStrictMode, MaxAge: int(inspectorSessionTTL.Seconds()),
	})
	http.Redirect(w, r, InspectorPath+"/", http.StatusSeeOther)
}

// page requires a session, and for POSTs its CSRF token.
func (in *inspector) page(h func(http.ResponseWriter, *http.Request, inspectorSession)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		setInspectorHeaders(w)
		c, err := r.Cookie(inspectorCookie)
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

type inspectorApproval struct {
	// Action is where the answer is posted.
	Action    string
	Text      string
	ContextID string
	Contact   string
	Options   []approvals.Option
	Created   time.Time
}

type inspectorConversation struct {
	ContextID string
	User      string
	Chat      string
	State     string
	Snippet   string
	Tasks     int
	Updated   time.Time
}

type inspectorOverview struct {
	Agent         string
	CSRF          string
	Status        *controlapi.Status
	Approvals     []inspectorApproval
	Conversations []inspectorConversation
	Pairings      []controlapi.Pairing
	Nodes         []controlapi.NodeInfo
	Contacts      []store.Contact
}

func (in *inspector) handleOverview(w http.ResponseWriter, r *http.Request, sess inspectorSession) {
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

func (in *inspector) overview(ctx context.Context) (*inspectorOverview, error) {
	s := in.s
	st, err := s.status(ctx)
	if err != nil {
		return nil, err
	}
	page := &inspectorOverview{Agent: st.Name, Status: st}
	if page.Approvals, err = in.pendingApprovals(ctx); err != nil {
		return nil, err
	}
	if page.Conversations, err = in.conversations(ctx); err != nil {
		return nil, err
	}
	if page.Pairings, err = s.pairings.List(ctx); err != nil {
		return nil, err
	}
	nodes, err := s.store.Nodes(ctx)
	if err != nil {
		return nil, err
	}
	for _, n := range nodes {
		page.Nodes = append(page.Nodes, s.nodes.Info(n))
	}
	if page.Contacts, err = s.store.Contacts(ctx); err != nil {
		return nil, err
	}
	return page, nil
}

// pendingApprovals are the questions of this agent's tasks waiting for
// input, and the unanswered questions of remote agents relayed here. A
// question which was sent to a chat has a record, answered like the chat
// would; one asked of an A2A caller is answered on its task.
func (in *inspector) pendingApprovals(ctx context.Context) ([]inspectorApproval, error) {
	recs, err := in.s.store.UnansweredQuestions(ctx)
	if err != nil {
		return nil, err
	}
	byTask := map[string]store.Question{}
	var out []inspectorApproval
	for _, rec := range recs {
		if rec.Contact == "" {
			byTask[rec.TaskID] = rec
			continue
		}
		q := approvals.FromRecord(rec)
		out = append(out, inspectorApproval{Action: InspectorPath + "/questions/" + rec.ID, Text: q.Display(), ContextID: rec.ContextID, Contact: rec.Contact, Options: q.Options, Created: rec.Created})
	}
	resp, err := in.s.tasks.List(ctx, &a2a.ListTasksRequest{Status: a2a.TaskStateInputRequired, PageSize: 100})
	if err != nil {
		return nil, err
	}
	for _, t := range resp.Tasks {
		q := approvals.FromStatus(t.Status)
		if q == nil {
			continue
		}
		a := inspectorApproval{Action: InspectorPath + "/tasks/" + string(t.ID) + "/answer", Text: q.Display(), ContextID: t.ContextID, Options: q.Options}
		if t.Status.Timestamp != nil {
			a.Created = *t.Status.Timestamp
		}
		if rec, ok := byTask[string(t.ID)]; ok {
			a.Action = InspectorPath + "/questions/" + rec.ID
		}
		out = append(out, a)
	}
	return out, nil
}

// conversations groups the recent tasks by context, the most recent first.
func (in *inspector) conversations(ctx context.Context) ([]inspectorConversation, error) {
	history := 1
	resp, err := in.s.tasks.List(ctx, &a2a.ListTasksRequest{PageSize: inspectorTasks, HistoryLength: &history})
	if err != nil {
		return nil, err
	}
	var out []inspectorConversation
	index := map[string]int{}
	for _, t := range resp.Tasks {
		if i, ok := index[t.ContextID]; ok {
			out[i].Tasks++
			if len(t.History) > 0 {
				out[i].Snippet = snippet(messageText(t.History[0]))
			}
			continue
		}
		stored, err := in.s.tasks.Get(ctx, t.ID)
		if err != nil {
			return nil, err
		}
		c := inspectorConversation{ContextID: t.ContextID, User: auth.DisplayPrincipal(stored.User), State: stateName(t.Status.State), Tasks: 1}
		if t.Status.Timestamp != nil {
			c.Updated = *t.Status.Timestamp
		}
		if len(t.History) > 0 {
			c.Snippet = snippet(messageText(t.History[0]))
		}
		if ch, _, err := in.s.store.ConvOf(ctx, t.ContextID); err == nil {
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

type inspectorMessage struct {
	Role string
	Text string
}

type inspectorTask struct {
	ID        string
	State     string
	User      string
	Updated   time.Time
	Messages  []inspectorMessage
	Artifacts []inspectorMessage
}

type inspectorConversationPage struct {
	Agent     string
	ContextID string
	Chat      string
	Tasks     []inspectorTask
}

func (in *inspector) handleConversation(w http.ResponseWriter, r *http.Request, _ inspectorSession) {
	ctx := r.Context()
	id := r.PathValue("id")
	resp, err := in.s.tasks.List(ctx, &a2a.ListTasksRequest{ContextID: id, PageSize: 100, IncludeArtifacts: true})
	if err != nil {
		http.Error(w, "Couldn't load the conversation: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if len(resp.Tasks) == 0 {
		http.Error(w, "No such conversation.", http.StatusNotFound)
		return
	}
	page := inspectorConversationPage{Agent: in.s.config().Name, ContextID: id}
	if ch, conv, err := in.s.store.ConvOf(ctx, id); err == nil {
		page.Chat = ch + " " + conv
	}
	for _, t := range slices.Backward(resp.Tasks) {
		it := inspectorTask{ID: string(t.ID), State: stateName(t.Status.State)}
		if stored, err := in.s.tasks.Get(ctx, t.ID); err == nil {
			it.User = auth.DisplayPrincipal(stored.User)
		}
		if t.Status.Timestamp != nil {
			it.Updated = *t.Status.Timestamp
		}
		for _, m := range t.History {
			it.Messages = append(it.Messages, inspectorMessage{Role: roleName(m.Role), Text: partsText(m.Parts, "\n")})
		}
		if m := t.Status.Message; m != nil && !slices.ContainsFunc(t.History, func(h *a2a.Message) bool { return h.ID == m.ID }) {
			it.Messages = append(it.Messages, inspectorMessage{Role: roleName(m.Role), Text: partsText(m.Parts, "\n")})
		}
		for _, a := range t.Artifacts {
			it.Artifacts = append(it.Artifacts, inspectorMessage{Role: a.Name, Text: partsText(a.Parts, "")})
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

func (in *inspector) handleAnswer(w http.ResponseWriter, r *http.Request, _ inspectorSession) {
	ctx := r.Context()
	rec, err := in.s.store.QuestionByID(ctx, r.PathValue("id"))
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
	err = in.s.approvals.Answer(ctx, rec, auth.Owner(), opt)
	if errors.Is(err, approvals.ErrClosed) {
		in.s.approvals.Close(ctx, rec.ID)
		http.Error(w, "This approval is no longer open.", http.StatusConflict)
		return
	}
	if err != nil && !errors.Is(err, store.ErrAnswered) {
		slog.Error("answering a question from the inspector", "question", rec.ID, "error", err)
		http.Error(w, "Couldn't send the answer: "+err.Error(), http.StatusInternalServerError)
		return
	}
	in.s.router.MarkAnswered(ctx, rec.ID, opt.Label, "the inspector")
	http.Redirect(w, r, InspectorPath+"/", http.StatusSeeOther)
}

// handleTaskAnswer answers the question of a task asked of its A2A caller,
// as the owner.
func (in *inspector) handleTaskAnswer(w http.ResponseWriter, r *http.Request, _ inspectorSession) {
	ctx := r.Context()
	stored, err := in.s.tasks.Get(ctx, a2a.TaskID(r.PathValue("id")))
	if err != nil {
		http.Error(w, "This task does not exist.", http.StatusNotFound)
		return
	}
	t := stored.Task
	q := approvals.FromStatus(t.Status)
	if t.Status.State != a2a.TaskStateInputRequired || q == nil {
		http.Redirect(w, r, InspectorPath+"/", http.StatusSeeOther)
		return
	}
	opt, ok := q.Match(r.FormValue("option"))
	if !ok {
		http.Error(w, "Unknown option.", http.StatusBadRequest)
		return
	}
	msg, err := approvals.AnswerMessage(t.ID, t.ContextID, q.ID, opt.ID)
	if err == nil {
		_, err = in.s.handler.SendMessage(auth.InProcess(ctx, auth.Owner()), &a2a.SendMessageRequest{Message: msg, Config: &a2a.SendMessageConfig{ReturnImmediately: true}})
	}
	if err != nil {
		slog.Error("answering a task from the inspector", "task", t.ID, "error", err)
		http.Error(w, "Couldn't send the answer: "+err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, InspectorPath+"/", http.StatusSeeOther)
}
