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

package jsonhttp

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestErrorMessage(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		body string
		want string
	}{
		{name: "api error", body: `{"error":"no such agent"}`, want: "no such agent"},
		{name: "empty body", body: "", want: "404 Not Found"},
		{name: "text body", body: "page not found\n", want: "404 Not Found: page not found"},
		{name: "long body", body: strings.Repeat("x", 400), want: "404 Not Found: " + strings.Repeat("x", 300) + "…"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := ErrorMessage("404 Not Found", []byte(tc.body)); got != tc.want {
				t.Fatalf("ErrorMessage() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRequireJSON(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		method string
		header http.Header
		want   int
	}{
		{name: "GET", method: http.MethodGet, want: http.StatusOK},
		{name: "JSON", method: http.MethodPost, header: http.Header{"Content-Type": {"application/json; charset=utf-8"}}, want: http.StatusOK},
		{name: "A2A JSON", method: http.MethodPut, header: http.Header{"Content-Type": {"application/a2a+json"}}, want: http.StatusOK},
		{name: "A2A-Version", method: http.MethodDelete, header: http.Header{"A2a-Version": {"1.0"}}, want: http.StatusOK},
		{name: "form", method: http.MethodPost, header: http.Header{"Content-Type": {"application/x-www-form-urlencoded"}}, want: http.StatusUnsupportedMediaType},
		{name: "text", method: http.MethodPost, header: http.Header{"Content-Type": {"text/plain"}}, want: http.StatusUnsupportedMediaType},
		{name: "no content type", method: http.MethodPost, want: http.StatusUnsupportedMediaType},
	}
	h := RequireJSON(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := httptest.NewRequest(tc.method, "/", nil)
			r.Header = tc.header
			if r.Header == nil {
				r.Header = http.Header{}
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.want {
				t.Fatalf("RequireJSON() status = %d, want %d", w.Code, tc.want)
			}
		})
	}
}
