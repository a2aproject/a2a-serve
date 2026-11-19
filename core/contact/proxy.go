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

package contact

import (
	"cmp"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/a2aproject/a2a-serve/internal/cardinfo"
	"github.com/a2aproject/a2a-serve/internal/jsonhttp"
)

// keepAlive is the interval of the keep-alive comments of event streams.
const keepAlive = 15 * time.Second

// Config configures a [Proxy].
type Config struct {
	Turns     Turns
	Directory Directory
	// Calls records the tasks of each conversation; nil keeps them in
	// memory: at most 1000 calls per conversation, of the conversations
	// that called last.
	Calls Calls
	// Followup takes the tasks that calls leave unfinished. Without one,
	// nobody follows them, and a contact's task that waits for an approval
	// waits until it times out or is canceled, as the agent may not answer
	// it.
	Followup Followup
	// Scoped serves contacts at /<scope>/<name>, for one agent process
	// that serves many conversations. Otherwise they are at /<name>.
	Scoped bool
	// Log receives the records of the proxy; nil uses the default logger.
	Log *slog.Logger
}

// Proxy serves the contacts of the agent on its agent API, as A2A agents.
type Proxy struct {
	turns    Turns
	dir      Directory
	calls    Calls
	followup Followup
	scoped   bool
	log      *slog.Logger
	mux      http.Handler
}

var _ http.Handler = (*Proxy)(nil)

// New returns the proxy of cfg.
func New(cfg Config) (*Proxy, error) {
	if cfg.Turns == nil || cfg.Directory == nil {
		return nil, errors.New("contact: a proxy needs Turns and a Directory")
	}
	p := &Proxy{turns: cfg.Turns, dir: cfg.Directory, calls: cfg.Calls, followup: cfg.Followup, scoped: cfg.Scoped, log: cfg.Log}
	if p.calls == nil {
		p.calls = newMemoryCalls()
	}
	if p.log == nil {
		p.log = slog.Default()
	}
	mux := http.NewServeMux()
	if p.scoped {
		mux.HandleFunc("/{scope}/{name}", p.serve)
		mux.HandleFunc("/{scope}/{name}/{rest...}", p.serve)
	} else {
		mux.HandleFunc("/{name}", p.serve)
		mux.HandleFunc("/{name}/{rest...}", p.serve)
	}
	p.mux = jsonhttp.LimitBody(mux)
	return p, nil
}

// ServeHTTP serves a request of the agent.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) { p.mux.ServeHTTP(w, r) }

func (p *Proxy) serve(w http.ResponseWriter, r *http.Request) {
	scope, name := r.PathValue("scope"), r.PathValue("name")
	t, err := p.turns.Turn(r, scope)
	switch {
	case errors.Is(err, ErrUnauthenticated):
		jsonhttp.Error(w, http.StatusUnauthorized, err.Error())
		return
	case errors.Is(err, ErrNotFound):
		jsonhttp.Error(w, http.StatusNotFound, err.Error())
		return
	case errors.Is(err, ErrNoTurn):
		jsonhttp.Error(w, http.StatusForbidden, err.Error())
		return
	case err != nil && !errors.Is(err, ErrRefused):
		p.log.Error("finding the turn of a call to a contact", "error", err)
		jsonhttp.Error(w, http.StatusInternalServerError, "finding the turn of the call failed")
		return
	}
	base := "http://" + r.Host + "/" + name
	if p.scoped {
		base = "http://" + r.Host + "/" + scope + "/" + name
	}
	rest := "/" + r.PathValue("rest")
	isCard := r.Method == http.MethodGet && (rest == "/" || rest == "/.well-known/agent-card.json")
	if err == nil && t.Hops+1 > MaxHops {
		err = Refusef("too many agents in this chain (%d); answer without calling another agent", t.Hops)
	}
	var card *a2a.AgentCard
	if err == nil {
		card, err = p.dir.Card(r.Context(), t, name)
	}
	if errors.Is(err, ErrRefused) {
		serveRefusal(w, r, name, base, rest, a2a.NewError(a2a.ErrUnauthorized, err.Error()))
		return
	}
	if err != nil {
		p.log.Error("looking up a contact", "contact", name, "error", err)
		jsonhttp.Error(w, http.StatusInternalServerError, "looking up "+name+" failed")
		return
	}
	if isCard {
		a2asrv.NewStaticAgentCardHandler(proxyCard(name, base, card)).ServeHTTP(w, r)
		return
	}
	target, err := p.dir.Target(r.Context(), t, name)
	if err != nil {
		p.log.Warn("reaching a contact for the agent", "contact", name, "error", err)
		jsonhttp.Error(w, http.StatusBadGateway, err.Error())
		return
	}
	serveA2A(w, r, rest, p.Guard(t, name, target), a2asrv.WithTransportKeepAlive(keepAlive))
}

func serveA2A(w http.ResponseWriter, r *http.Request, rest string, h a2asrv.RequestHandler, opts ...a2asrv.TransportOption) {
	r2 := r.Clone(r.Context())
	r2.URL.Path, r2.URL.RawPath = rest, ""
	inner := a2asrv.NewRESTHandler(h, opts...)
	if r.Method == http.MethodPost && rest == "/" {
		inner = a2asrv.NewJSONRPCHandler(h, opts...)
	}
	inner.ServeHTTP(w, r2)
}

// serveRefusal serves a card for name and fails every call with refusal.
// A2A clients fetch the card before the call and report a failed card fetch
// by its status alone, so the refusal travels on the call, where the client
// shows its message.
func serveRefusal(w http.ResponseWriter, r *http.Request, name, base, rest string, refusal *a2a.Error) {
	if r.Method == http.MethodGet && (rest == "/" || rest == "/.well-known/agent-card.json") {
		card := proxyCard(name, base, nil)
		card.Description = refusal.Message
		a2asrv.NewStaticAgentCardHandler(card).ServeHTTP(w, r)
		return
	}
	serveA2A(w, r, rest, refusingHandler{err: refusal})
}

// proxyCard is the card that the agent reads for the contact name at base:
// the contact's description and skills from card, shortened as untrusted
// text, and the proxy's interfaces.
func proxyCard(name, base string, card *a2a.AgentCard) *a2a.AgentCard {
	out := &a2a.AgentCard{
		Name:        name,
		Description: name + ", called through the server hosting you.",
		Version:     "1.0.0",
		SupportedInterfaces: []*a2a.AgentInterface{
			a2a.NewAgentInterface(base, a2a.TransportProtocolJSONRPC),
			a2a.NewAgentInterface(base, a2a.TransportProtocolHTTPJSON),
		},
		Capabilities:       a2a.AgentCapabilities{Streaming: true},
		DefaultInputModes:  []string{"text/plain", "application/json"},
		DefaultOutputModes: []string{"text/plain", "application/json"},
	}
	if short := shorten(card); short != nil {
		if short.Description != "" {
			out.Description = name + `, called through the server hosting you. It says of itself: "` + short.Description + `"`
		}
		out.Skills = short.Skills
	}
	return out
}

// shorten returns the description and skills of card, which come from
// another agent, as short plain text that an agent may read: bounded, on
// one line, and without markdown. It returns nil for a nil card.
func shorten(card *a2a.AgentCard) *a2a.AgentCard {
	if card == nil {
		return nil
	}
	s := cardinfo.Of(card)
	out := &a2a.AgentCard{Name: cardinfo.Clean(card.Name, cardinfo.MaxSkillName), Description: s.Description}
	for _, sk := range s.Skills {
		out.Skills = append(out.Skills, a2a.AgentSkill{ID: sk.ID, Name: cmp.Or(sk.Name, sk.ID), Description: sk.Description, Tags: []string{}})
	}
	return out
}

// CancelCalls cancels the tasks that the turn t started at contacts, which
// are still running: call it once the turn is canceled and starts no more.
// A task that already ended, or that the contact no longer has, counts as
// canceled. It returns the calls it canceled, and an error that joins the
// ones it couldn't.
func (p *Proxy) CancelCalls(ctx context.Context, t Turn) ([]Call, error) {
	calls, err := p.calls.OfTurn(ctx, t)
	if err != nil {
		return nil, err
	}
	canceled, err := cancelCalls(ctx, t, calls, func(ctx context.Context, c Call) error {
		target, err := p.dir.Target(ctx, t, c.Contact)
		if err != nil {
			return err
		}
		return target.cancel(ctx, t, c.TaskID)
	})
	if mc, ok := p.calls.(*memoryCalls); ok {
		mc.endTurn(t)
	}
	return canceled, err
}
