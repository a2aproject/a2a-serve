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

package agentctl

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/agenterr"
	"github.com/a2aproject/a2a-serve/internal/controlapi"
	"github.com/a2aproject/a2a-serve/internal/fsutil"
	"github.com/a2aproject/a2a-serve/internal/harness"
	"github.com/a2aproject/a2a-serve/internal/jsonhttp"
)

// fakeServer answers on the control socket of an agent like a running
// server, and records the calls it gets.
type fakeServer struct {
	mu         sync.Mutex
	calls      []string
	bodies     map[string]string
	pid        int
	running    *agentdir.Config
	noReload   bool
	noConfig   bool
	messengers []controlapi.MessengerStatus
	access     *controlapi.Access
}

func (f *fakeServer) record(r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		panic(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	call := r.Method + " " + r.URL.Path
	f.calls = append(f.calls, call)
	f.bodies[call] = string(body)
}

func (f *fakeServer) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

func (f *fakeServer) Body(call string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.bodies[call]
}

func serveFake(t *testing.T, d agentdir.Dir, f *fakeServer) {
	t.Helper()
	f.bodies = map[string]string{}
	path := d.SocketPath()
	if err := fsutil.MkdirPrivate(filepath.Dir(path)); err != nil {
		t.Fatalf("fsutil.MkdirPrivate() error = %v", err)
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("net.Listen() error = %v", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		f.record(r)
		jsonhttp.Write(w, http.StatusOK, controlapi.Status{Name: "coder", PID: f.pid, Messengers: f.messengers, Access: f.access})
	})
	mux.HandleFunc("GET /config", func(w http.ResponseWriter, r *http.Request) {
		f.record(r)
		if f.noConfig {
			http.Error(w, "broken", http.StatusInternalServerError)
			return
		}
		jsonhttp.Write(w, http.StatusOK, f.running)
	})
	mux.HandleFunc("POST /messengers", func(w http.ResponseWriter, r *http.Request) {
		f.record(r)
		jsonhttp.Write(w, http.StatusOK, controlapi.MessengerStatus{Name: "telegram"})
	})
	mux.HandleFunc("DELETE /messengers/{kind}", func(w http.ResponseWriter, r *http.Request) {
		f.record(r)
		jsonhttp.Write(w, http.StatusOK, map[string]string{})
	})
	mux.HandleFunc("POST /config/reload", func(w http.ResponseWriter, r *http.Request) {
		f.record(r)
		if f.noReload {
			http.NotFound(w, r)
			return
		}
		jsonhttp.Write(w, http.StatusOK, controlapi.ReloadResponse{Applied: []string{"description", "approver"}})
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: time.Second}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("Serve() error = %v", err)
		}
	}()
	t.Cleanup(func() {
		if err := srv.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})
}

func newAgent(t *testing.T, cfg *agentdir.Config, secrets agentdir.Secrets) agentdir.Dir {
	t.Helper()
	d, err := agentdir.Open(t.TempDir())
	if err != nil {
		t.Fatalf("agentdir.Open() error = %v", err)
	}
	if err := d.SaveConfig(cfg); err != nil {
		t.Fatalf("SaveConfig() error = %v", err)
	}
	if err := d.SaveSecrets(secrets); err != nil {
		t.Fatalf("SaveSecrets() error = %v", err)
	}
	return d
}

func loadConfig(t *testing.T, d agentdir.Dir) *agentdir.Config {
	t.Helper()
	cfg, err := d.ReadConfig()
	if err != nil {
		t.Fatalf("ReadConfig() error = %v", err)
	}
	return cfg
}

func TestSetStopped(t *testing.T) {
	t.Parallel()
	d := newAgent(t, &agentdir.Config{Name: "coder", Harness: "opencode", Secrets: []string{"KEY"}}, agentdir.Secrets{"KEY": "old"})
	got, err := Set(t.Context(), d, controlapi.NewClient(d), agentdir.Settings{
		Sandbox:      "local",
		AllowTools:   []string{"read"},
		Secrets:      []string{"KEY", "NEW"},
		SecretValues: agentdir.Secrets{"KEY": "rotated", "NEW": "n"},
	}, false)
	if err != nil {
		t.Fatalf("Set() error = %v", err)
	}
	want := &Result{
		Name: "coder", Path: d.ConfigPath(),
		Changes: []agentdir.Change{
			{Setting: "sandbox", New: "local"},
			{Setting: "secret", Added: []string{"NEW"}},
			{Setting: "allow-tools", Added: []string{"read"}, Live: true},
		},
		Secrets: []SecretChange{{Name: "KEY", Action: SecretRotated}, {Name: "NEW", Action: SecretStored}},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("Set() wrong result (-want +got) diff = %s", diff)
	}
	if cfg := loadConfig(t, d); cfg.Sandbox != "local" {
		t.Fatalf("saved sandbox = %q, want local", cfg.Sandbox)
	}
	secrets, err := d.LoadSecrets()
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(agentdir.Secrets{"KEY": "rotated", "NEW": "n"}, secrets); diff != "" {
		t.Fatalf("saved secrets wrong result (-want +got) diff = %s", diff)
	}
}

func TestSetOwnerChatWithItsMessenger(t *testing.T) {
	t.Parallel()
	d := newAgent(t, &agentdir.Config{
		Name: "coder", Harness: "opencode",
		Messengers: agentdir.MessengersConfig{Telegram: &agentdir.TelegramConfig{Token: "secret:" + agentdir.SecretTelegram}},
	}, agentdir.Secrets{agentdir.SecretTelegram: "token"})

	if _, err := Set(t.Context(), d, controlapi.NewClient(d), agentdir.Settings{OwnerChat: "telegram:anna"}, false); err != nil {
		t.Fatalf("Set() error = %v", err)
	}

	if got := loadConfig(t, d).Approvals.OwnerChat; got != "telegram:anna" {
		t.Fatalf("saved owner chat = %q, want %q", got, "telegram:anna")
	}
}

func TestSetRejectsWithoutSaving(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		s       agentdir.Settings
		dryRun  bool
		wantErr bool
	}{
		{name: "invalid", s: agentdir.Settings{Private: "tailscal"}, wantErr: true},
		{name: "missing mount", s: agentdir.Settings{Mounts: []string{"/nonexistent-a2a"}}, wantErr: true},
		{name: "secret with a line break", s: agentdir.Settings{Secrets: []string{"MULTI"}, SecretValues: agentdir.Secrets{"MULTI": "line1\nline2"}}, wantErr: true},
		{name: "owner chat without its messenger", s: agentdir.Settings{OwnerChat: "telegram:anna"}, wantErr: true},
		{name: "dry run", s: agentdir.Settings{Sandbox: "local"}, dryRun: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			want := &agentdir.Config{Name: "coder", Harness: "opencode"}
			d := newAgent(t, want, nil)
			_, err := Set(t.Context(), d, controlapi.NewClient(d), tc.s, tc.dryRun)
			var se *agentdir.SettingsError
			if tc.wantErr != errors.As(err, &se) {
				t.Fatalf("Set() error = %v, want a SettingsError %v", err, tc.wantErr)
			}
			if diff := cmp.Diff(want, loadConfig(t, d)); diff != "" {
				t.Fatalf("Set() saved the config (-want +got) diff = %s", diff)
			}
			secrets, err := d.LoadSecrets()
			if err != nil {
				t.Fatalf("LoadSecrets() error = %v", err)
			}
			if diff := cmp.Diff(agentdir.Secrets{}, secrets); diff != "" {
				t.Fatalf("Set() saved secrets (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestSetCreatingInsideAnotherAgent(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		running string
		wantErr bool
	}{
		{name: "enclosing agents stopped"},
		{name: "outer agent running", running: "outer", wantErr: true},
		{name: "inner agent running", running: "inner", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			outer := newAgent(t, &agentdir.Config{Name: "outer", Harness: "opencode"}, nil)
			inner, err := agentdir.Open(filepath.Join(outer.Root, "inner"))
			if err != nil {
				t.Fatal(err)
			}
			if err := inner.SaveConfig(&agentdir.Config{Name: "inner", Harness: "opencode"}); err != nil {
				t.Fatal(err)
			}
			agents := map[string]agentdir.Dir{"outer": outer, "inner": inner}
			if tc.running != "" {
				serveFake(t, agents[tc.running], &fakeServer{})
			}
			d, err := agentdir.Open(filepath.Join(inner.Root, "sub", "deeper"))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(d.Root, 0o755); err != nil {
				t.Fatal(err)
			}

			got, err := Set(t.Context(), d, controlapi.NewClient(d), agentdir.Settings{Harness: "opencode", Name: "nested"}, false)

			if !tc.wantErr {
				if err != nil || !got.Created {
					t.Fatalf("Set() = %+v, %v, want the agent created", got, err)
				}
				return
			}
			if want := "is inside the agent in " + agents[tc.running].Root + ", which is running"; err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("Set() error = %v, want %q", err, want)
			}
			if d.Exists() {
				t.Fatalf("Set() created the agent in %s", d.Root)
			}
		})
	}
}

func TestSetWarnings(t *testing.T) {
	t.Parallel()
	d := newAgent(t, &agentdir.Config{Name: "coder", Harness: "opencode"}, nil)
	got, err := Set(t.Context(), d, controlapi.NewClient(d), agentdir.Settings{
		Secrets: []string{"OPENROUTER_API_KEY"}, SecretValues: agentdir.Secrets{"OPENROUTER_API_KEY": "k"},
	}, false)
	if err != nil {
		t.Fatalf("Set() error = %v", err)
	}
	if len(got.Warnings) != 1 {
		t.Fatalf("Set() warnings = %q, want one about OPENROUTER_API_KEY", got.Warnings)
	}
	got, err = Set(t.Context(), d, controlapi.NewClient(d), agentdir.Settings{Model: "openrouter/a/b"}, false)
	if err != nil {
		t.Fatalf("Set() error = %v", err)
	}
	if len(got.Warnings) != 0 {
		t.Fatalf("Set() warnings = %q, want none with a model", got.Warnings)
	}
}

func TestSetDisableMCPWarnings(t *testing.T) {
	t.Parallel()
	const mcp = `{"mcpServers": {"github": {"command": "gh-mcp"}, "files": {"command": "fs-mcp"}}}`
	tests := []struct {
		name    string
		mcp     string
		stale   []string
		disable []string
		want    []string
	}{
		{name: "a server of the file", mcp: mcp, disable: []string{"github"}},
		{
			name: "a typo", mcp: mcp, disable: []string{"githb"},
			want: []string{"--disable-mcp githb names no server of .mcp.json: did you mean github?"},
		},
		{
			name: "no near name", mcp: mcp, disable: []string{"nosuch", "github"},
			want: []string{"--disable-mcp nosuch names no server of .mcp.json; it has files, github"},
		},
		{
			name: "no file", disable: []string{"github"},
			want: []string{"--disable-mcp github names no server of .mcp.json; it has none"},
		},
		{name: "a name set before", mcp: mcp, stale: []string{"nosuch"}, disable: []string{"github"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := newAgent(t, &agentdir.Config{Name: "coder", Harness: "opencode", MCP: agentdir.MCPConfig{Disabled: tc.stale}}, nil)
			if tc.mcp != "" {
				if err := os.WriteFile(filepath.Join(d.Root, ".mcp.json"), []byte(tc.mcp), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			got, err := Set(t.Context(), d, controlapi.NewClient(d), agentdir.Settings{DisableMCP: tc.disable}, false)
			if err != nil {
				t.Fatalf("Set() error = %v", err)
			}
			if diff := cmp.Diff(tc.want, got.Warnings); diff != "" {
				t.Fatalf("Set().Warnings wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestUnsetSecret(t *testing.T) {
	t.Parallel()
	d := newAgent(t, &agentdir.Config{Name: "coder", Harness: "opencode", Secrets: []string{"KEY", "OTHER"}}, agentdir.Secrets{"KEY": "k", "OTHER": "o"})
	got, err := Unset(t.Context(), d, controlapi.NewClient(d), "secret", []string{"KEY"}, false)
	if err != nil {
		t.Fatalf("Unset() error = %v", err)
	}
	if diff := cmp.Diff([]SecretChange{{Name: "KEY", Action: SecretDeleted}}, got.Secrets); diff != "" {
		t.Fatalf("Unset() wrong secrets (-want +got) diff = %s", diff)
	}
	secrets, err := d.LoadSecrets()
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(agentdir.Secrets{"OTHER": "o"}, secrets); diff != "" {
		t.Fatalf("stored secrets wrong result (-want +got) diff = %s", diff)
	}
}

func TestSetRunning(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		noReload    bool
		wantCalls   []string
		wantApplied []string
		wantRestart []string
	}{
		{
			name:        "live settings applied",
			wantCalls:   []string{"GET /status", "POST /messengers", "POST /config/reload"},
			wantApplied: []string{"telegram", "description"},
			wantRestart: []string{"sandbox"},
		},
		{
			name:        "server without reload",
			noReload:    true,
			wantCalls:   []string{"GET /status", "POST /messengers", "POST /config/reload"},
			wantApplied: []string{"telegram"},
			wantRestart: []string{"sandbox", "description"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := newAgent(t, &agentdir.Config{Name: "coder", Harness: "opencode"}, nil)
			f := &fakeServer{noReload: tc.noReload}
			serveFake(t, d, f)
			got, err := Set(t.Context(), d, controlapi.NewClient(d), agentdir.Settings{Description: "x", Sandbox: "local", Telegram: "123:abc"}, false)
			if err != nil {
				t.Fatalf("Set() error = %v", err)
			}
			if diff := cmp.Diff(tc.wantCalls, f.Calls()); diff != "" {
				t.Fatalf("Set() wrong calls (-want +got) diff = %s", diff)
			}
			var req controlapi.MessengerRequest
			if err := json.Unmarshal([]byte(f.Body("POST /messengers")), &req); err != nil {
				t.Fatalf("parsing the messenger request error = %v", err)
			}
			wantReq := controlapi.MessengerRequest{Kind: "telegram", Token: "secret:TELEGRAM_BOT_TOKEN", Secrets: agentdir.Secrets{"TELEGRAM_BOT_TOKEN": "123:abc"}}
			if diff := cmp.Diff(wantReq, req); diff != "" {
				t.Fatalf("messenger request wrong result (-want +got) diff = %s", diff)
			}
			if !got.Running {
				t.Fatalf("Set().Running = false, want true")
			}
			if diff := cmp.Diff(tc.wantApplied, got.Applied); diff != "" {
				t.Fatalf("Set().Applied wrong result (-want +got) diff = %s", diff)
			}
			if diff := cmp.Diff(tc.wantRestart, got.Restart); diff != "" {
				t.Fatalf("Set().Restart wrong result (-want +got) diff = %s", diff)
			}
			if cfg := loadConfig(t, d); cfg.Sandbox != "local" || cfg.Description != "x" {
				t.Fatalf("saved config = %+v, want sandbox local and description x", cfg)
			}
		})
	}
}

func TestSetAccess(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		running bool
		s       agentdir.Settings
		want    *controlapi.Access
	}{
		{name: "other setting", s: agentdir.Settings{Sandbox: "local"}},
		{
			name: "stopped agent", s: agentdir.Settings{Private: "tailscale", PrivateAllowIncoming: agentdir.AccessPaired},
			want: &controlapi.Access{
				PrivateAllowIncoming: agentdir.AccessPaired, Owner: []string{"self"}, Authenticated: []string{}, Anonymous: []string{},
				Summary: []string{"you: this machine and your tailnet devices", "nobody else", "the tailnet needs a pairing"},
			},
		},
		{
			name: "running agent", running: true, s: agentdir.Settings{AllowEmails: []string{"*@example.com"}},
			want: &controlapi.Access{
				Owner: []string{"self"}, Authenticated: []string{"private", "email:*@example.com"}, Anonymous: []string{},
				Pairings: 3, OwnerPairings: 1, Others: []string{"emails matching *@example.com"},
				Summary: []string{"you: this machine", "emails matching *@example.com, 3 pairings (1 owner)"},
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := newAgent(t, &agentdir.Config{Name: "coder", Harness: "opencode"}, nil)
			if tc.running {
				serveFake(t, d, &fakeServer{running: &agentdir.Config{Name: "coder", Harness: "opencode"}, access: &controlapi.Access{Pairings: 3, OwnerPairings: 1}})
			}
			got, err := Set(t.Context(), d, controlapi.NewClient(d), tc.s, false)
			if err != nil {
				t.Fatalf("Set() error = %v", err)
			}
			if diff := cmp.Diff(tc.want, got.Access); diff != "" {
				t.Fatalf("Set().Access wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestSetSandboxRuleWaitsWhenTheServerCantSay(t *testing.T) {
	t.Parallel()
	d := newAgent(t, &agentdir.Config{Name: "coder", Harness: "opencode"}, nil)
	serveFake(t, d, &fakeServer{noConfig: true})

	got, err := Set(t.Context(), d, controlapi.NewClient(d), agentdir.Settings{GuestStorage: agentdir.StorageEmpty, Description: "x"}, false)
	if err != nil {
		t.Fatalf("Set() error = %v", err)
	}

	if diff := cmp.Diff([]string{"description"}, got.Applied); diff != "" {
		t.Fatalf("Set().Applied wrong result (-want +got) diff = %s", diff)
	}
	if diff := cmp.Diff([]string{"guest-storage"}, got.Restart); diff != "" {
		t.Fatalf("Set().Restart wrong result (-want +got) diff = %s", diff)
	}
}

func TestPending(t *testing.T) {
	t.Parallel()
	d := newAgent(t, &agentdir.Config{Name: "coder", Harness: "opencode", Sandbox: "local"}, nil)
	serveFake(t, d, &fakeServer{running: &agentdir.Config{Name: "coder", Harness: "opencode"}})
	got, running, err := Pending(t.Context(), d, controlapi.NewClient(d))
	if err != nil {
		t.Fatalf("Pending() error = %v", err)
	}
	if !running {
		t.Fatalf("Pending() running = false, want true")
	}
	if diff := cmp.Diff([]string{"sandbox"}, got); diff != "" {
		t.Fatalf("Pending() wrong result (-want +got) diff = %s", diff)
	}
}

func TestWaitingForRestart(t *testing.T) {
	t.Parallel()
	running := &agentdir.Config{Name: "coder", Harness: "opencode"}
	tests := []struct {
		name     string
		running  bool
		settings agentdir.Settings
		dryRun   bool
		want     []string
	}{
		{name: "unchanged setting pending", running: true, settings: agentdir.Settings{Sandbox: "local"}, want: []string{"sandbox"}},
		{name: "change and a pending setting", running: true, settings: agentdir.Settings{Env: []string{"DEBUG=1"}}, want: []string{"env", "sandbox"}},
		{name: "dry run", running: true, settings: agentdir.Settings{Sandbox: "local"}, dryRun: true},
		{name: "stopped", settings: agentdir.Settings{Env: []string{"DEBUG=1"}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := newAgent(t, &agentdir.Config{Name: "coder", Harness: "opencode", Sandbox: "local"}, nil)
			if tc.running {
				serveFake(t, d, &fakeServer{running: running})
			}
			client := controlapi.NewClient(d)
			res, err := Set(t.Context(), d, client, tc.settings, tc.dryRun)
			if err != nil {
				t.Fatalf("Set() error = %v", err)
			}
			got, err := WaitingForRestart(t.Context(), d, client, res)
			if err != nil {
				t.Fatalf("WaitingForRestart() error = %v", err)
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Fatalf("WaitingForRestart() wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestHarnessesMatchProfiles(t *testing.T) {
	t.Parallel()
	if diff := cmp.Diff(harness.ProfileNames(), agentdir.Harnesses); diff != "" {
		t.Fatalf("agentdir.Harnesses wrong result (-want +got) diff = %s", diff)
	}
}

func TestSetConcurrentlyKeepsEveryUpdate(t *testing.T) {
	t.Parallel()
	d := newAgent(t, &agentdir.Config{Name: "coder", Harness: "opencode"}, nil)
	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		wg.Go(func() {
			_, errs[i] = Set(t.Context(), d, controlapi.NewClient(d), agentdir.Settings{Env: []string{fmt.Sprintf("VAR%d=1", i)}}, false)
		})
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("Set(%d) error = %v", i, err)
		}
	}
	var want []string
	for i := range n {
		want = append(want, fmt.Sprintf("VAR%d=1", i))
	}
	got := slices.Clone(loadConfig(t, d).Env)
	slices.Sort(got)
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("saved env wrong result (-want +got) diff = %s", diff)
	}
}

func TestApplyThen(t *testing.T) {
	t.Parallel()
	errStep := errors.New("step failed")
	tests := []struct {
		name      string
		dryRun    bool
		stepErr   error
		wantCalls int
		wantErr   error
	}{
		{name: "runs after saving", wantCalls: 1},
		{name: "dry run skips it", dryRun: true},
		{name: "fails", stepErr: errStep, wantCalls: 1, wantErr: errStep},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := newAgent(t, &agentdir.Config{Name: "coder", Harness: "opencode"}, nil)
			var calls int
			var saved string
			var lockErr error
			then := func(cfg *agentdir.Config) error {
				calls++
				saved = loadConfig(t, d).Sandbox
				_, lockErr = fsutil.TryLock(d.LockPath())
				if cfg.Sandbox != "local" {
					t.Errorf("then() got sandbox %q, want local", cfg.Sandbox)
				}
				return tc.stepErr
			}
			mutate := func(cfg *agentdir.Config) (agentdir.Secrets, []string, error) {
				cfg.Sandbox = "local"
				return nil, nil, nil
			}

			_, err := ApplyThen(t.Context(), d, controlapi.NewClient(d), mutate, then, tc.dryRun)

			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("ApplyThen() error = %v, want %v", err, tc.wantErr)
			}
			if calls != tc.wantCalls {
				t.Fatalf("ApplyThen() ran the step %d times, want %d", calls, tc.wantCalls)
			}
			if calls == 0 {
				return
			}
			if saved != "local" {
				t.Fatalf("ApplyThen() ran the step before saving config.yaml, sandbox = %q", saved)
			}
			if !errors.Is(lockErr, fsutil.ErrLocked) {
				t.Fatalf("fsutil.TryLock() during the step error = %v, want %v", lockErr, fsutil.ErrLocked)
			}
		})
	}
}

func TestApplyToInvalidFile(t *testing.T) {
	t.Parallel()
	const file = "name: coder\nagent: opencode\napprovals:\n    deny:\n        - exec\n"
	tests := []struct {
		name     string
		file     string
		apply    func(d agentdir.Dir) error
		wantErr  string
		wantHint string
		wantDeny []string
	}{
		{
			name: "unset repairs the file",
			file: file,
			apply: func(d agentdir.Dir) error {
				_, err := Unset(t.Context(), d, controlapi.NewClient(d), "deny-tools", []string{"exec"}, false)
				return err
			},
		},
		{
			name: "set names the entry in the file",
			file: file,
			apply: func(d agentdir.Dir) error {
				_, err := Set(t.Context(), d, controlapi.NewClient(d), agentdir.Settings{DenyTools: []string{"execute"}}, false)
				return err
			},
			wantErr:  `config.yaml: approvals.deny has "exec"`,
			wantHint: "fix it with `a2a-serve config unset deny-tools exec`",
			wantDeny: []string{"exec"},
		},
		{
			name: "set names the flag",
			file: "name: coder\nagent: opencode\n",
			apply: func(d agentdir.Dir) error {
				_, err := Set(t.Context(), d, controlapi.NewClient(d), agentdir.Settings{DenyTools: []string{"exec"}}, false)
				return err
			},
			wantErr: `invalid --deny-tools "exec"`,
		},
		{
			name: "a file that doesn't parse",
			file: "name: coder\nsandbox: [\n",
			apply: func(d agentdir.Dir) error {
				_, err := Set(t.Context(), d, controlapi.NewClient(d), agentdir.Settings{Sandbox: "local"}, false)
				return err
			},
			wantErr:  "config.yaml: yaml: line 2",
			wantHint: "fix it with `a2a-serve config edit`",
		},
		{
			name: "a card skill without an id",
			file: "name: coder\nagent: opencode\ncard:\n    skills:\n        - name: Review\n",
			apply: func(d agentdir.Dir) error {
				_, err := Set(t.Context(), d, controlapi.NewClient(d), agentdir.Settings{Sandbox: "local"}, false)
				return err
			},
			wantErr:  `config.yaml: card.skills has a skill named "Review": want a non-empty id`,
			wantHint: "fix it with `a2a-serve config edit`",
		},
		{
			name: "a problem without a key in the file",
			file: "name: coder\nagent: opencode\nbackup:\n    to: /backups\n",
			apply: func(d agentdir.Dir) error {
				_, err := Set(t.Context(), d, controlapi.NewClient(d), agentdir.Settings{Sandbox: "local"}, false)
				return err
			},
			wantErr:  "config.yaml: --backup needs --backup-key",
			wantHint: "fix it with `a2a-serve config edit`",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := newAgent(t, &agentdir.Config{Name: "coder", Harness: "opencode"}, nil)
			if err := os.WriteFile(d.ConfigPath(), []byte(tc.file), 0o644); err != nil {
				t.Fatal(err)
			}

			err := tc.apply(d)

			if tc.wantErr == "" && err != nil {
				t.Fatalf("apply() error = %v, want nil", err)
			}
			if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("apply() error = %v, want %q", err, tc.wantErr)
			}
			if got := agenterr.HintText(err); got != tc.wantHint {
				t.Fatalf("agenterr.HintText() = %q, want %q", got, tc.wantHint)
			}
			if got, err := os.ReadFile(d.ConfigPath()); err != nil || (tc.wantErr != "" && string(got) != tc.file) {
				t.Fatalf("config.yaml = %q, %v, want it unchanged", got, err)
			}
			if tc.wantErr != "" {
				return
			}
			if diff := cmp.Diff(tc.wantDeny, loadConfig(t, d).Approvals.Deny); diff != "" {
				t.Fatalf("saved deny-tools wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}
