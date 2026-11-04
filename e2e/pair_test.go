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
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/go-cmp/cmp"
)

func TestPairedCallerCallsAgentUntilRevoked(t *testing.T) {
	t.Parallel()
	owner, anna := newUser(t), newUser(t)
	coder := owner.startAgent("coder")

	if _, err := anna.send(coder.url, "hello"); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("a2a send before pairing error = %v, want 401", err)
	}

	anna.pairJoin(coder.url, coder.invite("anna"))
	got := resultOf(anna.mustSend(coder.url, "hello"))
	want := taskResult{State: a2a.TaskStateCompleted, Text: "Hello from fakeacp."}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("a2a send after pairing wrong result (-want +got) diff = %s", diff)
	}

	coder.serve("pair", "rm", "anna")
	if _, err := anna.send(coder.url, "hello"); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("a2a send after pair rm error = %v, want 401", err)
	}
}

func TestAgentCallsContact(t *testing.T) {
	t.Parallel()
	owner := newUser(t)
	coder, reviewer := owner.startAgent("coder"), owner.startAgent("reviewer")

	coder.serve("contact", "add", "reviewer", "--url", reviewer.url, "--code", reviewer.invite("coder"))
	var paired pairing
	reviewer.serveJSON(&paired, "pair", "get", "coder")
	if paired.Kind != "agent" || paired.Role != "authenticated" {
		t.Fatalf("a2a-serve pair get coder on the reviewer = %+v, want an authenticated agent", paired)
	}

	got := resultOf(owner.mustSend(coder.url, "call reviewer hello"))
	if got.State != a2a.TaskStateCompleted || !strings.HasSuffix(got.Text, "TASK_STATE_COMPLETED: Hello from fakeacp.") {
		t.Fatalf("a2a send call reviewer = %+v, want completed with the reviewer's answer", got)
	}
}

type wake struct {
	Kind   string `json:"kind"`
	Node   string `json:"node"`
	TaskID string `json:"taskId"`
	Text   string `json:"text"`
}

func TestWebhookNodeAnswersAgent(t *testing.T) {
	t.Parallel()
	owner, grok := newUser(t), newUser(t)
	coder := owner.startAgent("coder")
	wakes := make(chan wake, 1)
	webhook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var wk wake
		if err := json.NewDecoder(r.Body).Decode(&wk); err != nil {
			t.Errorf("decode wake: %v", err)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		select {
		case wakes <- wk:
		default:
			t.Errorf("unexpected wake %+v", wk)
		}
	}))
	t.Cleanup(webhook.Close)

	grok.pairJoin(coder.url, coder.invite("grok", "--webhook", webhook.URL+"/hook"))
	type sent struct {
		task *a2a.Task
		err  error
	}
	done := make(chan sent, 1)
	go func() {
		task, err := owner.send(coder.url, "call grok summarize")
		done <- sent{task, err}
	}()

	var wk wake
	select {
	case wk = <-wakes:
	case <-time.After(cmdTimeout):
		t.Fatal("webhook got no wake")
	}
	if diff := cmp.Diff(wake{Kind: "task", Node: "grok", TaskID: wk.TaskID, Text: "summarize"}, wk); diff != "" {
		t.Fatalf("wake wrong result (-want +got) diff = %s", diff)
	}
	grok.run(grok.home, "a2a", "serve", "mailbox", "reply", wk.TaskID, "--final", "summary")

	res := <-done
	if res.err != nil {
		t.Fatal(res.err)
	}
	got := resultOf(res.task)
	want := taskResult{State: a2a.TaskStateCompleted, Text: fmt.Sprintf("tool result: task %s TASK_STATE_COMPLETED: summary", wk.TaskID)}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("a2a send call grok wrong result (-want +got) diff = %s", diff)
	}
}
