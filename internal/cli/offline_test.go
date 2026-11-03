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


package cli

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/controlapi"
)

func TestManageStoppedAgent(t *testing.T) {
	t.Parallel()
	d := newTestAgent(t, &agentdir.Config{Name: "coder", Harness: "opencode", Port: 4100})
	run := func(args ...string) string {
		t.Helper()
		out, err := runCMD(t, append(args, "--dir", d.Root)...)
		if err != nil {
			t.Fatalf("a2a serve %s error = %v", strings.Join(args, " "), err)
		}
		return out
	}

	out := run("pair", "invite", "anna")
	if !strings.Contains(out, "a2a serve pair join http://127.0.0.1:4100 ") || !strings.Contains(out, "the agent is stopped") {
		t.Fatalf("a2a serve pair invite of a stopped agent = %q, want a code at its local URL and a note that it is stopped", out)
	}
	if out := run("pair", "set", "anna", "--role", "owner"); !strings.Contains(out, "✓ updated anna") {
		t.Fatalf("a2a serve pair set of a stopped agent = %q, want anna updated", out)
	}
	var all []controlapi.Pairing
	if err := json.Unmarshal([]byte(run("-o", "json", "pair", "list")), &all); err != nil || len(all) != 1 || all[0].Role != "owner" || len(all[0].PendingCodes) != 1 {
		t.Fatalf("a2a serve pair list of a stopped agent = %+v, %v, want anna as an owner with a pending code", all, err)
	}
	if out := run("pair", "get", "anna"); !strings.Contains(out, "anna (caller, owner)") {
		t.Fatalf("a2a serve pair get of a stopped agent = %q, want anna", out)
	}
	if out := run("pair", "rm", "anna"); !strings.Contains(out, "anna can no longer call the agent") {
		t.Fatalf("a2a serve pair rm of a stopped agent = %q, want anna revoked", out)
	}

	if out := run("schedule", "add", "brief", "--cron", "@daily", "--prompt", "hello"); !strings.Contains(out, "added schedule brief") {
		t.Fatalf("a2a serve schedule add of a stopped agent = %q, want it added", out)
	}
	if out := run("schedule", "list"); !regexp.MustCompile(`brief\s+@daily`).MatchString(out) {
		t.Fatalf("a2a serve schedule list of a stopped agent = %q, want brief", out)
	}
	if out := run("-o", "jsonl", "schedule", "rm", "brief"); out != `{"kind":"schedule","name":"brief","removed":true}`+"\n" {
		t.Fatalf("a2a serve schedule rm of a stopped agent = %q, want brief removed", out)
	}

	if out := run("contact", "list"); !strings.Contains(out, "no contacts") {
		t.Fatalf("a2a serve contact list of a stopped agent = %q, want none", out)
	}
	if _, err := runCMD(t, "contact", "rm", "grok", "--dir", d.Root); err == nil || !strings.Contains(err.Error(), "no contact grok") {
		t.Fatalf("a2a serve contact rm of an unknown contact error = %v, want no contact", err)
	}
	if out := run("url", "--local"); out != "http://127.0.0.1:4100\n" {
		t.Fatalf("a2a serve url --local of a stopped agent = %q, want its saved port", out)
	}
	if _, err := runCMD(t, "url", "--dir", d.Root); err == nil {
		t.Fatalf("a2a serve url of a stopped agent error = nil, want it to need the running agent")
	}
}
