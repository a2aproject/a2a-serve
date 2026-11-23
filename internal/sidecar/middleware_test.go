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

package sidecar_test

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/core/egress"
	"github.com/a2aproject/a2a-serve/internal/sidecar/serve"
	"github.com/a2aproject/a2a-serve/internal/testutil"
)

func TestEgressMiddlewareInDockerSandbox(t *testing.T) {
	t.Parallel()
	bin, image := dockerTestEnv(t)
	dir := testutil.ShortTempDir(t)

	var (
		mu   sync.Mutex
		seen []string
	)
	mwHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Method+" "+r.RequestURI+" host="+r.Host+" ctx="+r.Header.Get(egress.ProxyHeaderContext)+" task="+r.Header.Get(egress.ProxyHeaderTask))
		mu.Unlock()
		if _, err := io.WriteString(w, "mw "+r.RequestURI); err != nil {
			t.Errorf("writing middleware answer error = %v", err)
		}
	})
	mwSock := filepath.Join(dir, "mw.sock")
	unixLn, err := net.Listen("unix", mwSock)
	if err != nil {
		t.Fatalf("net.Listen(unix) error = %v", err)
	}
	tcpLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen(tcp) error = %v", err)
	}
	for _, ln := range []net.Listener{unixLn, tcpLn} {
		srv := &http.Server{Handler: mwHandler, ReadHeaderTimeout: 5 * time.Second}
		go func(l net.Listener) {
			if err := srv.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
				t.Errorf("serving middleware error = %v", err)
			}
		}(ln)
		t.Cleanup(func() {
			if err := srv.Close(); err != nil {
				t.Errorf("closing middleware error = %v", err)
			}
		})
	}

	sock := filepath.Join(dir, "a2a.sock")
	ready := startBinary(t, bin,
		"--sandbox", dockerSandbox(), "--image", image, "--command", "/usr/local/bin/fakeacp",
		"--workspace", "dir:"+t.TempDir(), "--listen", "unix://"+sock,
		"--egress", "allowlist",
		"--egress-middleware", "api.anthropic.com=unix://"+mwSock,
		"--egress-middleware", "api.openai.com=http://"+tcpLn.Addr().String()+"/openai",
	)
	if ready.Egress != serve.EgressPolicy || !strings.HasPrefix(ready.Sandbox, dockerSandbox()) {
		t.Fatalf("ready line = %+v, want egress %q in the sandbox %s", ready, serve.EgressPolicy, dockerSandbox())
	}
	c, err := a2aclient.NewFromEndpoints(context.Background(), []*a2a.AgentInterface{a2a.NewAgentInterface("http://a2a/", a2a.TransportProtocolJSONRPC)}, a2aclient.WithJSONRPCTransport(unixClient(sock)))
	if err != nil {
		t.Fatalf("a2aclient.NewFromEndpoints() error = %v", err)
	}

	anthropicTask := sendTask(t, c, "ctx-mw", "fetch https://api.anthropic.com/v1/models")
	openaiTask := sendTask(t, c, "ctx-mw", "fetch https://api.openai.com/v1/responses")
	anthropicKey := sendTask(t, c, "ctx-mw", "env ANTHROPIC_API_KEY")
	refused := sendTask(t, c, "ctx-mw", "fetch https://blocked.test/")

	if got, want := testutil.AllArtifactText(anthropicTask), "fetch 200 mw /v1/models"; got != want {
		t.Fatalf("fetch through unix middleware = %q, want %q", got, want)
	}
	if got, want := testutil.AllArtifactText(openaiTask), "fetch 200 mw /openai/v1/responses"; got != want {
		t.Fatalf("fetch through tcp middleware = %q, want %q", got, want)
	}
	if got := testutil.AllArtifactText(anthropicKey); !strings.Contains(got, "sk-ant-api03-") {
		t.Fatalf("env ANTHROPIC_API_KEY = %q, want a placeholder key", got)
	}
	if got := testutil.AllArtifactText(refused); !strings.HasPrefix(got, "fetch error:") {
		t.Fatalf("fetch of a refused host = %q, want an error", got)
	}

	mu.Lock()
	gotSeen := append([]string(nil), seen...)
	mu.Unlock()
	wantSeen := []string{
		"GET /v1/models host=api.anthropic.com ctx=ctx-mw task=" + string(anthropicTask.ID),
		"GET /openai/v1/responses host=api.openai.com ctx=ctx-mw task=" + string(openaiTask.ID),
	}
	if diff := cmp.Diff(wantSeen, gotSeen); diff != "" {
		t.Fatalf("middleware requests wrong result (-want +got) diff = %s", diff)
	}
}
