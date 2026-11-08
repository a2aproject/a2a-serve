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
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/agentkey"
	"github.com/a2aproject/a2a-serve/internal/harness"
	"github.com/a2aproject/a2a-serve/internal/project"
)

// providerRecorder answers every model request with 200 and records the
// URL and authorization of each.
type providerRecorder struct {
	mu  sync.Mutex
	got []string
}

func (p *providerRecorder) RoundTrip(r *http.Request) (*http.Response, error) {
	p.mu.Lock()
	p.got = append(p.got, r.URL.String()+" "+r.Header.Get("Authorization"))
	p.mu.Unlock()
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("{}")), Request: r}, nil
}

func (p *providerRecorder) requests() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.got...)
}

func TestHarnessModelRoutesOverTCP(t *testing.T) {
	t.Parallel()
	providers := &providerRecorder{}
	ts := startServer(t, serverOpts{name: "assistant", modelTransport: providers})
	addNode(t, ts, "grok", 0)
	hl, err := listenHarnessTCP("127.0.0.1", "127.0.0.1")
	if err != nil {
		t.Fatalf("listenHarnessTCP() error = %v", err)
	}
	t.Cleanup(func() {
		if err := hl.close(context.Background()); err != nil {
			t.Errorf("harnessListeners.close() error = %v", err)
		}
	})
	shutdown := hl.serve(context.Background(), ts.srv.harnessRoutes(hl), make(chan error, 1))
	t.Cleanup(func() {
		if err := shutdown(context.Background()); err != nil {
			t.Errorf("shutting down the harness listener: %v", err)
		}
	})
	owner, anonymous, anna, revoked := hl.openKey(""), hl.openKey(agentkey.Anonymous), hl.openKey(agentkey.User("pair:anna")), hl.openKey(agentkey.User("pair:bob"))
	hl.grantModels("", map[string]string{"OPENAI_API_KEY": "sk-owner"})
	hl.grantModels(agentkey.Anonymous, map[string]string{"OPENAI_API_KEY": "sk-guest"})
	hl.grantModels(agentkey.User("pair:anna"), map[string]string{"GEMINI_API_KEY": "gem"})
	revoke := hl.grantModels(agentkey.User("pair:bob"), map[string]string{"OPENAI_API_KEY": "sk-bob"})
	revoke()
	ownerHandle := strings.TrimPrefix(turnURL(t, ts, "c1"), socketHost+"/")
	tests := []struct {
		name string
		key  string
		path string
		want int
	}{
		{name: "owner's model request", key: owner.key, path: "/_model/openai/v1/models", want: http.StatusOK},
		{name: "anonymous model request", key: anonymous.key, path: "/_model/openai/v1/models", want: http.StatusOK},
		{name: "anonymous agents request", key: anonymous.key, path: "/" + ownerHandle + "/grok/.well-known/agent-card.json", want: http.StatusNotFound},
		{name: "process without a model key", key: anna.key, path: "/_model/openai/v1/models", want: http.StatusNotFound},
		{name: "revoked grant", key: revoked.key, path: "/_model/openai/v1/models", want: http.StatusNotFound},
		{name: "unknown key", key: "wrong", path: "/_model/openai/v1/models", want: http.StatusUnauthorized},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := getURL(t, hl.url+tc.path, "Bearer "+tc.key)
			if got.Status != tc.want {
				t.Fatalf("GET %s status = %d (%s), want %d", tc.path, got.Status, got.Error, tc.want)
			}
		})
	}
	want := []string{"https://api.openai.com/v1/models Bearer sk-owner", "https://api.openai.com/v1/models Bearer sk-guest"}
	if diff := cmp.Diff(want, providers.requests()); diff != "" {
		t.Fatalf("requests to the provider wrong result (-want +got) diff = %s", diff)
	}
}

func TestModelSecrets(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		sandbox string
		secrets []string
		want    []string
	}{
		{name: "docker", secrets: []string{"GITHUB_TOKEN", "GEMINI_API_KEY", "OPENROUTER_API_KEY", "DEEPSEEK_API_KEY"}, want: []string{"GEMINI_API_KEY", "OPENROUTER_API_KEY"}},
		{name: "local", sandbox: harness.SandboxLocal, secrets: []string{"GEMINI_API_KEY"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := modelSecrets(&agentdir.Config{Sandbox: tc.sandbox, Secrets: tc.secrets})
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Fatalf("modelSecrets() wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestMCPModelKeys(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{})
	mcp := `{"mcpServers": {
  "search": {"command": "search-mcp", "env": {"KEY": "${GEMINI_API_KEY}", "TOKEN": "${GITHUB_TOKEN}"}},
  "llm": {"url": "https://llm.example/mcp?key=${OPENAI_API_KEY}"}
}}`
	if err := os.WriteFile(filepath.Join(ts.dir.Root, project.MCPFile), []byte(mcp), 0o600); err != nil {
		t.Fatal(err)
	}
	got := ts.srv.mcpModelKeys([]string{"GEMINI_API_KEY", "ANTHROPIC_API_KEY", "OPENAI_API_KEY"})
	slices.Sort(got)
	if diff := cmp.Diff([]string{"GEMINI_API_KEY", "OPENAI_API_KEY"}, got); diff != "" {
		t.Fatalf("mcpModelKeys() wrong result (-want +got) diff = %s", diff)
	}
}
