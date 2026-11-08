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
	"net/http"
	"sync"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/controlapi"
	"github.com/a2aproject/a2a-serve/internal/harness"
	"github.com/a2aproject/a2a-serve/internal/network"
)

func TestReloadConfigKeepsPendingChanges(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{name: "assistant", telegramAPI: fakeBotAPI(t)})
	ctx := context.Background()
	client := controlapi.NewClient(ts.dir)
	saved, err := ts.dir.ReadConfig()
	if err != nil {
		t.Fatalf("ReadConfig() error = %v", err)
	}
	saved.Description = "reviews code"
	saved.Approvals.OwnerChat = "telegram"
	saved.Policy.PrivateAllowIncoming = agentdir.AccessPaired
	saved.MCP.Disabled = []string{"github"}
	saved.Sandbox = harness.SandboxDocker
	if err := ts.dir.SaveConfig(saved); err != nil {
		t.Fatalf("SaveConfig() error = %v", err)
	}

	res, err := client.ReloadConfig(ctx)
	if err != nil {
		t.Fatalf("ReloadConfig() error = %v", err)
	}
	if diff := cmp.Diff(agentdir.LiveSettings(), res.Applied); diff != "" {
		t.Fatalf("ReloadConfig() wrong result (-want +got) diff = %s", diff)
	}
	running, err := client.Config(ctx)
	if err != nil {
		t.Fatalf("Config() error = %v", err)
	}
	want := saved.Clone()
	want.Sandbox = harness.SandboxLocal
	if diff := cmp.Diff(want, running); diff != "" {
		t.Fatalf("Config() wrong result (-want +got) diff = %s", diff)
	}

	if _, err := client.AddMessenger(ctx, controlapi.MessengerRequest{
		Kind:    agentdir.MessengerTelegram,
		Token:   "secret:" + agentdir.SecretTelegram,
		Secrets: agentdir.Secrets{agentdir.SecretTelegram: "good"},
	}); err != nil {
		t.Fatalf("AddMessenger() error = %v", err)
	}
	got, err := ts.dir.ReadConfig()
	if err != nil {
		t.Fatalf("ReadConfig() error = %v", err)
	}
	if got.Sandbox != harness.SandboxDocker || got.Messengers.Telegram == nil {
		t.Fatalf("config.yaml after AddMessenger = %+v, want the pending sandbox and the messenger", got)
	}
}

func TestStatusAccess(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{
		private: &fakeProvider{net: &fakeNet{agents: map[string]string{}}, name: "coder", kind: network.ProviderTailscale},
		public:  publicNet(),
	})
	ts.pair("phone", auth.RoleOwner)
	ctx := context.Background()
	client := controlapi.NewClient(ts.dir)
	saved, err := ts.dir.ReadConfig()
	if err != nil {
		t.Fatalf("ReadConfig() error = %v", err)
	}
	saved.Policy = agentdir.PolicyConfig{PublicAllowIncoming: agentdir.AccessAnyone, AllowEmails: []string{"*@example.com"}}
	if err := ts.dir.SaveConfig(saved); err != nil {
		t.Fatalf("SaveConfig() error = %v", err)
	}
	if _, err := client.ReloadConfig(ctx); err != nil {
		t.Fatalf("ReloadConfig() error = %v", err)
	}

	st, err := client.Status(ctx)
	if err != nil {
		t.Fatalf("Status() error = %v", err)
	}
	want := &controlapi.Access{
		PrivateAllowIncoming: agentdir.AccessEveryone, PublicAllowIncoming: agentdir.AccessAnyone,
		Owner: []string{"self"}, Authenticated: []string{"private", "email:*@example.com"}, Anonymous: []string{"anyone"},
		Pairings: 1, OwnerPairings: 1,
		Others: []string{"everyone on the tailnet", "emails matching *@example.com", "anyone on the public URL"},
		Summary: []string{
			"you: this machine and your tailnet devices",
			"everyone on the tailnet, emails matching *@example.com, anyone on the public URL, 1 pairing (1 owner)",
			"the public URL is open to anyone",
		},
		Sandbox: []string{"you and owner pairings: no sandbox, no secrets", "everyone else: no sandbox, no secrets"},
		SandboxNotes: []string{
			"without the Docker sandbox, every caller's agent can read and change your files; the storage of sandbox rules needs --sandbox docker",
			"callers who aren't owners now get no secrets; to give them every secret as before, run `a2a-serve config set --caller-secrets all`; to keep the new default and hide this note, run `a2a-serve config set --caller-storage read-only`",
		},
	}
	if diff := cmp.Diff(want, st.Access); diff != "" {
		t.Fatalf("Status() wrong access (-want +got) diff = %s", diff)
	}
}

func TestDenyToolsEndGrants(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		approvals agentdir.ApprovalsConfig
		want      string
	}{
		{name: "more denied tools", approvals: agentdir.ApprovalsConfig{Deny: []string{"execute"}}, want: "whoami denied."},
		{name: "more allowed tools", approvals: agentdir.ApprovalsConfig{Allow: []string{"read"}}, want: "Ran whoami (allow)."},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ts := startServer(t, serverOpts{})
			c := ts.client(ts.pair("me", auth.RoleOwner))
			asked := send(t, c, userText("whoami", "", ""))
			wantResult(t, send(t, c, userText("always", string(asked.ID), asked.ContextID)), a2a.TaskStateCompleted, "Ran whoami (allow).")
			wantResult(t, send(t, c, userText("whoami", "", asked.ContextID)), a2a.TaskStateCompleted, "Ran whoami (allow).")
			saved, err := ts.dir.ReadConfig()
			if err != nil {
				t.Fatalf("ReadConfig() error = %v", err)
			}
			saved.Approvals = tc.approvals
			if err := ts.dir.SaveConfig(saved); err != nil {
				t.Fatalf("SaveConfig() error = %v", err)
			}

			if _, err := controlapi.NewClient(ts.dir).ReloadConfig(context.Background()); err != nil {
				t.Fatalf("ReloadConfig() error = %v", err)
			}

			wantResult(t, send(t, c, userText("whoami", "", asked.ContextID)), a2a.TaskStateCompleted, tc.want)
		})
	}
}

func TestDenyToolsAnswersPendingApprovalsAndEndsPreferences(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{})
	c := ts.client(ts.pair("me", auth.RoleOwner))
	waiting := send(t, c, userText("ask", "", ""))
	if err := ts.srv.prefs.Allow(context.Background(), "pair:me", "command:whoami *"); err != nil {
		t.Fatalf("Preferences.Allow() error = %v", err)
	}
	wantResult(t, send(t, c, userText("whoami", "", "")), a2a.TaskStateCompleted, "Ran whoami (allow).")
	saved, err := ts.dir.ReadConfig()
	if err != nil {
		t.Fatalf("ReadConfig() error = %v", err)
	}
	saved.Approvals.Deny = []string{"execute"}
	if err := ts.dir.SaveConfig(saved); err != nil {
		t.Fatalf("SaveConfig() error = %v", err)
	}

	if _, err := controlapi.NewClient(ts.dir).ReloadConfig(context.Background()); err != nil {
		t.Fatalf("ReloadConfig() error = %v", err)
	}

	wantResult(t, waitState(t, c, waiting.ID, a2a.TaskStateCompleted), a2a.TaskStateCompleted, "Push denied.")
	wantResult(t, send(t, c, userText("whoami", "", "")), a2a.TaskStateCompleted, "whoami denied.")
}

func TestPreferenceAnswersPendingApprovals(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{})
	bob := ts.client(ts.pair("bob", auth.RoleAuthenticated))
	waiting := send(t, bob, userText("ask", "", ""))

	if err := ts.srv.prefs.Allow(context.Background(), "pair:bob", "command:git push *"); err != nil {
		t.Fatalf("Preferences.Allow() error = %v", err)
	}

	wantResult(t, waitState(t, bob, waiting.ID, a2a.TaskStateCompleted), a2a.TaskStateCompleted, "Pushed (allow).")
}

func TestAllowToolsAnswersPendingApprovals(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{})
	c := ts.client(ts.pair("me", auth.RoleOwner))
	task := send(t, c, userText("ask", "", ""))
	saved, err := ts.dir.ReadConfig()
	if err != nil {
		t.Fatalf("ReadConfig() error = %v", err)
	}
	saved.Approvals.Allow = []string{"execute"}
	if err := ts.dir.SaveConfig(saved); err != nil {
		t.Fatalf("SaveConfig() error = %v", err)
	}

	if _, err := controlapi.NewClient(ts.dir).ReloadConfig(context.Background()); err != nil {
		t.Fatalf("ReloadConfig() error = %v", err)
	}

	wantResult(t, waitState(t, c, task.ID, a2a.TaskStateCompleted), a2a.TaskStateCompleted, "Pushed (allow).")
}

func TestControlCardServesReloadedSkills(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{})
	ctx := context.Background()
	client := controlapi.NewClient(ts.dir)
	saved, err := ts.dir.ReadConfig()
	if err != nil {
		t.Fatalf("ReadConfig() error = %v", err)
	}
	saved.Card.Skills = []agentdir.CardSkill{{ID: "release-notes", Name: "Release notes", Description: "Writes release notes.", Tags: []string{"git"}}}
	if err := ts.dir.SaveConfig(saved); err != nil {
		t.Fatalf("SaveConfig() error = %v", err)
	}
	if _, err := client.ReloadConfig(ctx); err != nil {
		t.Fatalf("ReloadConfig() error = %v", err)
	}

	got, err := client.Card(ctx, network.ListenerLocal)
	if err != nil {
		t.Fatalf("Card() error = %v", err)
	}
	local, _, _ := ts.srv.URLs()
	served, err := network.FetchCard(ctx, http.DefaultClient, local)
	if err != nil {
		t.Fatalf("FetchCard() error = %v", err)
	}
	if diff := cmp.Diff(served, got); diff != "" {
		t.Fatalf("Card() differs from the served card (-served +got) diff = %s", diff)
	}
	want := []a2a.AgentSkill{{ID: "release-notes", Name: "Release notes", Description: "Writes release notes.", Tags: []string{"git"}}}
	if diff := cmp.Diff(want, got.Skills); diff != "" {
		t.Fatalf("Card() wrong skills (-want +got) diff = %s", diff)
	}
	if _, err := client.Card(ctx, network.ListenerPrivate); err == nil {
		t.Fatalf("Card(%q) error = nil, want an error for an agent without a private URL", network.ListenerPrivate)
	}
}

func TestConcurrentRuleAnswersOfWaitingApprovals(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{})
	c := ts.client(ts.pair("me", auth.RoleOwner))
	waiting := make([]*a2a.Task, 3)
	for i := range waiting {
		waiting[i] = send(t, c, userText("ask", "", ""))
	}
	saved, err := ts.dir.ReadConfig()
	if err != nil {
		t.Fatalf("ReadConfig() error = %v", err)
	}
	saved.Approvals.Allow = []string{"execute"}
	if err := ts.dir.SaveConfig(saved); err != nil {
		t.Fatalf("SaveConfig() error = %v", err)
	}
	if err := ts.srv.rt.Reload(); err != nil {
		t.Fatalf("Reload() error = %v", err)
	}

	start := make(chan struct{})
	errs := make(chan error, 4)
	var wg sync.WaitGroup
	for range cap(errs) {
		wg.Go(func() {
			<-start
			errs <- ts.srv.answerWaiting(context.Background(), nil)
		})
	}
	close(start)
	wg.Wait()
	close(errs)

	for err := range errs {
		if err != nil {
			t.Fatalf("answerWaiting() error = %v", err)
		}
	}
	for _, w := range waiting {
		wantResult(t, waitState(t, c, w.ID, a2a.TaskStateCompleted), a2a.TaskStateCompleted, "Pushed (allow).")
	}
}
