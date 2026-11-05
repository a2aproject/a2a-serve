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

package agentops

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/access"
	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/agenterr"
	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/contacts"
	"github.com/a2aproject/a2a-serve/internal/controlapi"
	"github.com/a2aproject/a2a-serve/internal/network"
	"github.com/a2aproject/a2a-serve/internal/pairapi"
	"github.com/a2aproject/a2a-serve/internal/pairings"
	"github.com/a2aproject/a2a-serve/internal/schedule"
	"github.com/a2aproject/a2a-serve/internal/schedules"
	"github.com/a2aproject/a2a-serve/internal/store"
)

const baseURL = "https://coder.example.com"

func newService(t *testing.T) (*Service, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "server.db"))
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})
	return New(Deps{
		Store:     st,
		Pairings:  pairings.New(pairings.Deps{Store: st, BaseURL: func() string { return baseURL }}),
		Contacts:  contacts.Service{Store: st},
		Schedules: schedules.Service{Store: st},
		Runtime:   agentdir.NewRuntime(agentdir.Dir{}, &agentdir.Config{Name: "coder"}, nil),
		Network:   func() access.Network { return access.Network{Private: network.ProviderTailscale} },
	}), st
}

func TestPair(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := newService(t)

	got, err := s.Pair(ctx, controlapi.PairCodeRequest{Name: "grok", Webhook: "https://example.com/hook"})
	if err != nil {
		t.Fatalf("Pair() error = %v", err)
	}

	want := &controlapi.PairCodeResponse{
		Code: got.Code, Expires: got.Expires, URL: baseURL,
		PairCommand: "a2a-serve pair join " + baseURL + " " + got.Code,
		Node: &controlapi.NodeInfo{
			ID: "grok", Name: "grok", URL: baseURL + "/n/grok", Webhook: "https://example.com/hook", Created: got.Node.Created,
		},
		Instructions: "You are the webhook node \"grok\" of the agent at " + baseURL + ".\nRead " + baseURL + "/mailbox/skill.md and follow it.\nYour pairing code is " + got.Code + ".",
		Access: &controlapi.Access{
			PrivateAccess: "everyone", Owner: []string{auth.MemberSelf}, Authenticated: []string{auth.MemberPrivate}, Anonymous: []string{},
			Pairings: 1, Others: []string{"everyone on the tailnet"},
			Summary: []string{"you: this machine and your tailnet devices", "everyone on the tailnet, 1 pairing"},
		},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("Pair() wrong result (-want +got) diff = %s", diff)
	}
}

func TestRevoke(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := newService(t)
	for _, req := range []controlapi.PairCodeRequest{{Name: "anna"}, {Name: "grok", Webhook: "https://example.com/hook"}} {
		if _, err := s.Pair(ctx, req); err != nil {
			t.Fatalf("Pair(%q) error = %v", req.Name, err)
		}
	}
	tests := []struct {
		name             string
		wantKind         string
		wantStillAllowed []string
		wantKept         bool
		wantAccess       bool
	}{
		{name: "anna", wantStillAllowed: []string{"everyone on the tailnet"}, wantAccess: true},
		{name: "grok", wantKind: pairapi.PairingWebhook, wantAccess: true},
		{name: pairings.LocalOwner, wantKind: pairapi.PairingPerson, wantKept: true},
	}
	for _, tc := range tests {
		got, err := s.Revoke(ctx, tc.name)
		if err != nil {
			t.Fatalf("Revoke(%q) error = %v", tc.name, err)
		}
		want := struct {
			Kind         string
			StillAllowed []string
			Kept, Access bool
		}{tc.wantKind, tc.wantStillAllowed, tc.wantKept, tc.wantAccess}
		gotSummary := struct {
			Kind         string
			StillAllowed []string
			Kept, Access bool
		}{got.Kind, got.StillAllowed, got.Kept, got.Access != nil}
		if diff := cmp.Diff(want, gotSummary); diff != "" {
			t.Fatalf("Revoke(%q) wrong result (-want +got) diff = %s", tc.name, diff)
		}
	}
}

func TestSchedulesActAsOwner(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, st := newService(t)
	if _, err := s.Pair(ctx, controlapi.PairCodeRequest{Name: "anna"}); err != nil {
		t.Fatalf("Pair() error = %v", err)
	}
	anna := auth.Principal{ID: auth.PairingPrincipal("anna")}
	if _, err := (schedules.Service{Store: st}).Add(ctx, schedule.Request{Name: "hers", Cron: "@daily", Prompt: "hi"}, anna, "ctx-1"); err != nil {
		t.Fatalf("schedules.Service.Add() error = %v", err)
	}

	added, err := s.AddSchedule(ctx, schedule.Request{Name: "mine", Cron: "@daily", Prompt: "hi"})
	if err != nil {
		t.Fatalf("AddSchedule() error = %v", err)
	}
	if added.CreatedBy != auth.OwnerID {
		t.Fatalf("AddSchedule().CreatedBy = %q, want %q", added.CreatedBy, auth.OwnerID)
	}
	if err := s.DeleteSchedule(ctx, "hers"); err != nil {
		t.Fatalf("DeleteSchedule(hers) error = %v", err)
	}
	all, err := s.Schedules(ctx)
	if err != nil {
		t.Fatalf("Schedules() error = %v", err)
	}
	var names []string
	for _, sc := range all {
		names = append(names, sc.Name)
	}
	if diff := cmp.Diff([]string{"mine"}, names); diff != "" {
		t.Fatalf("Schedules() wrong result (-want +got) diff = %s", diff)
	}
}

func TestRunScheduleNeedsRunningServer(t *testing.T) {
	t.Parallel()
	s, _ := newService(t)

	_, err := s.RunSchedule(context.Background(), "brief")

	if !errors.Is(err, agenterr.ErrNotRunning) {
		t.Fatalf("RunSchedule() error = %v, want %v", err, agenterr.ErrNotRunning)
	}
}
