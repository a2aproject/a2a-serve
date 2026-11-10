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
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/a2atext"
	"github.com/a2aproject/a2a-serve/internal/access"
	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/agentkey"
	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/controlapi"
	"github.com/a2aproject/a2a-serve/internal/messenger"
	"github.com/a2aproject/a2a-serve/internal/messenger/messengertest"
	"github.com/a2aproject/a2a-serve/internal/network"
	"github.com/a2aproject/a2a-serve/internal/store"
	"github.com/a2aproject/a2a-serve/internal/workspaces"
)

func TestSandboxMounts(t *testing.T) {
	t.Parallel()
	mounts := []agentdir.Mount{{Src: "/src/app", Dst: "/src/app"}, {Src: "/docs", Dst: "/docs"}, {Src: "/data", Dst: "/data", ReadOnly: true}}
	tests := []struct {
		storage string
		want    []agentdir.Mount
	}{
		{storage: agentdir.StorageShared, want: mounts},
		{storage: agentdir.StorageReadOnly, want: []agentdir.Mount{{Src: "/src/app", Dst: "/src/app", ReadOnly: true}, {Src: "/docs", Dst: "/docs", ReadOnly: true}, {Src: "/data", Dst: "/data", ReadOnly: true}}},
		{storage: agentdir.StorageOwn, want: []agentdir.Mount{{Src: "/src/app", Dst: "/src/app"}, {Src: "/docs", Dst: "/docs", ReadOnly: true}, {Src: "/data", Dst: "/data", ReadOnly: true}}},
		{storage: agentdir.StorageEmpty, want: []agentdir.Mount{{Src: "/src/app", Dst: "/src/app"}}},
	}
	for _, tt := range tests {
		t.Run(tt.storage, func(t *testing.T) {
			t.Parallel()
			if diff := cmp.Diff(tt.want, sandboxMounts(tt.storage, mounts)); diff != "" {
				t.Fatalf("sandboxMounts() wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}

func sandboxServer(t *testing.T, rules ...agentdir.SandboxRule) *testServer {
	t.Helper()
	return startServer(t, serverOpts{
		secrets: agentdir.Secrets{"KEY": "owner-key", "GUEST": "guest-key"},
		configure: func(c *agentdir.Config) {
			c.Secrets = []string{"KEY", "GUEST"}
			c.Policy.Sandbox = rules
		},
	})
}

func TestContainersGetTheSecretsOfTheirRule(t *testing.T) {
	t.Parallel()
	ts := sandboxServer(t, agentdir.SandboxRule{Match: []string{"pairing:ben"}, Storage: agentdir.StorageOwn, Secrets: []string{"KEY=secret:GUEST"}})
	owner := ts.client(ts.pair("phone", auth.RoleOwner))
	ben := ts.client(ts.pair("ben", auth.RoleMember))
	cy := ts.client(ts.pair("cy", auth.RoleMember))
	tests := []struct {
		name, prompt, want string
		client             *a2aclient.Client
	}{
		{name: "owner", client: owner, prompt: "env KEY", want: "env KEY=owner-key"},
		{name: "owner", client: owner, prompt: "env GUEST", want: "env GUEST=guest-key"},
		{name: "rule", client: ben, prompt: "env KEY", want: "env KEY=guest-key"},
		{name: "rule", client: ben, prompt: "env GUEST", want: "env GUEST unset"},
		{name: "no rule", client: cy, prompt: "env KEY", want: "env KEY=owner-key"},
	}
	for _, tt := range tests {
		if got := a2atext.Result(send(t, tt.client, userText(tt.prompt, "", ""))); got != tt.want {
			t.Fatalf("%s: SendMessage(%q) = %q, want %q", tt.name, tt.prompt, got, tt.want)
		}
	}
}

func TestRuleChangeAppliesToTheNextTurn(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		checkInterval time.Duration
	}{
		{name: "turn stops the container", checkInterval: time.Hour},
		{name: "sandbox loop stops the container", checkInterval: time.Millisecond},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ts := startServer(t, serverOpts{
				secrets:   agentdir.Secrets{"KEY": "owner-key", "GUEST": "guest-key"},
				configure: func(c *agentdir.Config) { c.Secrets = []string{"KEY", "GUEST"} },
				timings:   func(tm *Timings) { tm.SandboxCheckInterval = tc.checkInterval },
			})
			ben := ts.client(ts.pair("ben", auth.RoleMember))
			first := send(t, ben, userText("env KEY", "", ""))
			if got := a2atext.Result(first); got != "env KEY=owner-key" {
				t.Fatalf("SendMessage() = %q, want %q", got, "env KEY=owner-key")
			}

			setRule(t, ts, agentdir.SandboxRule{Match: []string{"pairing:ben"}, Secrets: []string{"none"}})

			if got := a2atext.Result(send(t, ben, userText("env KEY", "", first.ContextID))); got != "env KEY unset" {
				t.Fatalf("SendMessage() after the rule changed = %q, want %q", got, "env KEY unset")
			}
		})
	}
}

func TestMessageIntoARicherContainerIsRefused(t *testing.T) {
	t.Parallel()
	ts := sandboxServer(t,
		agentdir.SandboxRule{Match: []string{"self"}, Secrets: []string{"none"}},
	)
	owner := ts.client(ts.pair("phone", auth.RoleOwner))
	ben := ts.client(ts.pair("ben", auth.RoleMember))
	task := send(t, ben, userText("hello", "", ""))

	got := send(t, owner, userText("env KEY", "", task.ContextID))

	if got.Status.State != a2a.TaskStateFailed || !strings.Contains(a2atext.Message(got.Status.Message), "start a conversation of your own") {
		t.Fatalf("SendMessage() into a richer conversation = %v %q, want it failed with a hint to start a conversation", got.Status.State, a2atext.Message(got.Status.Message))
	}
}

func setOwner(t *testing.T, ts *testServer, name string, owner bool) {
	t.Helper()
	if _, err := controlapi.NewClient(ts.dir).UpdatePairing(context.Background(), name, controlapi.PairingUpdate{Owner: &owner}); err != nil {
		t.Fatalf("UpdatePairing(%s) error = %v", name, err)
	}
}

func TestConversationOfAnotherRoleAfterARoleChange(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		startedAs string
		first     string
		answer    bool
		wantState a2a.TaskState
	}{
		{name: "demoted owner sends a message", startedAs: auth.RoleOwner, first: "hello", wantState: a2a.TaskStateFailed},
		{name: "demoted owner answers an approval", startedAs: auth.RoleOwner, first: "ask", answer: true, wantState: a2a.TaskStateFailed},
		{name: "promoted member sends a message", startedAs: auth.RoleMember, first: "hello", wantState: a2a.TaskStateCompleted},
		{name: "promoted member answers an approval", startedAs: auth.RoleMember, first: "ask", answer: true, wantState: a2a.TaskStateCompleted},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ts := sandboxServer(t)
			anna := ts.client(ts.pair("anna", tt.startedAs))
			first := send(t, anna, userText(tt.first, "", ""))

			setOwner(t, ts, "anna", tt.startedAs != auth.RoleOwner)
			next := userText("hello", "", first.ContextID)
			if tt.answer {
				next = userText("once", string(first.ID), first.ContextID)
			}
			got := send(t, anna, next)

			if got.Status.State != tt.wantState {
				t.Fatalf("SendMessage() after the role change state = %v %q, want %v", got.Status.State, a2atext.Message(got.Status.Message), tt.wantState)
			}
			if tt.wantState == a2a.TaskStateFailed && !strings.Contains(a2atext.Message(got.Status.Message), "start a new conversation") {
				t.Fatalf("SendMessage() after the role change = %q, want a hint to start a new conversation", a2atext.Message(got.Status.Message))
			}
		})
	}
}

func TestRefusedAnswerEndsTheWaitingTurnOfTheAgent(t *testing.T) {
	t.Parallel()
	ts := sandboxServer(t)
	owner := ts.client(ts.pair("phone", auth.RoleOwner))
	anna := ts.client(ts.pair("anna", auth.RoleOwner))
	waiting := send(t, anna, userText("ask", "", ""))
	setOwner(t, ts, "anna", false)
	if got := send(t, anna, userText("once", string(waiting.ID), waiting.ContextID)); got.Status.State != a2a.TaskStateFailed {
		t.Fatalf("SendMessage() answering after the role change state = %v, want %v", got.Status.State, a2a.TaskStateFailed)
	}

	next := sendAsync(t, owner, userText("hello", "", waiting.ContextID))

	if got := waitState(t, owner, next.ID, a2a.TaskStateCompleted); got.Status.State != a2a.TaskStateCompleted {
		t.Fatalf("owner's task after the refused answer state = %v, want %v", got.Status.State, a2a.TaskStateCompleted)
	}
}

func TestAdmitTurnOfAnotherUsersContainer(t *testing.T) {
	t.Parallel()
	s := &Server{sandboxes: newSandboxes(func() *agentdir.Config { return &agentdir.Config{} }, openStore(t))}
	member := func(name string) auth.Principal {
		return auth.Principal{ID: auth.PairPrincipal(name), Role: auth.RoleMember, Identity: auth.Identity{Pairing: name}}
	}
	behindAgent := member("assistant")
	behindAgent.Caller, behindAgent.Hops = "m13", 1
	guest := auth.Principal{ID: auth.GuestID, Role: auth.RoleGuest}
	demotedToGuest := auth.Principal{ID: "email:anna@example.com", Role: auth.RoleGuest, Identity: auth.Identity{Email: "anna@example.com"}}
	tests := []struct {
		name    string
		caller  auth.Principal
		via     func(context.Context) context.Context
		key     string
		wantErr error
	}{
		{name: "member into their own container", caller: member("anna"), key: agentkey.User(auth.PairPrincipal("anna"))},
		{name: "caller behind an agent into the agent's container", caller: behindAgent, key: agentkey.User(auth.PairPrincipal("assistant"))},
		{name: "agent into the container of a caller behind it", caller: member("assistant"), key: agentkey.Caller(auth.PairPrincipal("assistant"), "m13")},
		{name: "owner into a member's container", caller: auth.Owner(), key: agentkey.User(auth.PairPrincipal("anna"))},
		{name: "member into the owner's container", caller: member("anna"), key: "", wantErr: errOtherContainer},
		{name: "member into another member's container", caller: member("anna"), key: agentkey.User(auth.PairPrincipal("ben")), wantErr: errOtherContainer},
		{name: "guest into a member's container", caller: guest, key: agentkey.User(auth.PairPrincipal("anna")), wantErr: errOtherContainer},
		{name: "member turned guest into their container as a member", caller: demotedToGuest, key: agentkey.User(demotedToGuest.ID), wantErr: errOtherContainer},
		{name: "posted turn of a member into the owner's container", caller: member("anna"), via: auth.Posted, key: "", wantErr: errOtherContainer},
		{name: "member in a group chat into the owner's container", caller: member("anna"), via: auth.InGroupChat, key: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			if tt.via != nil {
				ctx = tt.via(ctx)
			}
			release, err := s.admitTurn(auth.InProcess(ctx, tt.caller), tt.key, nil)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("admitTurn() error = %v, want %v", err, tt.wantErr)
			}
			if err == nil {
				release()
			}
		})
	}
}

func TestDemotedOwnerInAChatStartsANewConversation(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{messenger: messengertest.NewMessenger("fake")})
	linkAs(t, ts, "anna", auth.RoleOwner, "telegram:2", "dm2")
	ts.ch.Write("telegram:2", "dm2", "remember red")
	ts.ch.WaitFor(t, "dm2", messengertest.HasText("OK."))

	setOwner(t, ts, "anna", false)
	ts.ch.Write("telegram:2", "dm2", "recall")
	ts.ch.WaitFor(t, "dm2", messengertest.HasText("start a new conversation"))
	ts.ch.Write("telegram:2", "dm2", "/new")
	ts.ch.WaitFor(t, "dm2", messengertest.HasText("Started a new conversation."))
	ts.ch.Write("telegram:2", "dm2", "recall")

	ts.ch.WaitFor(t, "dm2", func(m messenger.Outbound) bool { return m.Text == "You said: " })
}

func TestMemberContinuesAGroupConversationTheOwnerStarted(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{messenger: messengertest.NewMessenger("fake")})
	linkOwner(t, ts, "telegram:1", "dm1")
	linkAs(t, ts, "anna", auth.RoleMember, "telegram:2", "dm2")
	inGroup := func(identity, text string) {
		in := ts.ch.Message(identity, "grp", text)
		in.Direct = false
		ts.ch.Deliver(in)
	}
	inGroup("telegram:1", "remember red")
	ts.ch.WaitFor(t, "grp", messengertest.HasText("OK."))

	inGroup("telegram:2", "ask")
	ref, q := ts.ch.WaitFor(t, "grp", messengertest.HasQuestion)
	click := ts.ch.ClickMessage("telegram:2", "grp", ref, q.Question, 0)
	click.Direct = false
	ts.ch.Deliver(click)

	ts.ch.WaitFor(t, "grp", messengertest.HasText("Pushed (allow)."))
}

func TestScheduleOfADemotedOwnerFailsInItsConversation(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{messenger: messengertest.NewMessenger("fake")})
	linkAs(t, ts, "anna", auth.RoleOwner, "telegram:2", "dm2")
	ts.ch.Write("telegram:2", "dm2", `call schedules add brief --cron @daily --prompt "brief me"`)
	ts.ch.WaitFor(t, "dm2", messengertest.HasText("Scheduled brief"))

	setOwner(t, ts, "anna", false)
	if _, err := controlapi.NewClient(ts.dir).RunSchedule(context.Background(), "brief"); err != nil {
		t.Fatalf("RunSchedule() error = %v", err)
	}

	ts.ch.WaitFor(t, "dm2", messengertest.HasText("start a new conversation"))
}

func TestMCPServersNeedTheSecretsOfTheContainer(t *testing.T) {
	t.Parallel()
	ts := sandboxServer(t, agentdir.SandboxRule{Match: []string{"pairing:ben"}, Secrets: []string{"none"}})
	mcp := `{"mcpServers": {"gh": {"type": "http", "url": "https://example.com/mcp", "headers": {"Authorization": "Bearer ${KEY}"}}}}`
	if err := os.WriteFile(filepath.Join(ts.dir.Root, ".mcp.json"), []byte(mcp), 0o644); err != nil {
		t.Fatal(err)
	}
	owner := ts.client(ts.pair("phone", auth.RoleOwner))
	ben := ts.client(ts.pair("ben", auth.RoleMember))

	if got := a2atext.Result(send(t, owner, userText("mcp", "", ""))); !strings.Contains(got, `"gh"`) {
		t.Fatalf("MCP servers of the owner = %s, want gh", got)
	}
	if got := a2atext.Result(send(t, ben, userText("mcp", "", ""))); strings.Contains(got, `"gh"`) {
		t.Fatalf("MCP servers of a user without KEY = %s, want no gh", got)
	}
}

func setRule(t *testing.T, ts *testServer, r agentdir.SandboxRule) {
	t.Helper()
	if err := ts.srv.rt.Update(func(c *agentdir.Config) error {
		c.Policy.SetSandboxRule(r)
		return nil
	}); err != nil {
		t.Fatalf("Update() error = %v", err)
	}
}

func TestBusyContainerWithWiderRulesTakesOnlyAnswers(t *testing.T) {
	t.Parallel()
	ts := sandboxServer(t, agentdir.SandboxRule{Match: []string{"pairing:ben"}, Secrets: []string{"none"}})
	ben := ts.client(ts.pair("ben", auth.RoleMember))
	waiting := send(t, ben, userText("ask", "", ""))
	if waiting.Status.State != a2a.TaskStateInputRequired {
		t.Fatalf("SendMessage(ask) state = %v, want %v", waiting.Status.State, a2a.TaskStateInputRequired)
	}
	setRule(t, ts, agentdir.SandboxRule{Match: []string{"pairing:ben"}, Secrets: []string{"KEY"}})

	refused := send(t, ben, userText("env KEY", "", ""))
	if refused.Status.State != a2a.TaskStateFailed || !strings.Contains(a2atext.Message(refused.Status.Message), "restarts with them") {
		t.Fatalf("SendMessage() to a busy container with wider rules = %v %q, want it refused until the restart", refused.Status.State, a2atext.Message(refused.Status.Message))
	}
	if got := send(t, ben, userText("once", string(waiting.ID), waiting.ContextID)); got.Status.State != a2a.TaskStateCompleted {
		t.Fatalf("SendMessage() answering the waiting task state = %v, want %v", got.Status.State, a2a.TaskStateCompleted)
	}
	if got := a2atext.Result(send(t, ben, userText("env KEY", "", ""))); got != "env KEY=owner-key" {
		t.Fatalf("SendMessage() after the answer = %q, want the new rule", got)
	}
}

func TestNarrowedRulesEndTheWaitingTasksOfTheContainer(t *testing.T) {
	t.Parallel()
	ts := sandboxServer(t)
	ben := ts.client(ts.pair("ben", auth.RoleMember))
	waiting := send(t, ben, userText("ask", "", ""))
	if waiting.Status.State != a2a.TaskStateInputRequired {
		t.Fatalf("SendMessage(ask) state = %v, want %v", waiting.Status.State, a2a.TaskStateInputRequired)
	}
	setRule(t, ts, agentdir.SandboxRule{Match: []string{"pairing:ben"}, Secrets: []string{"none"}})

	if got := a2atext.Result(send(t, ben, userText("env KEY", "", ""))); got != "env KEY unset" {
		t.Fatalf("SendMessage() after the rules narrowed = %q, want %q", got, "env KEY unset")
	}
	if got := waitState(t, ben, waiting.ID, a2a.TaskStateCanceled); got.Status.State != a2a.TaskStateCanceled {
		t.Fatalf("waiting task state = %v, want %v", got.Status.State, a2a.TaskStateCanceled)
	}
}

func TestAnswerIntoANarrowedContainerIsRefused(t *testing.T) {
	t.Parallel()
	// The sandbox loop would cancel the waiting task before the answer.
	ts := startServer(t, serverOpts{
		secrets:   agentdir.Secrets{"KEY": "owner-key", "GUEST": "guest-key"},
		configure: func(c *agentdir.Config) { c.Secrets = []string{"KEY", "GUEST"} },
		timings:   func(tm *Timings) { tm.SandboxCheckInterval = time.Hour },
	})
	ben := ts.client(ts.pair("ben", auth.RoleMember))
	waiting := send(t, ben, userText("ask", "", ""))
	setRule(t, ts, agentdir.SandboxRule{Match: []string{"pairing:ben"}, Secrets: []string{"none"}})

	got := send(t, ben, userText("once", string(waiting.ID), waiting.ContextID))

	if got.Status.State != a2a.TaskStateFailed || !strings.Contains(a2atext.Message(got.Status.Message), "give less") {
		t.Fatalf("SendMessage() answering into a narrowed container = %v %q, want it failed", got.Status.State, a2atext.Message(got.Status.Message))
	}
	if got := a2atext.Result(send(t, ben, userText("env KEY", "", ""))); got != "env KEY unset" {
		t.Fatalf("SendMessage() after the refusal = %q, want %q", got, "env KEY unset")
	}
}

func TestAdmitOther(t *testing.T) {
	t.Parallel()
	rules := []agentdir.SandboxRule{
		{Match: []string{"pairing:ann"}, Storage: agentdir.StorageOwn, Secrets: []string{"none"}},
		{Match: []string{"pairing:bob"}, Storage: agentdir.StorageOwn, Secrets: []string{"KEY"}},
		{Match: []string{"pairing:cy"}, Storage: agentdir.StorageShared},
	}
	s := &Server{sandboxes: newSandboxes(func() *agentdir.Config {
		return &agentdir.Config{Secrets: []string{"KEY"}, Policy: agentdir.PolicyConfig{Sandbox: rules}}
	}, openStore(t))}
	user := func(name string) auth.Principal {
		return auth.Principal{ID: auth.PairPrincipal(name), Role: auth.RoleMember, Identity: auth.Identity{Pairing: name}}
	}
	tests := []struct {
		name    string
		caller  auth.Principal
		into    string
		wantErr error
	}{
		{name: "owner into a private workspace", caller: auth.Owner(), into: "ann"},
		{name: "user into another's private workspace", caller: user("bob"), into: "ann", wantErr: errSandboxPrivate},
		{name: "user with fewer secrets", caller: user("ann"), into: "bob", wantErr: errSandboxCovers},
		{name: "user with less storage", caller: user("ann"), into: "cy", wantErr: errSandboxCovers},
		{name: "user into a shared container", caller: user("cy"), into: "cy"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			key := agentkey.User(auth.PairPrincipal(tt.into))
			want, err := s.sandboxes.of(context.Background(), key)
			if err != nil {
				t.Fatalf("of() error = %v", err)
			}
			if err := s.admitOther(context.Background(), tt.caller, key, want); !errors.Is(err, tt.wantErr) {
				t.Fatalf("admitOther() error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestPruneWorkspaces(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	src := t.TempDir()
	s := &Server{
		workspaces: workspaces.New(filepath.Join(t.TempDir(), "w")),
		sandboxes: newSandboxes(func() *agentdir.Config {
			return &agentdir.Config{Policy: agentdir.PolicyConfig{Sandbox: []agentdir.SandboxRule{{Match: []string{"guests"}, Storage: agentdir.StorageEmpty}}}}
		}, openStore(t)),
	}
	old := time.Now().Add(-2 * unusedWorkspaceTTL)
	for _, w := range []struct {
		key, storage string
		used         time.Time
	}{
		{key: agentkey.User("email:old@example.com"), storage: agentdir.StorageOwn, used: old},
		{key: agentkey.User("email:new@example.com"), storage: agentdir.StorageOwn, used: time.Now()},
		{key: agentkey.User(auth.PairPrincipal("ben")), storage: agentdir.StorageOwn, used: old},
		{key: agentkey.Guest, storage: agentdir.StorageOwn, used: time.Now()},
		{key: agentkey.Caller("email:old-agent@example.com", "m13"), storage: agentdir.StorageOwn, used: old},
		{key: agentkey.Caller("email:new-agent@example.com", "m13"), storage: agentdir.StorageOwn, used: time.Now()},
		{key: agentkey.Caller(auth.PairPrincipal("assistant"), "m13"), storage: agentdir.StorageOwn, used: old},
	} {
		id, _ := agentkey.UserOf(w.key)
		name := workspaceName(w.key)
		if _, err := s.workspaces.Prepare(ctx, name, id, w.storage, src); err != nil {
			t.Fatalf("Prepare() error = %v", err)
		}
		if err := os.Chtimes(filepath.Join(s.workspaces.Root, name, "used"), w.used, w.used); err != nil {
			t.Fatal(err)
		}
	}

	s.pruneWorkspaces(ctx)

	entries, err := s.workspaces.List()
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	var got []string
	for _, e := range entries {
		if len(e.Storage) > 0 {
			got = append(got, e.Principal)
		}
	}
	sort.Strings(got)
	if diff := cmp.Diff([]string{"email:new-agent@example.com", "email:new@example.com", "pair:assistant", "pair:ben"}, got); diff != "" {
		t.Fatalf("pruneWorkspaces() kept wrong workspaces (-want +got) diff = %s", diff)
	}
}

func TestSandboxOfUserAfterRestartMatchesTheVerifiedIdentity(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openStore(t)
	config := func() *agentdir.Config {
		return &agentdir.Config{Secrets: []string{"KEY"}, Policy: agentdir.PolicyConfig{Sandbox: []agentdir.SandboxRule{
			{Match: []string{"email:*@example.com"}, Storage: agentdir.StorageReadOnly, Secrets: []string{"none"}},
		}}}
	}
	ann := auth.Principal{ID: "email:ann@example.com", Role: auth.RoleMember, Identity: auth.Identity{
		Network: network.ListenerPublic, Email: "ann@example.com", EmailSource: auth.EmailFromGoogle, Domain: "example.com",
	}}
	key := agentkey.User(ann.ID)
	newSandboxes(config, st).remember(ctx, ann)

	got, err := newSandboxes(config, st).of(ctx, key)
	if err != nil {
		t.Fatalf("of() error = %v", err)
	}

	if diff := cmp.Diff(access.Sandbox{Storage: agentdir.StorageReadOnly}, got); diff != "" {
		t.Fatalf("of() after a restart wrong result (-want +got) diff = %s", diff)
	}
}

func openStore(t *testing.T) *store.Store {
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
	return st
}

func TestSandboxOfUnverifiedUserIsTheNarrowest(t *testing.T) {
	t.Parallel()
	config := func() *agentdir.Config {
		return &agentdir.Config{Secrets: []string{"KEY"}, Policy: agentdir.PolicyConfig{Sandbox: []agentdir.SandboxRule{
			{Match: []string{"email:*@example.com"}, Secrets: []string{"none"}},
		}}}
	}

	got, err := newSandboxes(config, openStore(t)).of(context.Background(), agentkey.User("email:ann@example.com"))
	if err != nil {
		t.Fatalf("of() error = %v", err)
	}

	if diff := cmp.Diff(access.Sandbox{Storage: agentdir.StorageShared}, got); diff != "" {
		t.Fatalf("of() of a user without a verified identity wrong result (-want +got) diff = %s", diff)
	}
}

func TestGuestRuleGivesAGuestKeyAndKeepsPairingsAsTheyAre(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{
		public:    publicNet(),
		messenger: messengertest.NewMessenger("fake"),
		secrets:   agentdir.Secrets{"KEY": "owner-key", "GUEST": "guest-key"},
		configure: func(c *agentdir.Config) {
			c.Secrets = []string{"KEY", "GUEST"}
			c.Policy.PublicAllowIncoming = agentdir.AccessAnyone
		},
	})
	anon := anonymousClient(t, ts)
	ben := ts.client(ts.pair("ben", auth.RoleMember))
	if got := a2atext.Result(send(t, anon, userText("env KEY", "", ""))); got != "env KEY unset" {
		t.Fatalf("SendMessage() by a guest without a rule = %q, want %q", got, "env KEY unset")
	}

	setRule(t, ts, agentdir.SandboxRule{Match: []string{"guests"}, Secrets: []string{"KEY=secret:GUEST"}})

	if got := a2atext.Result(send(t, anon, userText("env KEY", "", ""))); got != "env KEY=guest-key" {
		t.Fatalf("SendMessage() by a guest with a rule = %q, want %q", got, "env KEY=guest-key")
	}
	for _, prompt := range []string{"env KEY", "env GUEST"} {
		want := map[string]string{"env KEY": "env KEY=owner-key", "env GUEST": "env GUEST=guest-key"}[prompt]
		if got := a2atext.Result(send(t, ben, userText(prompt, "", ""))); got != want {
			t.Fatalf("SendMessage(%q) by a pairing after the guest rule = %q, want %q", prompt, got, want)
		}
	}
}

func TestSandboxOfFailsWhenTheStoreCantTell(t *testing.T) {
	t.Parallel()
	st, err := store.Open(filepath.Join(t.TempDir(), "server.db"))
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	s := newSandboxes(func() *agentdir.Config { return &agentdir.Config{} }, st)

	if _, err := s.of(context.Background(), agentkey.User("email:ann@example.com")); err == nil {
		t.Fatalf("of() with a closed store error = nil, want an error")
	}
}

func TestPostedTurnsDontSaveTheIdentityOfTheirUser(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openStore(t)
	s := &Server{sandboxes: newSandboxes(func() *agentdir.Config { return &agentdir.Config{} }, st)}
	ann := auth.Principal{ID: "email:ann@example.com", Role: auth.RoleMember, Identity: auth.Identity{Network: network.ListenerPrivate}}

	release, err := s.admitTurn(auth.InProcess(auth.Posted(ctx), ann), agentkey.User(ann.ID), nil)
	if err != nil {
		t.Fatalf("admitTurn() error = %v", err)
	}
	release()

	if _, err := st.UserIdentity(ctx, ann.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("UserIdentity() after a posted turn error = %v, want %v", err, store.ErrNotFound)
	}
}

func TestMessengerTurnsSaveTheIdentityOfTheirUser(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openStore(t)
	s := &Server{sandboxes: newSandboxes(func() *agentdir.Config { return &agentdir.Config{} }, st)}
	ann := auth.Principal{ID: "email:ann@example.com", Role: auth.RoleMember, Identity: auth.Identity{Network: network.ListenerPrivate}}

	release, err := s.admitTurn(auth.InProcess(ctx, ann), agentkey.User(ann.ID), nil)
	if err != nil {
		t.Fatalf("admitTurn() error = %v", err)
	}
	release()

	if _, err := st.UserIdentity(ctx, ann.ID); err != nil {
		t.Fatalf("UserIdentity() after a messenger turn error = %v, want nil", err)
	}
}

func TestTurnRightAfterTheLastTaskEndedRestartsTheContainer(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openStore(t)
	rules := []agentdir.SandboxRule{{Match: []string{"pairing:ann"}, Secrets: []string{"KEY"}}}
	s := &Server{
		sandboxes: newSandboxes(func() *agentdir.Config {
			return &agentdir.Config{Secrets: []string{"KEY"}, Policy: agentdir.PolicyConfig{Sandbox: rules}}
		}, st),
		tasks: st.Tasks("", func(context.Context) (string, bool, error) { return "", true, nil }),
	}
	ann := auth.Principal{ID: auth.PairPrincipal("ann"), Role: auth.RoleMember, Identity: auth.Identity{Pairing: "ann"}}
	key := agentkey.User(ann.ID)
	proc := &countingCloser{}
	s.users.started(s.users.get(key), proc, access.Sandbox{Storage: s.sandboxes.resolve(ann).Storage})
	time.AfterFunc(50*time.Millisecond, s.users.turn(key))

	release, err := s.admitTurn(auth.InProcess(ctx, ann), key, nil)

	if err != nil {
		t.Fatalf("admitTurn() while the turn of an ended task finishes error = %v, want nil", err)
	}
	release()
	if proc.closed != 1 {
		t.Fatalf("admitTurn() closed the container %d times, want 1", proc.closed)
	}
}
