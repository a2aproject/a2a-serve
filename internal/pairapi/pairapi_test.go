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

package pairapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/jsonhttp"
)

func TestPair(t *testing.T) {
	t.Parallel()
	ok := Response{Kind: KindCaller, Token: "a2a_tok", Principal: "pair:anna", Agent: "coder", URL: "http://agent"}
	tests := []struct {
		name    string
		handler http.HandlerFunc
		want    Response
		wantErr string
	}{
		{
			name: "valid code",
			handler: func(w http.ResponseWriter, r *http.Request) {
				var req Request
				if r.Method != http.MethodPost || r.URL.Path != Path || r.Header.Get("Content-Type") != "application/json" || json.NewDecoder(r.Body).Decode(&req) != nil || req.Code != "K7Q2-9XF4" {
					jsonhttp.Error(w, http.StatusBadRequest, "unexpected request")
					return
				}
				jsonhttp.Write(w, http.StatusOK, ok)
			},
			want: ok,
		},
		{
			name: "invalid code",
			handler: func(w http.ResponseWriter, r *http.Request) {
				jsonhttp.Error(w, http.StatusUnauthorized, "invalid or expired code")
			},
			wantErr: "invalid or expired code",
		},
		{
			name:    "not an agent server",
			handler: func(w http.ResponseWriter, r *http.Request) { http.Error(w, "nothing here", http.StatusNotFound) },
			wantErr: "404 Not Found: nothing here",
		},
		{
			name: "malformed response",
			handler: func(w http.ResponseWriter, r *http.Request) {
				if _, err := w.Write([]byte("{")); err != nil {
					t.Errorf("Write() error = %v", err)
				}
			},
			wantErr: "pairing with",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(tc.handler)
			t.Cleanup(srv.Close)

			got, err := Pair(t.Context(), srv.Client(), srv.URL+"/", "K7Q2-9XF4")

			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("Pair() error = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Pair() error = %v, want nil", err)
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Fatalf("Pair() wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}
