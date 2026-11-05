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

//go:build e2e

package e2e

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// pairing is the part of `pair list -o jsonl` and `pair get -o json` these
// tests read.
type pairing struct {
	Name   string `json:"name"`
	Kind   string `json:"kind"`
	Role   string `json:"role"`
	Tokens []struct {
		URL string `json:"url"`
	} `json:"tokens"`
	PendingCodes []string `json:"pendingCodes"`
	Node         *struct {
		Webhook     string   `json:"webhook"`
		Description string   `json:"description"`
		HeaderNames []string `json:"headerNames"`
		Paired      bool     `json:"paired"`
	} `json:"node"`
}

// jsonl decodes one JSON object per line.
func jsonl[T any](t *testing.T, out string) []T {
	t.Helper()
	var all []T
	for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		var v T
		if err := json.Unmarshal([]byte(line), &v); err != nil {
			t.Fatalf("json.Unmarshal(%q) error = %v", line, err)
		}
		all = append(all, v)
	}
	return all
}

func (a *agent) mustFail(want string, args ...string) {
	a.u.t.Helper()
	out, err := a.u.exec(a.dir, append([]string{"a2a", "serve"}, args...)...)
	if err == nil || !strings.Contains(err.Error(), want) {
		a.u.t.Fatalf("a2a-serve %s = %q, %v, want an error containing %q", strings.Join(args, " "), out, err, want)
	}
}

// TestPairingsOnBothSides covers invite, join, get, set, rotate and rm on
// the agent and the caller, and the JSON lists.
func TestPairingsOnBothSides(t *testing.T) {
	t.Parallel()
	owner, anna := newUser(t), newUser(t)
	coder := owner.startAgent("coder")

	anna.pairJoin(coder.url, coder.invite("anna"))
	coder.invite("anna")
	coder.mustFail("a2a-serve pair set anna --owner", "pair", "invite", "anna", "--owner")

	var got pairing
	coder.serveJSON(&got, "pair", "get", "anna")
	if got.Kind != "person" || got.Role != "authenticated" || len(got.Tokens) != 1 || len(got.PendingCodes) != 1 {
		t.Fatalf("a2a-serve pair get anna = %+v, want an authenticated person with the token of the first code and the second code pending", got)
	}

	coder.serve("pair", "set", "anna", "--owner", "--kind", "device")
	all := jsonl[pairing](t, coder.serve("pair", "list", "--invited", "-o", "jsonl"))
	kinds := map[string]string{}
	for _, p := range all {
		kinds[p.Name] = p.Kind + "/" + p.Role
	}
	if diff := cmp.Diff(map[string]string{"anna": "device/owner", "local": "person/owner"}, kinds); diff != "" {
		t.Fatalf("a2a-serve pair list -o jsonl wrong result (-want +got) diff = %s", diff)
	}

	var joined []struct {
		URL   string `json:"url"`
		Agent string `json:"agent"`
	}
	anna.runJSON(anna.home, &joined, "a2a", "serve", "pair", "list", "--joined")
	if len(joined) != 1 || joined[0].URL != coder.url || joined[0].Agent != "coder" {
		t.Fatalf("a2a-serve pair list --joined outside an agent directory = %+v, want coder at %s", joined, coder.url)
	}
	if out := resultOf(anna.mustSend(coder.url, "hello")); out.Text != "Hello from fakeacp." {
		t.Fatalf("a2a send by anna = %+v, want the agent's answer", out)
	}

	coder.serve("pair", "set", "anna", "--rotate")
	if _, err := anna.send(coder.url, "hello"); err == nil {
		t.Fatalf("a2a send with a rotated token error = nil, want error")
	}
	anna.run(anna.home, "a2a", "serve", "pair", "rm", coder.url)
	coder.serve("pair", "delete", "anna")
	coder.mustFail("no pairing anna", "pair", "get", "anna")
}

// TestWebhookNodeChangedInPlace covers a webhook node: pair set changes it
// while its token keeps working, the node reads its mailbox, and
// --rotate revokes the token.
func TestWebhookNodeChangedInPlace(t *testing.T) {
	t.Parallel()
	owner, grok := newUser(t), newUser(t)
	coder := owner.startAgent("coder")
	webhook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	t.Cleanup(webhook.Close)

	grok.pairJoin(coder.url, coder.invite("grok", "--webhook", webhook.URL+"/hook"))
	coder.mustFail("a2a-serve pair set grok", "pair", "invite", "grok", "--webhook", webhook.URL+"/other")
	coder.serve("pair", "set", "grok", "--webhook", webhook.URL+"/new", "--webhook-header", "X-Key: v1", "--description", "web search")

	var got pairing
	coder.serveJSON(&got, "pair", "get", "grok")
	if got.Kind != "webhook" || got.Node == nil || got.Node.Webhook != webhook.URL+"/new" || got.Node.Description != "web search" ||
		!cmp.Equal(got.Node.HeaderNames, []string{"X-Key"}) || !got.Node.Paired {
		t.Fatalf("a2a-serve pair get grok = %+v, want the changed node, still paired", got)
	}

	task := owner.mustSend(coder.url+"/n/grok", "--async", "summarize")
	entries := jsonl[struct {
		Envelope struct {
			TaskID string `json:"taskId"`
			Text   string `json:"text"`
		} `json:"envelope"`
	}](t, grok.run(grok.home, "a2a", "serve", "mailbox", "list", "-o", "jsonl"))
	if len(entries) != 1 || entries[0].Envelope.TaskID != string(task.ID) || entries[0].Envelope.Text != "summarize" {
		t.Fatalf("a2a-serve mailbox list -o jsonl = %+v, want the task %s", entries, task.ID)
	}
	if out := grok.run(grok.home, "a2a", "serve", "mailbox", "get", string(task.ID)); !strings.Contains(out, "summarize") {
		t.Fatalf("a2a-serve mailbox get = %q, want the task", out)
	}
	grok.run(grok.home, "a2a", "serve", "mailbox", "reply", string(task.ID), "--final", "summary")

	coder.serve("pair", "set", "grok", "--rotate")
	if out, err := grok.exec(grok.home, "a2a", "serve", "mailbox", "list"); err == nil || !strings.Contains(err.Error(), "invalid node token") {
		t.Fatalf("a2a-serve mailbox list with a rotated token = %q, %v, want it refused", out, err)
	}
}

// TestScheduleChatVocabulary covers the chat vocabulary: schedule add takes
// --chat and --thread, and the owner chat is a setting.
func TestScheduleChatVocabulary(t *testing.T) {
	t.Parallel()
	owner := newUser(t)
	coder := owner.startAgent("coder")

	coder.serve("schedule", "add", "brief", "--cron", "0 7 * * 1-5", "--prompt", "brief me", "--thread", "same")
	all := jsonl[struct {
		Name   string `json:"name"`
		Thread string `json:"thread"`
	}](t, coder.serve("schedule", "list", "-o", "jsonl"))
	if len(all) != 1 || all[0].Name != "brief" || all[0].Thread != "same" {
		t.Fatalf("a2a-serve schedule list -o jsonl = %+v, want brief in the same thread", all)
	}
	coder.mustFail("owner has no chat on telegram", "schedule", "add", "x", "--cron", "@daily", "--prompt", "p", "--chat", "telegram:owner")
	coder.mustFail("unknown flag: --deliver", "schedule", "add", "x", "--cron", "@daily", "--prompt", "p", "--deliver", "telegram")
	var set struct {
		Applied []string `json:"applied"`
	}
	coder.serveJSON(&set, "config", "set", "--owner-chat", "discord")
	if diff := cmp.Diff([]string{"owner-chat"}, set.Applied); diff != "" {
		t.Fatalf("a2a-serve config set --owner-chat -o json applied wrong result (-want +got) diff = %s", diff)
	}
}

// TestAccessSettings covers the access settings: a running agent applies
// them at once, and status and every change say who may call.
func TestAccessSettings(t *testing.T) {
	t.Parallel()
	owner := newUser(t)
	coder := owner.startAgent("coder")

	type access struct {
		Owner         []string `json:"owner"`
		Authenticated []string `json:"authenticated"`
	}
	var set struct {
		Applied []string `json:"applied"`
		Access  access   `json:"access"`
	}
	coder.serveJSON(&set, "config", "set", "--private-access", "paired", "--allow-email", "*@example.com")
	var status struct {
		Access access `json:"access"`
	}
	coder.serveJSON(&status, "status")

	wantAccess := access{Owner: []string{"self"}, Authenticated: []string{"email:*@example.com"}}
	if diff := cmp.Diff([]string{"private-access", "allow-email"}, set.Applied); diff != "" {
		t.Fatalf("a2a-serve config set -o json applied wrong result (-want +got) diff = %s", diff)
	}
	if diff := cmp.Diff(wantAccess, set.Access); diff != "" {
		t.Fatalf("a2a-serve config set -o json access wrong result (-want +got) diff = %s", diff)
	}
	if diff := cmp.Diff(wantAccess, status.Access); diff != "" {
		t.Fatalf("a2a-serve status -o json access wrong result (-want +got) diff = %s", diff)
	}
	coder.mustFail("unknown flag: --allow", "config", "set", "--allow", "email:me@example.com")
	coder.mustFail(`invalid --public "ngrok"`, "config", "set", "--public", "ngrok")
}

// TestManageStoppedAgent covers pairings and schedules that change while
// the agent is stopped, and the agent picks them up when it starts.
func TestManageStoppedAgent(t *testing.T) {
	t.Parallel()
	owner, bob := newUser(t), newUser(t)
	coder := owner.startAgent("coder")
	coder.stop()

	code := coder.invite("bob")
	coder.serve("schedule", "add", "nightly", "--cron", "@daily", "--prompt", "x")
	coder.mustFail("is not running", "peers")
	if got := strings.TrimSpace(coder.serve("status", "--url=local")); got != coder.url {
		t.Fatalf("a2a-serve status --url=local of a stopped agent = %q, want %q", got, coder.url)
	}

	coder.serve("start", "--detach")
	coder.stopped = false
	bob.pairJoin(coder.url, code)
	if out := resultOf(bob.mustSend(coder.url, "hello")); out.Text != "Hello from fakeacp." {
		t.Fatalf("a2a send with a code issued while stopped = %+v, want the agent's answer", out)
	}
	all := jsonl[struct {
		Name string `json:"name"`
	}](t, coder.serve("schedule", "list", "-o", "jsonl"))
	if len(all) != 1 || all[0].Name != "nightly" {
		t.Fatalf("a2a-serve schedule list -o jsonl after the start = %+v, want nightly", all)
	}
}

// TestCommandHelp covers the help: bare a2a serve prints the command groups,
// and a2a-serve --help names a2a-serve.
func TestCommandHelp(t *testing.T) {
	t.Parallel()
	u := newUser(t)
	help := u.run(u.home, "a2a", "serve")
	for _, group := range []string{"Run the agent:", "Configure the agent:", "Pairing and contacts:", "Webhook node:", "Setup:"} {
		if !strings.Contains(help, group) {
			t.Fatalf("a2a-serve = %q, want the group %q", help, group)
		}
	}
	standalone := u.run(u.home, "a2a-serve", "--help")
	if !strings.Contains(standalone, "a2a-serve [command]") || !strings.Contains(standalone, "a2a-serve start opencode") {
		t.Fatalf("a2a-serve --help = %q, want help which names a2a-serve", standalone)
	}
}
