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

package refhost_test

import (
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/egressrules"
	"github.com/a2aproject/a2a-serve/internal/sidecar/refhost"
)

// upstream records the requests that the host forwards and answers 200.
type upstream struct {
	mu   sync.Mutex
	reqs []*http.Request
}

func (u *upstream) RoundTrip(r *http.Request) (*http.Response, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.reqs = append(u.reqs, r)
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("ok")), Header: http.Header{}, Request: r}, nil
}

func (u *upstream) last(t *testing.T) *http.Request {
	t.Helper()
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.reqs) == 0 {
		t.Fatal("the host forwarded no request")
	}
	return u.reqs[len(u.reqs)-1]
}

func TestHostReplacesPlaceholderModelKeys(t *testing.T) {
	t.Parallel()
	p, _ := egressrules.LookupProvider("gemini")
	placeholder := p.Placeholder(p.KeyEnv)
	tests := []struct {
		name       string
		url        string
		header     http.Header
		wantKey    string
		wantQuery  string
		wantHeader http.Header
	}{
		{
			name:    "key in the header",
			url:     "https://generativelanguage.googleapis.com/v1beta/models/gemini-3-flash-preview:streamGenerateContent?alt=sse",
			header:  http.Header{"X-Goog-Api-Key": {placeholder}},
			wantKey: "AIza_real", wantQuery: "alt=sse",
		},
		{
			name:    "key in the query",
			url:     "https://generativelanguage.googleapis.com/v1beta/models/gemini-3-flash-preview:generateContent?key=" + placeholder,
			wantKey: "AIza_real",
		},
		{
			name:   "endpoint that doesn't get the key",
			url:    "https://generativelanguage.googleapis.com/v1beta/files?key=" + placeholder,
			header: http.Header{"X-Goog-Api-Key": {placeholder}},
		},
		{
			name:    "another host",
			url:     "https://example.com/v1beta/models/g:generateContent",
			header:  http.Header{"X-Goog-Api-Key": {placeholder}},
			wantKey: placeholder,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			up := &upstream{}
			host := refhost.New(refhost.Config{Upstream: up, ModelKeys: map[string]string{"gemini": "AIza_real"}})
			req := httptest.NewRequest(http.MethodPost, tc.url, strings.NewReader("{}"))
			maps.Copy(req.Header, tc.header)
			rec := httptest.NewRecorder()

			host.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("Host.ServeHTTP() status = %d, want 200: %s", rec.Code, rec.Body)
			}
			got := up.last(t)
			if diff := cmp.Diff(tc.wantKey, got.Header.Get("X-Goog-Api-Key")); diff != "" {
				t.Fatalf("forwarded X-Goog-Api-Key wrong result (-want +got) diff = %s", diff)
			}
			if strings.Contains(got.URL.RawQuery, placeholder) {
				t.Fatalf("forwarded query = %q, want no placeholder", got.URL.RawQuery)
			}
			if tc.wantQuery != "" && got.URL.RawQuery != tc.wantQuery {
				t.Fatalf("forwarded query = %q, want %q", got.URL.RawQuery, tc.wantQuery)
			}
		})
	}
}

func TestParseModelKey(t *testing.T) {
	t.Parallel()
	env := map[string]string{"GEMINI_API_KEY": "AIza_real", "BAD": "a\nb"}
	getenv := func(name string) string { return env[name] }
	tests := []struct {
		name         string
		arg          string
		wantProvider string
		wantKey      string
		wantErr      string
	}{
		{name: "valid", arg: "gemini=GEMINI_API_KEY", wantProvider: "gemini", wantKey: "AIza_real"},
		{name: "no variable", arg: "gemini", wantErr: "want provider=NAME"},
		{name: "unknown provider", arg: "mistral=GEMINI_API_KEY", wantErr: `unknown model provider "mistral"`},
		{name: "unset variable", arg: "gemini=MISSING", wantErr: "the variable MISSING is unset or empty"},
		{name: "key a header can't carry", arg: "gemini=BAD", wantErr: "a character that a header can't carry"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			provider, key, err := refhost.ParseModelKey(tc.arg, getenv)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) || strings.Contains(err.Error(), "AIza_real") {
					t.Fatalf("refhost.ParseModelKey(%q) error = %v, want one with %q and without the key", tc.arg, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("refhost.ParseModelKey(%q) error = %v", tc.arg, err)
			}
			if provider != tc.wantProvider || key != tc.wantKey {
				t.Fatalf("refhost.ParseModelKey(%q) = %q, %q, want %q, %q", tc.arg, provider, key, tc.wantProvider, tc.wantKey)
			}
		})
	}
}
