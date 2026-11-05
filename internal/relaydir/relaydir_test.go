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

package relaydir

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/agenterr"
)

func newDir(t *testing.T) Dir {
	t.Helper()
	d, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	return d
}

func TestFind(t *testing.T) {
	t.Parallel()
	d := newDir(t)
	if _, err := d.SaveSettings(Settings{URL: "https://relay.example.com"}); err != nil {
		t.Fatalf("SaveSettings() error = %v", err)
	}
	nested := filepath.Join(d.Root, "a", "b")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := Find(nested)
	if err != nil {
		t.Fatalf("Find() error = %v", err)
	}
	if got.Root != d.Root {
		t.Fatalf("Find() = %v, want %v", got.Root, d.Root)
	}
	if _, err := Find(t.TempDir()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Find() error = %v, want %v", err, ErrNotFound)
	}
}

func TestADirectoryHoldsAnAgentOrARelay(t *testing.T) {
	t.Parallel()
	relay := newDir(t)
	if _, err := relay.SaveSettings(Settings{URL: "https://relay.example.com"}); err != nil {
		t.Fatalf("SaveSettings() error = %v", err)
	}
	if err := (agentdir.Dir{Root: relay.Root}).Init(); !errors.Is(err, agentdir.ErrRelayDir) {
		t.Fatalf("agentdir.Dir.Init() in a relay directory error = %v, want %v", err, agentdir.ErrRelayDir)
	}

	agent := agentdir.Dir{Root: t.TempDir()}
	if err := agent.SaveConfig(&agentdir.Config{Name: "coder", Harness: "opencode"}); err != nil {
		t.Fatalf("SaveConfig() error = %v", err)
	}
	if _, err := (Dir{Root: agent.Root}).SaveSettings(Settings{URL: "https://relay.example.com"}); !errors.Is(err, ErrAgentDir) {
		t.Fatalf("SaveSettings() in an agent directory error = %v, want %v", err, ErrAgentDir)
	}
}

func TestStateIsClosedToOthers(t *testing.T) {
	t.Parallel()
	d := newDir(t)
	if err := d.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	fi, err := os.Stat(d.StateDir())
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if fi.Mode().Perm() != 0o700 {
		t.Fatalf("state directory mode = %v, want 0700", fi.Mode().Perm())
	}
}

func TestApplySettings(t *testing.T) {
	t.Setenv("TEST_RELAY_DSN", "postgres://relay@db/relay")
	tests := []struct {
		name        string
		saved       *Config
		settings    Settings
		want        *Config
		wantSecrets agentdir.Secrets
		wantErr     bool
	}{
		{
			name:        "first run",
			settings:    Settings{URL: "https://relay.example.com/", Listen: ":8080"},
			want:        &Config{URL: "https://relay.example.com", Listen: ":8080"},
			wantSecrets: agentdir.Secrets{},
		},
		{
			name:        "loopback http",
			settings:    Settings{URL: "http://127.0.0.1:8443"},
			want:        &Config{URL: "http://127.0.0.1:8443"},
			wantSecrets: agentdir.Secrets{},
		},
		{
			name:        "secrets from the environment and values",
			saved:       &Config{URL: "https://relay.example.com"},
			settings:    Settings{AdminToken: strings.Repeat("t", 32), Database: "env:TEST_RELAY_DSN"},
			want:        &Config{URL: "https://relay.example.com", AdminToken: "secret:RELAY_ADMIN_TOKEN", Database: "secret:TEST_RELAY_DSN"},
			wantSecrets: agentdir.Secrets{"RELAY_ADMIN_TOKEN": strings.Repeat("t", 32), "TEST_RELAY_DSN": "postgres://relay@db/relay"},
		},
		{
			name:        "none clears the database and the tunnel",
			saved:       &Config{Tunnel: "cloudflare", Database: "secret:RELAY_DATABASE"},
			settings:    Settings{Tunnel: "none", Database: "none", URL: "https://relay.example.com"},
			want:        &Config{URL: "https://relay.example.com"},
			wantSecrets: agentdir.Secrets{},
		},
		{
			name:        "a tunnel gives the URL",
			settings:    Settings{Tunnel: "cloudflare"},
			want:        &Config{Tunnel: "cloudflare"},
			wantSecrets: agentdir.Secrets{},
		},
		{name: "no URL", settings: Settings{Listen: ":8080"}, wantErr: true},
		{name: "plain http off loopback", settings: Settings{URL: "http://relay.example.com"}, wantErr: true},
		{name: "a path", settings: Settings{URL: "https://example.com/relay"}, wantErr: true},
		{name: "a tunnel and a URL", settings: Settings{URL: "https://relay.example.com", Tunnel: "cloudflare"}, wantErr: true},
		{name: "unknown tunnel", settings: Settings{Tunnel: "ngrok"}, wantErr: true},
		{name: "bad listen", settings: Settings{URL: "https://relay.example.com", Listen: "8080"}, wantErr: true},
		{name: "short admin token", settings: Settings{URL: "https://relay.example.com", AdminToken: "s3cret"}, wantErr: true},
		{name: "empty environment variable", settings: Settings{URL: "https://relay.example.com", AdminToken: "env:TEST_RELAY_UNSET"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := newDir(t)
			if tt.saved != nil {
				if err := d.Init(); err != nil {
					t.Fatalf("Init() error = %v", err)
				}
				if err := d.SaveConfig(tt.saved); err != nil {
					t.Fatalf("SaveConfig() error = %v", err)
				}
			}

			got, secrets, err := d.ApplySettings(tt.settings)

			if tt.wantErr {
				if !agenterr.IsRequest(err) {
					t.Fatalf("ApplySettings() error = %v, want an invalid request", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ApplySettings() error = %v", err)
			}
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Fatalf("ApplySettings() wrong config (-want +got) diff = %s", diff)
			}
			if diff := cmp.Diff(tt.wantSecrets, secrets); diff != "" {
				t.Fatalf("ApplySettings() wrong secrets (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestResolveCreatesTheAdminTokenOnce(t *testing.T) {
	t.Parallel()
	d := newDir(t)
	if _, err := d.SaveSettings(Settings{URL: "https://relay.example.com"}); err != nil {
		t.Fatalf("SaveSettings() error = %v", err)
	}

	first, err := d.Resolve()
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	second, err := d.Resolve()
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}

	if len(first.AdminToken) != 64 {
		t.Fatalf("Resolve() admin token = %q, want 32 random bytes in hex", first.AdminToken)
	}
	if second.AdminToken != first.AdminToken {
		t.Fatalf("Resolve() admin token changed between runs: %q, then %q", first.AdminToken, second.AdminToken)
	}
	conf, err := d.ReadConfig()
	if err != nil {
		t.Fatalf("ReadConfig() error = %v", err)
	}
	if conf.AdminToken != "secret:"+AdminTokenSecret {
		t.Fatalf("relay.yaml admin_token = %q, want a reference to the stored token", conf.AdminToken)
	}
}

func TestLockWhileRunning(t *testing.T) {
	t.Parallel()
	d := newDir(t)
	if err := d.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	release, err := d.Lock()
	if err != nil {
		t.Fatalf("Lock() error = %v", err)
	}
	if _, err := d.Lock(); !errors.Is(err, ErrRunning) {
		t.Fatalf("Lock() while locked error = %v, want %v", err, ErrRunning)
	}
	if err := release(); err != nil {
		t.Fatalf("release() error = %v", err)
	}
	again, err := d.Lock()
	if err != nil {
		t.Fatalf("Lock() after release error = %v", err)
	}
	if err := again(); err != nil {
		t.Fatalf("release() error = %v", err)
	}
}

func TestSaveSettingsLocksInState(t *testing.T) {
	t.Parallel()
	d := newDir(t)
	if _, err := d.SaveSettings(Settings{URL: "https://relay.example.com"}); err != nil {
		t.Fatalf("SaveSettings() error = %v", err)
	}
	if _, err := d.Resolve(); err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}

	entries, err := os.ReadDir(d.Path())
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range entries {
		got = append(got, e.Name())
	}
	if diff := cmp.Diff([]string{".gitignore", agentdir.RelayConfigFile, "state"}, got); diff != "" {
		t.Fatalf("SaveSettings() wrong .a2a entries (-want +got) diff = %s", diff)
	}
}
