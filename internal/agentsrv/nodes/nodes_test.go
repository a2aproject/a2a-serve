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

package nodes

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/a2aproject/a2a-serve/internal/mailboxapi"
	"github.com/a2aproject/a2a-serve/internal/store"
)

func TestValidateWebhook(t *testing.T) {
	t.Parallel()
	tests := []struct {
		url     string
		wantErr bool
	}{
		{url: "https://example.com/hook"},
		{url: "http://localhost:8080/hook"},
		{url: "ftp://example.com/hook", wantErr: true},
		{url: "example.com/hook", wantErr: true},
		{url: "https://", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.url, func(t *testing.T) {
			t.Parallel()
			if err := ValidateWebhook(tc.url); (err != nil) != tc.wantErr {
				t.Fatalf("ValidateWebhook(%q) error = %v, want error %v", tc.url, err, tc.wantErr)
			}
		})
	}
}

func TestRedactURL(t *testing.T) {
	t.Parallel()
	tests := []struct {
		url  string
		want string
	}{
		{url: "https://example.com/hook", want: "https://example.com/hook"},
		{url: "https://example.com/hook?key=secret", want: "https://example.com/hook?…"},
		{url: "https://user:pass@example.com/hook", want: "https://example.com/hook"},
	}
	for _, tc := range tests {
		t.Run(tc.url, func(t *testing.T) {
			t.Parallel()
			if got := RedactURL(tc.url); got != tc.want {
				t.Fatalf("RedactURL(%q) = %q, want %q", tc.url, got, tc.want)
			}
		})
	}
}

func TestRepeatedEventInLaterTurn(t *testing.T) {
	t.Parallel()
	h := New(t.Context(), Config{})
	node := store.Node{ID: "grok"}
	ev := mailboxapi.Event{IdempotencyKey: "k1", State: "input-required", Text: "which one?"}

	post := func() mailboxapi.EventResult {
		t.Helper()
		body, err := json.Marshal(ev)
		if err != nil {
			t.Fatalf("json.Marshal() error = %v", err)
		}
		r := httptest.NewRequest(http.MethodPost, "/mailbox/api/tasks/t1/events", bytes.NewReader(body))
		r.SetPathValue("id", "t1")
		w := httptest.NewRecorder()
		h.handleEvent(w, r, "", node)
		if w.Code != http.StatusOK {
			t.Fatalf("handleEvent() status = %d, want %d: %s", w.Code, http.StatusOK, w.Body)
		}
		var res mailboxapi.EventResult
		if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
			t.Fatalf("json.Unmarshal() error = %v", err)
		}
		return res
	}
	turn := func(wantDuplicate bool) {
		t.Helper()
		w := h.openTask("t1", node.ID)
		defer h.closeTask("t1", w)
		go func() {
			select {
			case d := <-w.events:
				d.result <- nil
			case <-w.done:
			}
		}()
		if got := post(); got.Duplicate {
			t.Fatalf("handleEvent() Duplicate = %v, want false", got.Duplicate)
		}
		if got := post(); got.Duplicate != wantDuplicate {
			t.Fatalf("handleEvent() retried Duplicate = %v, want %v", got.Duplicate, wantDuplicate)
		}
	}
	turn(true)
	turn(true)
}
