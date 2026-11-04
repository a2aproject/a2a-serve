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
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/controlapi"
	"github.com/a2aproject/a2a-serve/internal/harness"
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
	saved.Policy.PrivateAccess = agentdir.AccessPaired
	saved.Sandbox = harness.SandboxDocker
	if err := ts.dir.SaveConfig(saved); err != nil {
		t.Fatalf("SaveConfig() error = %v", err)
	}

	res, err := client.ReloadConfig(ctx)
	if err != nil {
		t.Fatalf("ReloadConfig() error = %v", err)
	}
	if diff := cmp.Diff([]string{"description", "allow-tools", "deny-tools", "owner-chat", "private-access", "public-access", "allow-email"}, res.Applied); diff != "" {
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

	if _, err := client.AddChannel(ctx, controlapi.ChannelRequest{
		Kind:    agentdir.ChannelTelegram,
		Token:   "secret:" + agentdir.SecretTelegram,
		Secrets: agentdir.Secrets{agentdir.SecretTelegram: "good"},
	}); err != nil {
		t.Fatalf("AddChannel() error = %v", err)
	}
	got, err := ts.dir.ReadConfig()
	if err != nil {
		t.Fatalf("ReadConfig() error = %v", err)
	}
	if got.Sandbox != harness.SandboxDocker || got.Channels.Telegram == nil {
		t.Fatalf("config.yaml after AddChannel = %+v, want the pending sandbox and the channel", got)
	}
}

func TestStatusAccess(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{
		private: &fakeProvider{net: &fakeNet{agents: map[string]string{}}, name: "coder", kind: ProviderTailscale},
		public:  publicNet(),
	})
	ts.pair("phone", auth.RoleOwner)
	ctx := context.Background()
	client := controlapi.NewClient(ts.dir)
	saved, err := ts.dir.ReadConfig()
	if err != nil {
		t.Fatalf("ReadConfig() error = %v", err)
	}
	saved.Policy = agentdir.PolicyConfig{PublicAccess: agentdir.AccessAnyone, AllowEmails: []string{"*@example.com"}}
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
		PrivateAccess: agentdir.AccessEveryone, PublicAccess: agentdir.AccessAnyone,
		Owner: []string{"self"}, Authenticated: []string{"private", "email:*@example.com"}, Anonymous: []string{"anyone"},
		Pairings: 1, OwnerPairings: 1,
		Others: []string{"everyone on the tailnet", "emails matching *@example.com", "anyone on the public URL or in a direct chat"},
		Summary: []string{
			"you: this machine and your tailnet devices",
			"everyone on the tailnet, emails matching *@example.com, anyone on the public URL or in a direct chat, 1 pairing (1 owner)",
			"the public URL is open to anyone",
		},
	}
	if diff := cmp.Diff(want, st.Access); diff != "" {
		t.Fatalf("Status() wrong access (-want +got) diff = %s", diff)
	}
}
