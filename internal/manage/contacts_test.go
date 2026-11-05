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

package manage

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/a2aproject/a2a-serve/internal/store"
)

func TestTokenClientKeepsRedirectPolicy(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/final", http.StatusFound)
	})
	mux.HandleFunc("/final", func(w http.ResponseWriter, r *http.Request) {})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	base := &http.Client{Transport: srv.Client().Transport, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}

	resp, err := TokenClient(base, store.Contact{URL: srv.URL}).Get(srv.URL)
	if err != nil {
		t.Fatalf("TokenClient().Get() error = %v", err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatalf("closing the response: %v", err)
	}

	if resp.StatusCode != http.StatusFound {
		t.Fatalf("TokenClient().Get() status = %d, want %d from the base client's redirect policy", resp.StatusCode, http.StatusFound)
	}
}
