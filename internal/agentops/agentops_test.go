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
	"github.com/a2aproject/a2a-serve/internal/channel"
	"github.com/a2aproject/a2a-serve/internal/contacts"
	"github.com/a2aproject/a2a-serve/internal/controlapi"
	"github.com/a2aproject/a2a-serve/internal/network"
	"github.com/a2aproject/a2a-serve/internal/pairapi"
	"github.com/a2aproject/a2a-serve/internal/pairings"
	"github.com/a2aproject/a2a-serve/internal/schedule"
	"github.com/a2aproject/a2a-serve/internal/schedulesvc"
	"github.com/a2aproject/a2a-serve/internal/store"
)

const baseURL = "https://coder.example.com"

func newService(t *testing.T) (*Service, *store.Store) {
	t.Helper()
	return newServiceWith(t, agentdir.NewRuntime(agentdir.Dir{}, &agentdir.Config{Name: "coder"}, nil), Hooks{})
}

func newServiceWith(t *testing.T, rt *agentdir.Runtime, hooks Hooks) (*Service, *store.Store) {
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
		Schedules: schedulesvc.Service{Store: st},
		Runtime:   rt,
		Network:   func() access.Network { return access.Network{Private: network.ProviderTailscale} },
		Hooks:     hooks,
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
			PrivateAllowIncoming: "everyone", Owner: []string{auth.MemberSelf}, Authenticated: []string{auth.MemberPrivate}, Anonymous: []string{},
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

func pairAnnaWithTelegram(t *testing.T, s *Service, st *store.Store) {
	t.Helper()
	ctx := context.Background()
	if _, err := s.Pair(ctx, controlapi.PairCodeRequest{Name: "anna"}); err != nil {
		t.Fatalf("Pair(anna) error = %v", err)
	}
	if err := st.PutLink(ctx, store.Link{Channel: agentdir.ChannelTelegram, Identity: "telegram:1", Pairing: "anna", Conv: "dm1"}, pairapi.PairingPerson); err != nil {
		t.Fatalf("PutLink() error = %v", err)
	}
}

func TestPairings(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, st := newService(t)
	pairAnnaWithTelegram(t, s, st)

	all, err := s.Pairings(ctx)
	if err != nil {
		t.Fatalf("Pairings() error = %v", err)
	}
	anna, err := s.Pairing(ctx, "anna")
	if err != nil {
		t.Fatalf("Pairing(anna) error = %v", err)
	}

	want := controlapi.Pairing{
		Name: "anna", Kind: pairapi.PairingPerson, Role: auth.RoleAuthenticated, Principal: auth.PairingPrincipal("anna"),
		Created: anna.Created, Tokens: []controlapi.PairingToken{},
		Accounts:     []controlapi.LinkedAccount{{Channel: agentdir.ChannelTelegram, Account: "telegram:1"}},
		PendingCodes: anna.PendingCodes,
	}
	if diff := cmp.Diff(want, *anna); diff != "" {
		t.Fatalf("Pairing(anna) wrong result (-want +got) diff = %s", diff)
	}
	if len(anna.PendingCodes) != 1 {
		t.Fatalf("Pairing(anna).PendingCodes = %v, want one code", anna.PendingCodes)
	}
	var names []string
	for _, p := range all {
		names = append(names, p.Name)
	}
	if diff := cmp.Diff([]string{"anna", pairings.LocalOwner}, names); diff != "" {
		t.Fatalf("Pairings() wrong result (-want +got) diff = %s", diff)
	}
}

func TestUpdatePairing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, st := newService(t)
	pairAnnaWithTelegram(t, s, st)
	owner := true

	got, err := s.UpdatePairing(ctx, "anna", controlapi.PairingUpdate{Owner: &owner, Rotate: true})
	if err != nil {
		t.Fatalf("UpdatePairing() error = %v", err)
	}

	gotSummary := struct {
		Role, PairCommand string
		Access            bool
	}{got.Pairing.Role, got.Code.PairCommand, got.Access != nil}
	want := struct {
		Role, PairCommand string
		Access            bool
	}{auth.RoleOwner, "a2a-serve pair join " + baseURL + " " + got.Code.Code, true}
	if diff := cmp.Diff(want, gotSummary); diff != "" {
		t.Fatalf("UpdatePairing() wrong result (-want +got) diff = %s", diff)
	}
}

func TestPreviewRevoke(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, st := newService(t)
	pairAnnaWithTelegram(t, s, st)

	got, err := s.PreviewRevoke(ctx, "anna")
	if err != nil {
		t.Fatalf("PreviewRevoke() error = %v", err)
	}

	want := &controlapi.RemovalPreview{
		Kind: pairapi.PairingPerson, Codes: 1, AgentHomes: []string{},
		Accounts: []controlapi.LinkedAccount{{Channel: agentdir.ChannelTelegram, Account: "telegram:1"}},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("PreviewRevoke() wrong result (-want +got) diff = %s", diff)
	}
	if _, err := s.Pairing(ctx, "anna"); err != nil {
		t.Fatalf("Pairing(anna) after PreviewRevoke() error = %v, want the pairing kept", err)
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
	if _, err := (schedulesvc.Service{Store: st}).Add(ctx, schedule.Request{Name: "hers", Cron: "@daily", Prompt: "hi"}, anna, "ctx-1"); err != nil {
		t.Fatalf("schedulesvc.Service.Add() error = %v", err)
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

// fakeChannel is a channel whose credentials check fails with err.
type fakeChannel struct {
	channel.Channel
	kind string
	err  error
}

func (c *fakeChannel) Name() string                   { return c.kind }
func (c *fakeChannel) Validate(context.Context) error { return c.err }

func newAgentRuntime(t *testing.T, cfg *agentdir.Config) (*agentdir.Runtime, agentdir.Dir) {
	t.Helper()
	d, err := agentdir.Open(t.TempDir())
	if err != nil {
		t.Fatalf("agentdir.Open() error = %v", err)
	}
	if err := d.SaveConfig(cfg); err != nil {
		t.Fatalf("SaveConfig() error = %v", err)
	}
	return agentdir.NewRuntime(d, cfg.Clone(), nil), d
}

func TestAddChannel(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		stopped     bool
		checkErr    error
		wantSaved   bool
		wantErr     bool
		wantStatus  *controlapi.ChannelStatus
		wantStarted []string
	}{
		{name: "valid credentials", wantSaved: true, wantStatus: &controlapi.ChannelStatus{Name: agentdir.ChannelTelegram, Account: "@bot"}, wantStarted: []string{agentdir.ChannelTelegram}},
		{name: "stopped agent", stopped: true, wantSaved: true, wantStatus: &controlapi.ChannelStatus{Name: agentdir.ChannelTelegram}},
		{name: "invalid credentials", checkErr: errors.New("unauthorized"), wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rt, d := newAgentRuntime(t, &agentdir.Config{Name: "coder", Harness: "opencode"})
			var started []string
			hooks := Hooks{
				NewChannel: func(_ *agentdir.Config, secrets agentdir.Secrets, kind string) (channel.Channel, error) {
					if secrets["TELEGRAM_TOKEN"] != "tok" {
						return nil, errors.New("the new secret is missing")
					}
					return &fakeChannel{kind: kind, err: tc.checkErr}, nil
				},
			}
			if !tc.stopped {
				hooks.ChannelAdded = func(_ context.Context, ch channel.Channel) *controlapi.ChannelStatus {
					started = append(started, ch.Name())
					return &controlapi.ChannelStatus{Name: ch.Name(), Account: "@bot"}
				}
			}
			s, _ := newServiceWith(t, rt, hooks)
			req := controlapi.ChannelRequest{Kind: agentdir.ChannelTelegram, Token: "secret:TELEGRAM_TOKEN", Secrets: agentdir.Secrets{"TELEGRAM_TOKEN": "tok"}}

			got, err := s.AddChannel(context.Background(), req)

			if (err != nil) != tc.wantErr {
				t.Fatalf("AddChannel() error = %v, want error %v", err, tc.wantErr)
			}
			saved, readErr := d.ReadConfig()
			if readErr != nil {
				t.Fatalf("ReadConfig() error = %v", readErr)
			}
			secrets, loadErr := d.LoadSecrets()
			if loadErr != nil {
				t.Fatalf("LoadSecrets() error = %v", loadErr)
			}
			gotSaved := []bool{saved.HasChannel(agentdir.ChannelTelegram), rt.Config().HasChannel(agentdir.ChannelTelegram), secrets["TELEGRAM_TOKEN"] == "tok"}
			if diff := cmp.Diff([]bool{tc.wantSaved, tc.wantSaved, tc.wantSaved}, gotSaved); diff != "" {
				t.Fatalf("AddChannel() saved the channel in config.yaml, the running config and the secrets (-want +got) diff = %s", diff)
			}
			if tc.wantErr {
				return
			}
			if diff := cmp.Diff(tc.wantStatus, got); diff != "" {
				t.Fatalf("AddChannel() wrong result (-want +got) diff = %s", diff)
			}
			if diff := cmp.Diff(tc.wantStarted, started); diff != "" {
				t.Fatalf("AddChannel() started wrong channels (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestDeleteChannel(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		kind        string
		wantErr     func(error) bool
		wantStopped []string
	}{
		{name: "configured", kind: agentdir.ChannelTelegram, wantStopped: []string{agentdir.ChannelTelegram}},
		{name: "not configured", kind: agentdir.ChannelDiscord, wantErr: func(err error) bool {
			var nf *agenterr.NotFoundError
			return errors.As(err, &nf)
		}},
		{name: "unknown kind", kind: "irc", wantErr: func(err error) bool {
			var in *agenterr.InvalidError
			return errors.As(err, &in)
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := &agentdir.Config{Name: "coder", Harness: "opencode"}
			if err := cfg.SetChannel(agentdir.ChannelTelegram, "secret:TELEGRAM_TOKEN", nil); err != nil {
				t.Fatalf("SetChannel() error = %v", err)
			}
			rt, d := newAgentRuntime(t, cfg)
			var stopped []string
			s, _ := newServiceWith(t, rt, Hooks{ChannelRemoved: func(kind string) { stopped = append(stopped, kind) }})

			err := s.DeleteChannel(context.Background(), tc.kind)

			if tc.wantErr != nil {
				if !tc.wantErr(err) {
					t.Fatalf("DeleteChannel(%q) error = %v, want a refused request", tc.kind, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("DeleteChannel(%q) error = %v", tc.kind, err)
			}
			saved, err := d.ReadConfig()
			if err != nil {
				t.Fatalf("ReadConfig() error = %v", err)
			}
			if saved.HasChannel(tc.kind) || rt.Config().HasChannel(tc.kind) {
				t.Fatalf("DeleteChannel(%q) kept the channel in the config", tc.kind)
			}
			if diff := cmp.Diff(tc.wantStopped, stopped); diff != "" {
				t.Fatalf("DeleteChannel(%q) stopped wrong channels (-want +got) diff = %s", tc.kind, diff)
			}
		})
	}
}
