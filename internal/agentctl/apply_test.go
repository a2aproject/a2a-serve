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
	"context"
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
	"github.com/google/go-cmp/cmp/cmpopts"

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

func TestSetReplacingAnInjectRuleDeletesItsSecret(t *testing.T) {
	t.Parallel()
	d := newAgent(t, &agentdir.Config{
		Name: "coder", Harness: "opencode", Secrets: []string{"OLD_GH"},
		Inject: []agentdir.InjectRule{{Host: "api.github.com", Header: "Authorization: Bearer $OLD_GH"}},
	}, agentdir.Secrets{"OLD_GH": "ghp_old"})
	_, err := Set(t.Context(), d, controlapi.NewClient(d), agentdir.Settings{
		Secrets: []string{"NEW_GH"}, SecretValues: agentdir.Secrets{"NEW_GH": "ghp_new"},
		InjectHeaders: []string{"api.github.com=Authorization: Bearer $NEW_GH"},
	}, false)
	if err != nil {
		t.Fatalf("Set() error = %v", err)
	}
	stored, err := d.LoadSecrets()
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(agentdir.Secrets{"NEW_GH": "ghp_new"}, stored); diff != "" {
		t.Fatalf("Set() wrong stored secrets (-want +got) diff = %s", diff)
	}
}

func TestSetVoiceOnAnotherHostDropsTheKey(t *testing.T) {
	t.Parallel()
	const key = agentdir.SecretPrefix + agentdir.SecretVoiceKey
	tests := []struct {
		name       string
		settings   agentdir.Settings
		wantVoice  agentdir.VoiceConfig
		wantStored agentdir.Secrets
	}{
		{
			name:       "another host",
			settings:   agentdir.Settings{Voice: "http://localhost:8000/v1"},
			wantVoice:  agentdir.VoiceConfig{Service: "http://localhost:8000/v1", Name: "nova"},
			wantStored: agentdir.Secrets{},
		},
		{
			name:       "the same host",
			settings:   agentdir.Settings{Voice: "https://API.openai.com/v1"},
			wantVoice:  agentdir.VoiceConfig{Service: "https://API.openai.com/v1", Key: key, Name: "nova"},
			wantStored: agentdir.Secrets{agentdir.SecretVoiceKey: "sk-old"},
		},
		{
			name:       "another host with a key",
			settings:   agentdir.Settings{Voice: "https://speech.example.com/v1", VoiceKey: "sk-new"},
			wantVoice:  agentdir.VoiceConfig{Service: "https://speech.example.com/v1", Key: key, Name: "nova"},
			wantStored: agentdir.Secrets{agentdir.SecretVoiceKey: "sk-new"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := newAgent(t, &agentdir.Config{
				Name: "coder", Harness: "opencode",
				Voice: agentdir.VoiceConfig{Service: "openai", Key: key, Name: "nova"},
			}, agentdir.Secrets{agentdir.SecretVoiceKey: "sk-old"})
			if _, err := Set(t.Context(), d, controlapi.NewClient(d), tc.settings, false); err != nil {
				t.Fatalf("Set() error = %v", err)
			}
			cfg, err := d.ReadConfig()
			if err != nil {
				t.Fatal(err)
			}
			if diff := cmp.Diff(tc.wantVoice, cfg.Voice); diff != "" {
				t.Fatalf("Set() wrong voice (-want +got) diff = %s", diff)
			}
			stored, err := d.LoadSecrets()
			if err != nil {
				t.Fatal(err)
			}
			if diff := cmp.Diff(tc.wantStored, stored); diff != "" {
				t.Fatalf("Set() wrong stored secrets (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestSetVoiceGemini(t *testing.T) {
	t.Parallel()
	const key = agentdir.SecretPrefix + agentdir.SecretVoiceKey
	tests := []struct {
		name       string
		settings   agentdir.Settings
		wantErr    string
		wantVoice  agentdir.VoiceConfig
		wantStored agentdir.Secrets
	}{
		{
			name:     "without a key",
			settings: agentdir.Settings{Voice: "gemini"},
			wantErr:  "--voice gemini needs --voice-key, such as --voice-key env:GEMINI_API_KEY",
		},
		{
			name:       "with a key",
			settings:   agentdir.Settings{Voice: "gemini", VoiceKey: "gm-new", VoiceName: "Puck"},
			wantVoice:  agentdir.VoiceConfig{Service: "gemini", Key: key, Name: "Puck"},
			wantStored: agentdir.Secrets{agentdir.SecretVoiceKey: "gm-new"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := newAgent(t, &agentdir.Config{
				Name: "coder", Harness: "opencode",
				Voice: agentdir.VoiceConfig{Service: "openai", Key: key},
			}, agentdir.Secrets{agentdir.SecretVoiceKey: "sk-old"})
			_, err := Set(t.Context(), d, controlapi.NewClient(d), tc.settings, false)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("Set() error = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Set() error = %v", err)
			}
			cfg, err := d.ReadConfig()
			if err != nil {
				t.Fatal(err)
			}
			if diff := cmp.Diff(tc.wantVoice, cfg.Voice); diff != "" {
				t.Fatalf("Set() wrong voice (-want +got) diff = %s", diff)
			}
			stored, err := d.LoadSecrets()
			if err != nil {
				t.Fatal(err)
			}
			if diff := cmp.Diff(tc.wantStored, stored); diff != "" {
				t.Fatalf("Set() wrong stored secrets (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestSetNotesNormalizedName(t *testing.T) {
	t.Parallel()
	d := newAgent(t, &agentdir.Config{Name: "coder", Harness: "opencode"}, nil)

	got, err := Set(t.Context(), d, controlapi.NewClient(d), agentdir.Settings{Name: "日本語"}, false)
	if err != nil {
		t.Fatalf("Set() error = %v", err)
	}

	want := []string{agentdir.NameNote("日本語")}
	if diff := cmp.Diff(want, got.Warnings); diff != "" {
		t.Fatalf("Set().Warnings wrong result (-want +got) diff = %s", diff)
	}
}

func TestSetToolRuleOfAnMCPServerWarns(t *testing.T) {
	t.Parallel()
	const mcp = `{"mcpServers": {"gmail": {"command": "gmail-mcp"}}}`
	tests := []struct {
		name     string
		settings agentdir.Settings
		want     []string
	}{
		{
			name:     "deny the server",
			settings: agentdir.Settings{DenyTools: []string{"tool:gmail"}},
			want:     []string{"--deny-tools tool:gmail matches no tool of the MCP server gmail; tool:gmail.* matches all its tools"},
		},
		{
			name:     "allow the server",
			settings: agentdir.Settings{AllowTools: []string{"tool:gmail"}},
			want:     []string{"--allow-tools tool:gmail matches no tool of the MCP server gmail; tool:gmail.* matches all its tools"},
		},
		{name: "all its tools", settings: agentdir.Settings{DenyTools: []string{"tool:gmail.*"}}},
		{name: "a tool of the agent", settings: agentdir.Settings{DenyTools: []string{"tool:webfetch"}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := newAgent(t, &agentdir.Config{Name: "coder", Harness: "opencode", Model: "openrouter/a/b", Secrets: []string{"OPENROUTER_API_KEY"}}, agentdir.Secrets{"OPENROUTER_API_KEY": "k"})
			if err := os.WriteFile(filepath.Join(d.Root, ".mcp.json"), []byte(mcp), 0o600); err != nil {
				t.Fatal(err)
			}

			got, err := Set(t.Context(), d, controlapi.NewClient(d), tc.settings, false)
			if err != nil {
				t.Fatalf("Set() error = %v", err)
			}

			if diff := cmp.Diff(tc.want, got.Warnings); diff != "" {
				t.Fatalf("Set().Warnings wrong result (-want +got) diff = %s", diff)
			}
		})
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

func TestLeavingTheNetworkOfAProvider(t *testing.T) {
	t.Parallel()
	const slimURL = "http://127.0.0.1:46357?namespace=demo&peers=reviewer"
	tests := []struct {
		name    string
		cfg     agentdir.NetworkConfig
		leave   func(context.Context, agentdir.Dir, *controlapi.Client) (*Result, error)
		want    agentdir.NetworkConfig
		wantCh  []agentdir.Change
		wantErr bool
	}{
		{
			name: "set private none",
			cfg:  agentdir.NetworkConfig{Private: slimURL, Provider: "slim"},
			leave: func(ctx context.Context, d agentdir.Dir, c *controlapi.Client) (*Result, error) {
				return Set(ctx, d, c, agentdir.Settings{Private: "none"}, false)
			},
			wantCh: []agentdir.Change{{Setting: "private", Old: slimURL}, {Setting: "network-provider", Old: "slim"}},
		},
		{
			name: "set public none",
			cfg:  agentdir.NetworkConfig{Public: slimURL, Provider: "slim"},
			leave: func(ctx context.Context, d agentdir.Dir, c *controlapi.Client) (*Result, error) {
				return Set(ctx, d, c, agentdir.Settings{Public: "none"}, false)
			},
			wantCh: []agentdir.Change{{Setting: "public", Old: slimURL}, {Setting: "network-provider", Old: "slim"}},
		},
		{
			name: "unset private",
			cfg:  agentdir.NetworkConfig{Private: slimURL, Provider: "slim"},
			leave: func(ctx context.Context, d agentdir.Dir, c *controlapi.Client) (*Result, error) {
				return Unset(ctx, d, c, "private", nil, false)
			},
			wantCh: []agentdir.Change{{Setting: "private", Old: slimURL}, {Setting: "network-provider", Old: "slim"}},
		},
		{
			name: "unset public",
			cfg:  agentdir.NetworkConfig{Public: slimURL, Provider: "slim"},
			leave: func(ctx context.Context, d agentdir.Dir, c *controlapi.Client) (*Result, error) {
				return Unset(ctx, d, c, "public", nil, false)
			},
			wantCh: []agentdir.Change{{Setting: "public", Old: slimURL}, {Setting: "network-provider", Old: "slim"}},
		},
		{
			name: "set private tailscale",
			cfg:  agentdir.NetworkConfig{Private: slimURL, Provider: "slim"},
			leave: func(ctx context.Context, d agentdir.Dir, c *controlapi.Client) (*Result, error) {
				return Set(ctx, d, c, agentdir.Settings{Private: "tailscale"}, false)
			},
			want:   agentdir.NetworkConfig{Private: "tailscale"},
			wantCh: []agentdir.Change{{Setting: "private", Old: slimURL, New: "tailscale"}, {Setting: "network-provider", Old: "slim"}},
		},
		{
			name: "set private none with a provider",
			cfg:  agentdir.NetworkConfig{Private: slimURL, Provider: "slim"},
			leave: func(ctx context.Context, d agentdir.Dir, c *controlapi.Client) (*Result, error) {
				return Set(ctx, d, c, agentdir.Settings{Private: "none", NetworkProvider: "loopnet"}, false)
			},
			wantErr: true,
		},
		{
			name: "unset private keeps the provider of the public URL",
			cfg:  agentdir.NetworkConfig{Private: "tailscale", Public: slimURL, Provider: "slim"},
			leave: func(ctx context.Context, d agentdir.Dir, c *controlapi.Client) (*Result, error) {
				return Unset(ctx, d, c, "private", nil, false)
			},
			want:   agentdir.NetworkConfig{Public: slimURL, Provider: "slim"},
			wantCh: []agentdir.Change{{Setting: "private", Old: "tailscale"}},
		},
	}
	for _, running := range []bool{false, true} {
		for _, tc := range tests {
			t.Run(fmt.Sprintf("%s running=%v", tc.name, running), func(t *testing.T) {
				t.Parallel()
				d := newAgent(t, &agentdir.Config{Name: "coder", Harness: "opencode", Network: tc.cfg}, nil)
				if running {
					serveFake(t, d, &fakeServer{})
				}
				got, err := tc.leave(t.Context(), d, controlapi.NewClient(d))
				if tc.wantErr {
					if err == nil {
						t.Fatalf("leave error = nil, want an error")
					}
					return
				}
				if err != nil {
					t.Fatalf("leave error = %v, want nil", err)
				}
				if diff := cmp.Diff(tc.wantCh, got.Changes); diff != "" {
					t.Fatalf("leave wrong changes (-want +got) diff = %s", diff)
				}
				if got.Running != running {
					t.Fatalf("leave Running = %v, want %v", got.Running, running)
				}
				if diff := cmp.Diff(tc.want, loadConfig(t, d).Network); diff != "" {
					t.Fatalf("saved network wrong result (-want +got) diff = %s", diff)
				}
			})
		}
	}
}

func TestUnsetHintOfCombinedUnsetWorks(t *testing.T) {
	t.Parallel()
	d := newAgent(t, &agentdir.Config{Name: "coder", Harness: "opencode", Network: agentdir.NetworkConfig{
		Private: "http://127.0.0.1:46357?namespace=demo", Provider: "slim",
	}}, nil)
	_, err := Unset(t.Context(), d, controlapi.NewClient(d), "private", []string{"network-provider"}, false)
	if err == nil {
		t.Fatalf("Unset() error = nil, want the hint")
	}
	for _, setting := range []string{"private", "network-provider"} {
		if !strings.Contains(err.Error(), "`a2a-serve config unset "+setting+"`") {
			t.Fatalf("Unset() error = %v, want the hint to name unset %s", err, setting)
		}
		if _, err := Unset(t.Context(), d, controlapi.NewClient(d), setting, nil, false); err != nil {
			t.Fatalf("Unset(%s) error = %v, want nil", setting, err)
		}
	}
	if diff := cmp.Diff(agentdir.NetworkConfig{}, loadConfig(t, d).Network); diff != "" {
		t.Fatalf("saved network wrong result (-want +got) diff = %s", diff)
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
			wantCalls:   []string{"GET /status", "POST /messengers", "POST /config/reload", "GET /config", "GET /status"},
			wantApplied: []string{"telegram", "description"},
			wantRestart: []string{"sandbox"},
		},
		{
			name:        "server without reload",
			noReload:    true,
			wantCalls:   []string{"GET /status", "POST /messengers", "POST /config/reload", "GET /config", "GET /status"},
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
		base    *agentdir.Config
		s       agentdir.Settings
		want    *controlapi.Access
	}{
		{name: "other setting", s: agentdir.Settings{Description: "x"}},
		{
			name: "sandbox mode with a sandbox rule", base: &agentdir.Config{Name: "coder", Harness: "opencode", Secrets: []string{"KEY"}, Policy: agentdir.PolicyConfig{Sandbox: []agentdir.SandboxRule{{Match: []string{"pairing:ben"}, Storage: agentdir.StorageReadOnly}}}},
			s:    agentdir.Settings{Sandbox: "local"},
			want: &controlapi.Access{Owner: []string{"self"}, Member: []string{"private"}, Guest: []string{}, Summary: []string{"you: this machine", "nobody else"}},
		},
		{
			name: "stopped agent", s: agentdir.Settings{Private: "tailscale", PrivateAllowIncoming: agentdir.AccessPaired},
			want: &controlapi.Access{
				PrivateAllowIncoming: agentdir.AccessPaired, Owner: []string{"self"}, Member: []string{}, Guest: []string{},
				Summary: []string{"you: this machine and your tailnet devices", "nobody else", "the tailnet needs a pairing"},
			},
		},
		{
			name: "running agent", running: true, s: agentdir.Settings{AllowEmails: []string{"*@example.com"}},
			want: &controlapi.Access{
				Owner: []string{"self"}, Member: []string{"private", "email:*@example.com"}, Guest: []string{},
				Pairings: 3, OwnerPairings: 1, Others: []string{"emails matching *@example.com"},
				Summary: []string{"you: this machine", "emails matching *@example.com, 3 pairings (1 owner)"},
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			base := tc.base
			if base == nil {
				base = &agentdir.Config{Name: "coder", Harness: "opencode"}
			}
			d := newAgent(t, base, agentdir.Secrets{"KEY": "k"})
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

func TestSetPublicAllowIncomingWaitsForAPublicURLThatWaits(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		running     agentdir.NetworkConfig
		wantApplied []string
		wantRestart []string
	}{
		{name: "public URL waits", wantRestart: []string{"public-allow-incoming"}},
		{name: "public URL up", running: agentdir.NetworkConfig{Public: "https://relay.example.com"}, wantApplied: []string{"public-allow-incoming"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := newAgent(t, &agentdir.Config{Name: "coder", Harness: "opencode", Network: agentdir.NetworkConfig{Public: "https://relay.example.com"}}, nil)
			serveFake(t, d, &fakeServer{running: &agentdir.Config{Name: "coder", Harness: "opencode", Network: tc.running, Policy: agentdir.PolicyConfig{PublicAllowIncoming: agentdir.AccessAnyone}}})

			got, err := Set(t.Context(), d, controlapi.NewClient(d), agentdir.Settings{PublicAllowIncoming: agentdir.AccessAnyone}, false)
			if err != nil {
				t.Fatalf("Set() error = %v", err)
			}

			if diff := cmp.Diff(tc.wantApplied, got.Applied); diff != "" {
				t.Fatalf("Set().Applied wrong result (-want +got) diff = %s", diff)
			}
			if diff := cmp.Diff(tc.wantRestart, got.Restart); diff != "" {
				t.Fatalf("Set().Restart wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestSetBackToTheRunningSettingWaitsForNothing(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		running     *agentdir.Config
		noConfig    bool
		wantRestart []string
	}{
		{name: "running setting", running: &agentdir.Config{Name: "coder", Harness: "opencode", Sandbox: "local"}},
		{name: "other running setting", running: &agentdir.Config{Name: "coder", Harness: "opencode"}, wantRestart: []string{"sandbox"}},
		{name: "running config unknown", noConfig: true, wantRestart: []string{"sandbox"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := newAgent(t, &agentdir.Config{Name: "coder", Harness: "opencode", Sandbox: "docker"}, nil)
			serveFake(t, d, &fakeServer{running: tc.running, noConfig: tc.noConfig})

			got, err := Set(t.Context(), d, controlapi.NewClient(d), agentdir.Settings{Sandbox: "local"}, false)
			if err != nil {
				t.Fatalf("Set() error = %v", err)
			}

			if diff := cmp.Diff(tc.wantRestart, got.Restart); diff != "" {
				t.Fatalf("Set().Restart wrong result (-want +got) diff = %s", diff)
			}
		})
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
	setSandbox := func(d agentdir.Dir) (*Result, error) {
		return Set(t.Context(), d, controlapi.NewClient(d), agentdir.Settings{Sandbox: "local"}, false)
	}
	tests := []struct {
		name         string
		file         string
		apply        func(d agentdir.Dir) (*Result, error)
		wantErr      string
		wantHint     string
		wantProblems []string
		wantDeny     []string
	}{
		{
			name: "unset repairs the file",
			file: file,
			apply: func(d agentdir.Dir) (*Result, error) {
				return Unset(t.Context(), d, controlapi.NewClient(d), "deny-tools", []string{"exec"}, false)
			},
		},
		{
			name: "set names the entry in the file",
			file: file,
			apply: func(d agentdir.Dir) (*Result, error) {
				return Set(t.Context(), d, controlapi.NewClient(d), agentdir.Settings{DenyTools: []string{"execute"}}, false)
			},
			wantProblems: []string{`config.yaml: approvals.deny has "exec": want ACP tool kinds (read, edit, delete, move, search, execute, think, fetch, switch_mode, other) or a glob such as *, tool:<server>.<tool> or command:<command>: fix it with ` + "`a2a-serve config unset deny-tools exec`"},
			wantDeny:     []string{"exec", "execute"},
		},
		{
			name: "set names the flag",
			file: "name: coder\nagent: opencode\n",
			apply: func(d agentdir.Dir) (*Result, error) {
				return Set(t.Context(), d, controlapi.NewClient(d), agentdir.Settings{DenyTools: []string{"exec"}}, false)
			},
			wantErr: `invalid --deny-tools "exec"`,
		},
		{
			name:     "a file that doesn't parse",
			file:     "name: coder\nsandbox: [\n",
			apply:    setSandbox,
			wantErr:  "config.yaml: yaml: line 2",
			wantHint: "fix it with `a2a-serve config edit`",
		},
		{
			name:         "a card skill without an id",
			file:         "name: coder\nagent: opencode\ncard:\n    skills:\n        - name: Review\n",
			apply:        setSandbox,
			wantProblems: []string{`config.yaml: card.skills has a skill named "Review": want a non-empty id`},
		},
		{
			name:         "a problem without a key in the file",
			file:         "name: coder\nagent: opencode\nbackup:\n    to: /backups\n",
			apply:        setSandbox,
			wantProblems: []string{"config.yaml: --backup needs --backup-key: snapshots are always encrypted"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := newAgent(t, &agentdir.Config{Name: "coder", Harness: "opencode"}, nil)
			if err := os.WriteFile(d.ConfigPath(), []byte(tc.file), 0o644); err != nil {
				t.Fatal(err)
			}

			res, err := tc.apply(d)

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
			if diff := cmp.Diff(tc.wantProblems, res.Problems); diff != "" {
				t.Fatalf("apply() problems wrong result (-want +got) diff = %s", diff)
			}
			if diff := cmp.Diff(tc.wantDeny, loadConfig(t, d).Approvals.Deny); diff != "" {
				t.Fatalf("saved deny-tools wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestApplySavesChangesThatAddNoProblem(t *testing.T) {
	t.Parallel()
	set := func(s agentdir.Settings) func(agentdir.Dir) (*Result, error) {
		return func(d agentdir.Dir) (*Result, error) { return Set(t.Context(), d, controlapi.NewClient(d), s, false) }
	}
	unset := func(setting string) func(agentdir.Dir) (*Result, error) {
		return func(d agentdir.Dir) (*Result, error) {
			return Unset(t.Context(), d, controlapi.NewClient(d), setting, nil, false)
		}
	}
	const (
		clone      = "name: coder\nagent: opencode\nsecrets:\n    - API_A\n    - API_B\n"
		cloneMount = "name: coder\nagent: opencode\nmounts:\n    - /nonexistent-a2a:/docs\nsecrets:\n    - API_A\n"
		lan        = "name: coder\nagent: opencode\nnetwork:\n    private: lan\nmessengers:\n    telegram:\n        token: secret:TELEGRAM_BOT_TOKEN\n"
	)
	missingB := "config.yaml: secret API_B is not stored: run `a2a-serve config set --secret API_B` with the variable set"
	tests := []struct {
		name  string
		file  string
		steps []func(agentdir.Dir) (*Result, error)
		// wantProblems are the problems each step reports, or nil for a
		// step that is refused.
		wantProblems [][]string
		wantSecrets  agentdir.Secrets
	}{
		{
			name: "two missing secrets fixed one at a time",
			file: clone,
			steps: []func(agentdir.Dir) (*Result, error){
				set(agentdir.Settings{Secrets: []string{"API_A"}, SecretValues: agentdir.Secrets{"API_A": "a"}}),
				set(agentdir.Settings{Secrets: []string{"API_B"}, SecretValues: agentdir.Secrets{"API_B": "b"}}),
			},
			wantProblems: [][]string{{missingB}, {}},
			wantSecrets:  agentdir.Secrets{"API_A": "a", "API_B": "b"},
		},
		{
			name: "missing mount source and missing secret",
			file: cloneMount,
			steps: []func(agentdir.Dir) (*Result, error){
				set(agentdir.Settings{Secrets: []string{"API_A"}, SecretValues: agentdir.Secrets{"API_A": "a"}}),
			},
			wantProblems: [][]string{{"config.yaml: --mount /nonexistent-a2a:/docs: the source does not exist"}},
			wantSecrets:  agentdir.Secrets{"API_A": "a"},
		},
		{
			name: "a change that adds a problem is refused",
			file: clone,
			steps: []func(agentdir.Dir) (*Result, error){
				set(agentdir.Settings{Secrets: []string{"API_A"}, SecretValues: agentdir.Secrets{"API_A": "a"}, Mounts: []string{"/nonexistent-a2a"}}),
			},
			wantProblems: [][]string{nil},
			wantSecrets:  agentdir.Secrets{},
		},
		{
			name: "private lan with a messenger whose secret is missing",
			file: lan,
			steps: []func(agentdir.Dir) (*Result, error){
				unset("private"),
				func(d agentdir.Dir) (*Result, error) {
					m, closer, err := Open(t.Context(), d, controlapi.NewClient(d))
					if err != nil {
						return nil, err
					}
					_, err = m.DeleteMessenger(t.Context(), agentdir.MessengerTelegram)
					return &Result{}, errors.Join(err, closer.Close())
				},
			},
			wantProblems: [][]string{{"config.yaml: telegram token (a2a-serve messenger add telegram --token): secret TELEGRAM_BOT_TOKEN is not set; pass it again with the flag that set it"}, {}},
			wantSecrets:  agentdir.Secrets{},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := newAgent(t, &agentdir.Config{Name: "coder", Harness: "opencode"}, nil)
			if err := os.WriteFile(d.ConfigPath(), []byte(tc.file), 0o644); err != nil {
				t.Fatal(err)
			}
			for i, step := range tc.steps {
				before, err := os.ReadFile(d.ConfigPath())
				if err != nil {
					t.Fatal(err)
				}
				res, err := step(d)
				if tc.wantProblems[i] == nil {
					var se *agentdir.SettingsError
					if !errors.As(err, &se) {
						t.Fatalf("step %d error = %v, want a SettingsError", i, err)
					}
					if after, rerr := os.ReadFile(d.ConfigPath()); rerr != nil || string(after) != string(before) {
						t.Fatalf("step %d changed config.yaml to %q, %v, want it unchanged", i, after, rerr)
					}
					continue
				}
				if err != nil {
					t.Fatalf("step %d error = %v, want nil", i, err)
				}
				if diff := cmp.Diff(tc.wantProblems[i], res.Problems, cmpopts.EquateEmpty()); diff != "" {
					t.Fatalf("step %d problems wrong result (-want +got) diff = %s", i, diff)
				}
			}
			secrets, err := d.LoadSecrets()
			if err != nil {
				t.Fatalf("LoadSecrets() error = %v", err)
			}
			if diff := cmp.Diff(tc.wantSecrets, secrets); diff != "" {
				t.Fatalf("stored secrets wrong result (-want +got) diff = %s", diff)
			}
			if last := tc.wantProblems[len(tc.wantProblems)-1]; last != nil && len(last) == 0 {
				if _, err := d.LoadConfig(); err != nil {
					t.Fatalf("LoadConfig() error = %v, want a valid config", err)
				}
			}
		})
	}
}

func TestSetMovingASecretKeepsItStored(t *testing.T) {
	t.Parallel()
	d := newAgent(t, &agentdir.Config{
		Name: "coder", Harness: "opencode",
		Voice: agentdir.VoiceConfig{Service: "openai", Key: "secret:" + agentdir.SecretVoiceKey},
	}, agentdir.Secrets{agentdir.SecretVoiceKey: "sk-voice"})

	_, err := Set(t.Context(), d, controlapi.NewClient(d), agentdir.Settings{
		Secrets:       []string{"OPENAI_API_KEY"},
		SecretSources: map[string]string{"OPENAI_API_KEY": agentdir.SecretVoiceKey},
		VoiceKey:      "secret:OPENAI_API_KEY",
	}, false)

	if err != nil {
		t.Fatalf("Set() error = %v", err)
	}
	if got, want := loadConfig(t, d).Voice.Key, "secret:OPENAI_API_KEY"; got != want {
		t.Fatalf("saved voice key = %q, want %q", got, want)
	}
	secrets, err := d.LoadSecrets()
	if err != nil {
		t.Fatalf("LoadSecrets() error = %v", err)
	}
	if diff := cmp.Diff(agentdir.Secrets{"OPENAI_API_KEY": "sk-voice"}, secrets); diff != "" {
		t.Fatalf("stored secrets wrong result (-want +got) diff = %s", diff)
	}
}

func TestUnsetGoogleApp(t *testing.T) {
	t.Parallel()
	for _, setting := range []string{"google-client-id", "google-client-secret"} {
		t.Run(setting, func(t *testing.T) {
			t.Parallel()
			d := newAgent(t, &agentdir.Config{
				Name: "coder", Harness: "opencode",
				Policy: agentdir.PolicyConfig{AllowEmails: []string{"anna@example.com"}},
				Auth:   agentdir.AuthConfig{GoogleClientID: "id.apps.googleusercontent.com", GoogleClientSecret: "secret:" + agentdir.SecretGoogleClient},
			}, agentdir.Secrets{agentdir.SecretGoogleClient: "gsecret"})

			got, err := Unset(t.Context(), d, controlapi.NewClient(d), setting, nil, false)

			if err != nil {
				t.Fatalf("Unset(%s) error = %v", setting, err)
			}
			if diff := cmp.Diff([]SecretChange{{Name: agentdir.SecretGoogleClient, Action: SecretDeleted}}, got.Secrets); diff != "" {
				t.Fatalf("Unset(%s) wrong secrets (-want +got) diff = %s", setting, diff)
			}
			if diff := cmp.Diff(agentdir.AuthConfig{}, loadConfig(t, d).Auth); diff != "" {
				t.Fatalf("saved auth wrong result (-want +got) diff = %s", diff)
			}
			secrets, err := d.LoadSecrets()
			if err != nil {
				t.Fatalf("LoadSecrets() error = %v", err)
			}
			if diff := cmp.Diff(agentdir.Secrets{}, secrets); diff != "" {
				t.Fatalf("stored secrets wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestUnsetLastAllowedEmailOfAGoogleAppNamesTheFix(t *testing.T) {
	t.Parallel()
	d := newAgent(t, &agentdir.Config{
		Name: "coder", Harness: "opencode",
		Policy: agentdir.PolicyConfig{AllowEmails: []string{"anna@example.com"}},
		Auth:   agentdir.AuthConfig{GoogleClientID: "id.apps.googleusercontent.com", GoogleClientSecret: "secret:" + agentdir.SecretGoogleClient},
	}, agentdir.Secrets{agentdir.SecretGoogleClient: "gsecret"})

	_, err := Unset(t.Context(), d, controlapi.NewClient(d), "allow-email", []string{"anna@example.com"}, false)

	const hint = "fix it with `a2a-serve config unset google-client-id`"
	if got := agenterr.HintText(err); got != hint {
		t.Fatalf("Unset(allow-email) error = %v, hint %q, want hint %q", err, got, hint)
	}
	if _, err := Unset(t.Context(), d, controlapi.NewClient(d), "google-client-id", nil, false); err != nil {
		t.Fatalf("Unset(google-client-id) error = %v", err)
	}
	if _, err := Unset(t.Context(), d, controlapi.NewClient(d), "allow-email", []string{"anna@example.com"}, false); err != nil {
		t.Fatalf("Unset(allow-email) after the hint error = %v", err)
	}
}

func TestLeavingANetworkDeletesItsToken(t *testing.T) {
	t.Parallel()
	const (
		relay = "https://relay.example.com"
		other = "https://other.example.com"
	)
	tokens := agentdir.Secrets{agentdir.SecretNetworkToken: "join", agentdir.SecretRelayToken: "old"}
	tests := []struct {
		name  string
		cfg   agentdir.NetworkConfig
		leave func(context.Context, agentdir.Dir, *controlapi.Client) (*Result, error)
		want  agentdir.Secrets
	}{
		{
			name: "unset private",
			cfg:  agentdir.NetworkConfig{Private: relay},
			leave: func(ctx context.Context, d agentdir.Dir, c *controlapi.Client) (*Result, error) {
				return Unset(ctx, d, c, "private", nil, false)
			},
			want: agentdir.Secrets{},
		},
		{
			name: "set private tailscale",
			cfg:  agentdir.NetworkConfig{Private: relay},
			leave: func(ctx context.Context, d agentdir.Dir, c *controlapi.Client) (*Result, error) {
				return Set(ctx, d, c, agentdir.Settings{Private: "tailscale"}, false)
			},
			want: agentdir.Secrets{},
		},
		{
			name: "set private to another URL",
			cfg:  agentdir.NetworkConfig{Private: relay},
			leave: func(ctx context.Context, d agentdir.Dir, c *controlapi.Client) (*Result, error) {
				return Set(ctx, d, c, agentdir.Settings{Private: other}, false)
			},
			want: tokens,
		},
		{
			name: "unset private keeps the public URL",
			cfg:  agentdir.NetworkConfig{Private: relay, Public: relay},
			leave: func(ctx context.Context, d agentdir.Dir, c *controlapi.Client) (*Result, error) {
				return Unset(ctx, d, c, "private", nil, false)
			},
			want: tokens,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := newAgent(t, &agentdir.Config{Name: "coder", Harness: "opencode", Network: tc.cfg}, tokens)

			if _, err := tc.leave(t.Context(), d, controlapi.NewClient(d)); err != nil {
				t.Fatalf("leave error = %v", err)
			}

			secrets, err := d.LoadSecrets()
			if err != nil {
				t.Fatalf("LoadSecrets() error = %v", err)
			}
			if diff := cmp.Diff(tc.want, secrets); diff != "" {
				t.Fatalf("stored secrets wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestSetDescriptionOnAProviderNetworkWaitsForRestart(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		network     agentdir.NetworkConfig
		wantApplied []string
		wantRestart []string
	}{
		{name: "provider network", network: agentdir.NetworkConfig{Private: "https://relay.example.com"}, wantRestart: []string{"description"}},
		{name: "tailscale", network: agentdir.NetworkConfig{Private: "tailscale"}, wantApplied: []string{"description"}},
		{name: "a config that doesn't validate", network: agentdir.NetworkConfig{Private: "lan"}, wantRestart: []string{"description"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := newAgent(t, &agentdir.Config{Name: "coder", Harness: "opencode", Network: tc.network}, nil)
			serveFake(t, d, &fakeServer{})

			got, err := Set(t.Context(), d, controlapi.NewClient(d), agentdir.Settings{Description: "reviews code"}, false)

			if err != nil {
				t.Fatalf("Set() error = %v", err)
			}
			if diff := cmp.Diff(tc.wantApplied, got.Applied); diff != "" {
				t.Fatalf("Set().Applied wrong result (-want +got) diff = %s", diff)
			}
			if diff := cmp.Diff(tc.wantRestart, got.Restart); diff != "" {
				t.Fatalf("Set().Restart wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}
