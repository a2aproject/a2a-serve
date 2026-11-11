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

package agentsrv

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRequireHost(t *testing.T) {
	t.Parallel()
	tests := []struct {
		host string
		want int
	}{
		{host: "coder.hs.test", want: http.StatusOK},
		{host: "coder.hs.test:80", want: http.StatusOK},
		{host: "CODER.hs.test", want: http.StatusOK},
		{host: "coder.hs.test.", want: http.StatusOK},
		{host: "evil.example", want: http.StatusMisdirectedRequest},
		{host: "evil.example:80", want: http.StatusMisdirectedRequest},
		{host: "100.64.0.1", want: http.StatusMisdirectedRequest},
		{host: "coder.hs.test:8080", want: http.StatusMisdirectedRequest},
		{host: "", want: http.StatusMisdirectedRequest},
	}
	h := requireHost("coder.hs.test", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	for _, tc := range tests {
		t.Run(tc.host, func(t *testing.T) {
			t.Parallel()
			r := httptest.NewRequest(http.MethodPost, "http://coder.hs.test/", nil)
			r.Host = tc.host
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.want {
				t.Fatalf("requireHost() status for Host %q = %d, want %d", tc.host, w.Code, tc.want)
			}
		})
	}
}
