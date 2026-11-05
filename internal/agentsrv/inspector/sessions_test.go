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

package inspector

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestLoginDropsExpiredSessions(t *testing.T) {
	t.Parallel()
	in := New(Deps{})
	in.sessions["old"] = session{expires: time.Now().Add(-time.Minute)}
	in.sessions["live"] = session{expires: time.Now().Add(time.Hour)}

	req := httptest.NewRequest(http.MethodGet, Path+"/login?code="+in.LoginCode(), nil)
	w := httptest.NewRecorder()
	in.handleLogin(w, req)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("handleLogin() status = %d, want %d", w.Code, http.StatusSeeOther)
	}
	in.mu.Lock()
	defer in.mu.Unlock()
	if _, ok := in.sessions["old"]; ok {
		t.Errorf("sessions has the expired session, want it dropped")
	}
	if _, ok := in.sessions["live"]; !ok {
		t.Errorf("sessions lost the live session, want it kept")
	}
}
