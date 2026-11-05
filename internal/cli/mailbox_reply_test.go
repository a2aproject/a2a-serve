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
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/mailboxapi"
)

func TestMailboxReplyStates(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		args []string
		want mailboxapi.Event
	}{
		{name: "working", args: []string{"--working", "on it"}, want: mailboxapi.Event{State: "working", Text: "on it"}},
		{name: "status working", args: []string{"--status", "working", "--text", "on it"}, want: mailboxapi.Event{State: "working", Text: "on it"}},
		{name: "reject", args: []string{"--reject", "out of scope"}, want: mailboxapi.Event{State: "rejected", Text: "out of scope"}},
		{name: "status rejected", args: []string{"--status", "rejected", "--text", "out of scope"}, want: mailboxapi.Event{State: "rejected", Text: "out of scope"}},
		{name: "final", args: []string{"--final", "done"}, want: mailboxapi.Event{State: "completed", Text: "done"}},
		{name: "input required", args: []string{"--input-required", "which branch?"}, want: mailboxapi.Event{State: "input-required", Text: "which branch?"}},
		{name: "fail", args: []string{"--fail", "no repo"}, want: mailboxapi.Event{State: "failed", Text: "no repo"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var got mailboxapi.Event
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
					t.Errorf("decoding the event error = %v", err)
				}
				if err := json.NewEncoder(w).Encode(mailboxapi.EventResult{TaskID: "t1", State: got.State}); err != nil {
					t.Errorf("encoding the result error = %v", err)
				}
			}))
			defer srv.Close()
			token := mailboxapi.NewNodeToken(srv.URL+mailboxapi.Prefix, "n1", "secret")
			args := append([]string{"mailbox", "reply", "t1", "--token", token}, tc.args...)

			if _, err := runCMD(t, args...); err != nil {
				t.Fatalf("a2a-serve %s error = %v", strings.Join(tc.args, " "), err)
			}

			got.IdempotencyKey = ""
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Fatalf("a2a-serve mailbox reply %s posted wrong event (-want +got) diff = %s", strings.Join(tc.args, " "), diff)
			}
		})
	}
}

func TestMailboxReplyRejectsTwoStates(t *testing.T) {
	t.Parallel()
	token := mailboxapi.NewNodeToken("http://127.0.0.1:1/mailbox", "n1", "secret")

	_, err := runCMD(t, "mailbox", "reply", "t1", "--token", token, "--working", "a", "--reject", "b")

	if !isUsage(err) || !strings.Contains(err.Error(), "--working") {
		t.Fatalf("a2a-serve mailbox reply with two states error = %v, want a usage error naming --working", err)
	}
}
