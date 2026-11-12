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
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTokenProblemIsKeptUntilAJoinSucceeds(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if got, _, err := TokenProblem(dir); err != nil || got != "" {
		t.Fatalf("TokenProblem() before a join = %q, %v, want none", got, err)
	}

	refused := &TokenError{Problem: "the invite was already used", Fix: tokenFix("ask for a new invite")}
	if err := RecordJoin(dir, refused); err != nil {
		t.Fatalf("RecordJoin(refused token) error = %v, want nil", err)
	}
	problem, fix, err := TokenProblem(dir)
	if err != nil || problem != refused.Problem || fix != refused.Fix {
		t.Fatalf("TokenProblem() after a refused token = %q, %q, %v, want %q, %q", problem, fix, err, refused.Problem, refused.Fix)
	}

	if err := RecordJoin(dir, errors.New("dial tcp: connection refused")); err != nil {
		t.Fatalf("RecordJoin(offline network) error = %v, want nil", err)
	}
	if got, _, err := TokenProblem(dir); err != nil || got != refused.Problem {
		t.Fatalf("TokenProblem() after an offline network = %q, %v, want the earlier problem", got, err)
	}

	if err := RecordJoin(dir, nil); err != nil {
		t.Fatalf("RecordJoin(joined) error = %v, want nil", err)
	}
	if got, _, err := TokenProblem(dir); err != nil || got != "" {
		t.Fatalf("TokenProblem() after a join = %q, %v, want none", got, err)
	}
}

func TestTokenProblemReadsLegacyInviteProblem(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, legacyInviteProblemFile), []byte("the invite expired\n"), 0o600); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}
	problem, fix, err := TokenProblem(dir)
	if err != nil || problem != "the invite expired" || !strings.Contains(fix, "a2a-serve relay invite") {
		t.Fatalf("TokenProblem() = %q, %q, %v, want the legacy problem and the relay invite fix", problem, fix, err)
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
