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

package control

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/agentops"
	"github.com/a2aproject/a2a-serve/internal/clierr"
	"github.com/a2aproject/a2a-serve/internal/controlapi"
	"github.com/a2aproject/a2a-serve/internal/network"
	"github.com/a2aproject/a2a-serve/internal/store"
)

func serve(t *testing.T, deps Deps, method, path, body string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	Handler(deps).ServeHTTP(rec, req)
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		var list []any
		if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
			t.Fatalf("json.Unmarshal(%q) error = %v", rec.Body.String(), err)
		}
		got = map[string]any{"list": list}
	}
	return rec.Code, got
}

// newOps returns the operations of an agent with a Telegram channel.
func newOps(t *testing.T) *agentops.Service {
	t.Helper()
	d, err := agentdir.Open(t.TempDir())
	if err != nil {
		t.Fatalf("agentdir.Open() error = %v", err)
	}
	cfg := &agentdir.Config{Name: "coder", Harness: "opencode"}
	if err := cfg.SetChannel(agentdir.ChannelTelegram, "secret:TELEGRAM_TOKEN", nil); err != nil {
		t.Fatalf("SetChannel() error = %v", err)
	}
	if err := d.SaveConfig(cfg); err != nil {
		t.Fatalf("SaveConfig() error = %v", err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "server.db"))
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})
	return agentops.New(agentops.Deps{Store: st, Runtime: agentdir.NewRuntime(d, cfg.Clone(), nil)})
}

func TestHandler(t *testing.T) {
	t.Parallel()
	ops := newOps(t)
	tests := []struct {
		name       string
		deps       Deps
		method     string
		path       string
		body       string
		wantStatus int
		want       map[string]any
	}{
		{
			name:   "status error",
			deps:   Deps{Status: func(context.Context) (*controlapi.Status, error) { return nil, errors.New("boom") }},
			method: http.MethodGet, path: "/status", wantStatus: http.StatusInternalServerError,
			want: map[string]any{"error": "boom"},
		},
		{
			name: "peers",
			deps: Deps{Peers: func(context.Context) ([]network.PeerInfo, error) {
				return []network.PeerInfo{{Name: "grok", URL: "http://grok", Online: true}}, nil
			}},
			method: http.MethodGet, path: "/peers", wantStatus: http.StatusOK,
			want: map[string]any{"list": []any{map[string]any{"name": "grok", "url": "http://grok", "online": true}}},
		},
		{
			name:   "remove channel",
			deps:   Deps{Ops: ops},
			method: http.MethodDelete, path: "/channels/telegram", wantStatus: http.StatusOK,
			want: map[string]any{"removed": "telegram"},
		},
		{
			name:   "remove unknown channel",
			deps:   Deps{Ops: ops},
			method: http.MethodDelete, path: "/channels/discord", wantStatus: http.StatusNotFound,
			want: map[string]any{"error": "no channel discord", "kind": "channel", "name": "discord", "code": clierr.CodeUsage, "hint": clierr.UsageHint},
		},
		{
			name:   "inspector login",
			deps:   Deps{InspectorLogin: func() string { return "/inspect/login?code=abc" }},
			method: http.MethodPost, path: "/inspector/login", wantStatus: http.StatusOK,
			want: map[string]any{"path": "/inspect/login?code=abc"},
		},
		{
			name:   "reload config",
			deps:   Deps{ReloadConfig: func() error { return nil }},
			method: http.MethodPost, path: "/config/reload", wantStatus: http.StatusOK,
			want: map[string]any{"applied": []any{"description", "private-access", "public-access", "allow-email", "allow-tools", "deny-tools", "disable-mcp", "owner-chat"}},
		},
		{
			name:   "reload config error",
			deps:   Deps{ReloadConfig: func() error { return errors.New("bad yaml") }},
			method: http.MethodPost, path: "/config/reload", wantStatus: http.StatusInternalServerError,
			want: map[string]any{"error": "bad yaml"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			status, got := serve(t, tc.deps, tc.method, tc.path, tc.body)
			if status != tc.wantStatus {
				t.Fatalf("Handler() status = %d, want %d", status, tc.wantStatus)
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Fatalf("Handler() wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestHandlerStatus(t *testing.T) {
	t.Parallel()
	want := &controlapi.Status{Name: "coder", Harness: "opencode", PID: 42, Tasks: map[string]int{"working": 1}}
	deps := Deps{Status: func(context.Context) (*controlapi.Status, error) { return want, nil }}
	rec := httptest.NewRecorder()

	Handler(deps).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/status", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /status status = %d, want %d", rec.Code, http.StatusOK)
	}
	var got controlapi.Status
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if diff := cmp.Diff(want, &got); diff != "" {
		t.Fatalf("GET /status wrong result (-want +got) diff = %s", diff)
	}
}

func TestHandlerStop(t *testing.T) {
	t.Parallel()
	stopped := make(chan struct{})
	deps := Deps{Stop: func() { close(stopped) }}

	status, got := serve(t, deps, http.MethodPost, "/stop", "")
	if status != http.StatusOK {
		t.Fatalf("POST /stop status = %d, want %d", status, http.StatusOK)
	}
	if diff := cmp.Diff(map[string]any{"stopping": true}, got); diff != "" {
		t.Fatalf("POST /stop wrong result (-want +got) diff = %s", diff)
	}
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatalf("POST /stop didn't stop the server")
	}
}
