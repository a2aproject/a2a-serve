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

package network

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestExplainJoin(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		err         error
		wantProblem string
	}{
		{name: "truncated invite", err: errors.New("enrollment failed: Invalid bootstrap token"), wantProblem: "the relay doesn't know this invite; check that it was copied whole"},
		{name: "used invite", err: errors.New("enrollment failed: Bootstrap token max usages exceeded"), wantProblem: "the invite was already used"},
		{name: "expired invite", err: errors.New("enrollment failed: Bootstrap token expired"), wantProblem: "the invite expired"},
		{name: "revoked invite", err: errors.New("enrollment failed: Bootstrap token revoked"), wantProblem: "the invite was revoked"},
		{name: "spent invite", err: errors.New("enrollment failed: Bootstrap token expired, revoked or exhausted"), wantProblem: "the invite expired, was revoked or was already used"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := explainJoin(tc.err)
			var invite *InviteError
			if !errors.As(err, &invite) {
				t.Fatalf("explainJoin() error = %v, want an InviteError", err)
			}
			if invite.Problem != tc.wantProblem {
				t.Fatalf("explainJoin() problem = %q, want %q", invite.Problem, tc.wantProblem)
			}
			if msg := err.Error(); strings.Contains(strings.ToLower(msg), "bootstrap") || !strings.Contains(msg, "a2a-serve relay invite") {
				t.Fatalf("explainJoin() error = %q, want the server's terms and the next step", msg)
			}
		})
	}
}

func TestExplainJoinKeepsOtherErrors(t *testing.T) {
	t.Parallel()
	other := errors.New("dial tcp: connection refused")
	if err := explainJoin(other); err != other {
		t.Fatalf("explainJoin() error = %v, want %v", err, other)
	}
}

func TestInviteProblemIsKeptUntilAJoinSucceeds(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if got, err := InviteProblem(dir); err != nil || got != "" {
		t.Fatalf("InviteProblem() before a join = %q, %v, want none", got, err)
	}

	if err := RecordJoin(dir, explainJoin(errors.New("enrollment failed: Bootstrap token max usages exceeded"))); err != nil {
		t.Fatalf("RecordJoin(refused invite) error = %v, want nil", err)
	}
	if got, err := InviteProblem(dir); err != nil || got != "the invite was already used" {
		t.Fatalf("InviteProblem() after a refused invite = %q, %v, want the invite was already used", got, err)
	}

	if err := RecordJoin(dir, errors.New("dial tcp: connection refused")); err != nil {
		t.Fatalf("RecordJoin(offline relay) error = %v, want nil", err)
	}
	if got, err := InviteProblem(dir); err != nil || got != "the invite was already used" {
		t.Fatalf("InviteProblem() after an offline relay = %q, %v, want the earlier problem", got, err)
	}

	if err := RecordJoin(dir, nil); err != nil {
		t.Fatalf("RecordJoin(joined) error = %v, want nil", err)
	}
	if got, err := InviteProblem(dir); err != nil || got != "" {
		t.Fatalf("InviteProblem() after a join = %q, %v, want none", got, err)
	}
}

func TestCheckPeerResponse(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		status      int
		contentType string
		body        string
		wantErr     bool
	}{
		{name: "stopped agent", status: http.StatusBadGateway, wantErr: true},
		{name: "stopping agent", status: http.StatusServiceUnavailable, contentType: "text/plain; charset=utf-8", body: "Service listener closed\n", wantErr: true},
		{name: "agent's own error", status: http.StatusBadGateway, contentType: "application/json", body: `{"error":"contact offline"}`},
		{name: "answer", status: http.StatusOK, contentType: "application/json", body: `{}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			resp := &http.Response{StatusCode: tc.status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(tc.body))}
			if tc.contentType != "" {
				resp.Header.Set("Content-Type", tc.contentType)
			}

			got, err := checkPeerResponse(resp, nil)

			if tc.wantErr {
				if !errors.Is(err, ErrPeerUnavailable) {
					t.Fatalf("checkPeerResponse() error = %v, want %v", err, ErrPeerUnavailable)
				}
				return
			}
			if err != nil || got != resp {
				t.Fatalf("checkPeerResponse() = %v, %v, want the response", got, err)
			}
		})
	}
}
