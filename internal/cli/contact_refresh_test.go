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
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
)

func TestContactCardsOfStoppedAgent(t *testing.T) {
	t.Parallel()
	var desc atomic.Value
	desc.Store("Writes Go.")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		card := a2a.AgentCard{Name: "helper", Description: desc.Load().(string), Skills: []a2a.AgentSkill{{ID: "review", Name: "Code review", Description: "Reviews pull requests."}}}
		if err := json.NewEncoder(w).Encode(card); err != nil {
			t.Errorf("encoding the card: %v", err)
		}
	}))
	t.Cleanup(srv.Close)
	d := newTestAgent(t, &agentdir.Config{Name: "coder", Harness: "opencode", Port: 4100})
	run := func(args ...string) string {
		t.Helper()
		out, err := runCMD(t, append(args, "--dir", d.Root)...)
		if err != nil {
			t.Fatalf("a2a-serve %s error = %v", strings.Join(args, " "), err)
		}
		return out
	}

	steps := []struct {
		args []string
		desc string
		want []string
	}{
		{args: []string{"contact", "add", "helper", "--url", srv.URL}, want: []string{"✓ the agent can call helper", "  Writes Go.\n", "    review  Code review: Reviews pull requests.\n"}},
		{args: []string{"contact", "list"}, want: []string{"SKILLS", "review"}},
		{args: []string{"contact", "refresh", "helper"}, desc: "Reviews Go.", want: []string{"✓ refreshed helper", "  Reviews Go.\n"}},
		{args: []string{"contact", "refresh"}, desc: "Writes docs.", want: []string{"✓ refreshed helper", "  Writes docs.\n"}},
	}
	for _, step := range steps {
		if step.desc != "" {
			desc.Store(step.desc)
		}
		out := run(step.args...)
		for _, want := range step.want {
			if !strings.Contains(out, want) {
				t.Fatalf("a2a-serve %s = %q, want it to contain %q", strings.Join(step.args, " "), out, want)
			}
		}
	}
	if _, err := runCMD(t, "contact", "refresh", "ben", "--dir", d.Root); err == nil || !strings.Contains(err.Error(), "no contact ben") {
		t.Fatalf("a2a-serve contact refresh of an unknown contact error = %v, want no contact", err)
	}
}
