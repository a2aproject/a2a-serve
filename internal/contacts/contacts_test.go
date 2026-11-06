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

package contacts

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	gocmp "github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/agenterr"
	"github.com/a2aproject/a2a-serve/internal/cardinfo"
	"github.com/a2aproject/a2a-serve/internal/store"
)

func TestTokenClientKeepsRedirectPolicy(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/final", http.StatusFound)
	})
	mux.HandleFunc("/final", func(w http.ResponseWriter, r *http.Request) {})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	base := &http.Client{Transport: srv.Client().Transport, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}

	resp, err := TokenClient(base, store.Contact{URL: srv.URL}).Get(srv.URL)
	if err != nil {
		t.Fatalf("TokenClient().Get() error = %v", err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatalf("closing the response: %v", err)
	}

	if resp.StatusCode != http.StatusFound {
		t.Fatalf("TokenClient().Get() status = %d, want %d from the base client's redirect policy", resp.StatusCode, http.StatusFound)
	}
}

// cardServer serves the agent card that set last stored.
type cardServer struct {
	*httptest.Server
	mu   sync.Mutex
	card *a2a.AgentCard
}

func newCardServer(t *testing.T, card *a2a.AgentCard) *cardServer {
	t.Helper()
	cs := &cardServer{card: card}
	cs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cs.mu.Lock()
		defer cs.mu.Unlock()
		if err := json.NewEncoder(w).Encode(cs.card); err != nil {
			t.Errorf("encoding the card: %v", err)
		}
	}))
	t.Cleanup(cs.Close)
	return cs
}

func (cs *cardServer) set(card *a2a.AgentCard) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	cs.card = card
}

func openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Error(err)
		}
	})
	return st
}

func TestAddStoresCard(t *testing.T) {
	t.Parallel()
	srv := newCardServer(t, &a2a.AgentCard{Name: "coder", Description: "Writes Go.\n# heading", Skills: []a2a.AgentSkill{{ID: "review", Name: "Code review"}}})
	st := openStore(t)
	m := Service{Store: st, HTTPClient: func(store.Contact) *http.Client { return srv.Client() }}

	got, err := m.Add(context.Background(), Request{Name: "coder", URL: srv.URL})
	if err != nil {
		t.Fatalf("Service.Add() error = %v", err)
	}
	want := cardinfo.Summary{Description: "Writes Go. heading", Skills: []cardinfo.Skill{{ID: "review", Name: "Code review"}}}
	if diff := gocmp.Diff(want, got.Card); diff != "" {
		t.Fatalf("Service.Add() wrong card (-want +got) diff = %s", diff)
	}
	stored, err := st.Contact(context.Background(), "coder")
	if err != nil {
		t.Fatalf("Contact() error = %v", err)
	}
	if diff := gocmp.Diff(want, stored.Card); diff != "" {
		t.Fatalf("Contact() wrong card (-want +got) diff = %s", diff)
	}
}

func TestRefresh(t *testing.T) {
	t.Parallel()
	before := &a2a.AgentCard{Name: "coder", Description: "Writes Go."}
	tests := []struct {
		name        string
		next        *a2a.AgentCard
		names       []string
		via         string
		offline     bool
		wantCard    cardinfo.Summary
		wantChanged bool
		wantErr     error
	}{
		{name: "unchanged", next: before, wantCard: cardinfo.Summary{Description: "Writes Go."}},
		{name: "changed", next: &a2a.AgentCard{Name: "coder", Description: "Reviews Go."}, wantCard: cardinfo.Summary{Description: "Reviews Go."}, wantChanged: true},
		{name: "by name", next: &a2a.AgentCard{Name: "coder", Description: "Reviews Go."}, names: []string{"coder"}, wantCard: cardinfo.Summary{Description: "Reviews Go."}, wantChanged: true},
		{name: "unknown name", next: before, names: []string{"ben"}, wantCard: cardinfo.Summary{Description: "Writes Go."}, wantErr: &agenterr.NotFoundError{}},
		{name: "private contact of a stopped agent", next: before, via: ViaPrivate, offline: true, wantCard: cardinfo.Summary{Description: "Writes Go."}, wantErr: agenterr.ErrNotRunning},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := newCardServer(t, before)
			st := openStore(t)
			ctx := context.Background()
			via := cmp.Or(tc.via, ViaDirect)
			if err := st.PutContact(ctx, store.Contact{Name: "coder", URL: srv.URL, Via: via, Card: cardinfo.Of(before)}); err != nil {
				t.Fatalf("PutContact() error = %v", err)
			}
			srv.set(tc.next)
			changed := false
			m := Service{Store: st, Changed: func(context.Context) { changed = true }}
			if !tc.offline {
				m.HTTPClient = func(store.Contact) *http.Client { return srv.Client() }
			}

			_, err := m.Refresh(ctx, tc.names...)
			if !errorMatches(err, tc.wantErr) {
				t.Fatalf("Service.Refresh() error = %v, want %v", err, tc.wantErr)
			}
			if changed != tc.wantChanged {
				t.Fatalf("Service.Refresh() called Changed = %v, want %v", changed, tc.wantChanged)
			}
			got, err := st.Contact(ctx, "coder")
			if err != nil {
				t.Fatalf("Contact() error = %v", err)
			}
			if diff := gocmp.Diff(tc.wantCard, got.Card); diff != "" {
				t.Fatalf("Service.Refresh() wrong stored card (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestRefreshReportsUnreachableContacts(t *testing.T) {
	t.Parallel()
	srv := newCardServer(t, &a2a.AgentCard{Name: "coder", Description: "Writes Go."})
	st := openStore(t)
	ctx := context.Background()
	down := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(down.Close)
	for _, c := range []store.Contact{{Name: "ben", URL: down.URL, Via: ViaDirect}, {Name: "coder", URL: srv.URL, Via: ViaDirect}} {
		if err := st.PutContact(ctx, c); err != nil {
			t.Fatalf("PutContact() error = %v", err)
		}
	}
	m := Service{Store: st, HTTPClient: func(store.Contact) *http.Client { return srv.Client() }}

	got, err := m.Refresh(ctx)
	if err == nil || !strings.Contains(err.Error(), "refreshing the card of ben") {
		t.Fatalf("Service.Refresh() error = %v, want one naming ben", err)
	}
	if len(got) != 1 || got[0].Name != "coder" || got[0].Card.Description != "Writes Go." {
		t.Fatalf("Service.Refresh() = %+v, want coder with its card", got)
	}
}

func errorMatches(err, want error) bool {
	var nf *agenterr.NotFoundError
	switch {
	case want == nil:
		return err == nil
	case errors.As(want, &nf):
		return errors.As(err, &nf)
	}
	return errors.Is(err, want)
}
