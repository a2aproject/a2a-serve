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

//go:build agentmesh

package relay_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/network"
	"github.com/a2aproject/a2a-serve/internal/relay"
	"github.com/a2aproject/a2a-serve/internal/relay/control"
	"github.com/a2aproject/a2a-serve/internal/relaydir"
)

// agentmeshOne returns the pinned agentmesh-one: the one A2A_SERVE_AGENTMESH_ONE
// names, or the release, downloaded once into the user's cache.
func agentmeshOne(t *testing.T) string {
	t.Helper()
	cache, err := os.UserCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	f := relay.NewFetcher(filepath.Join(cache, "a2a-serve"), os.Getenv(relay.BinaryEnv), http.DefaultClient, runtime.GOOS, runtime.GOARCH, t.Logf)
	bin, err := f.Get(t.Context())
	if err != nil {
		t.Fatalf("Fetcher.Get() error = %v", err)
	}
	return bin
}

type logWriter struct{ t *testing.T }

func (w logWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	return port
}

type runningRelay struct {
	url    string
	client *control.Client
	stop   func() error
}

func startRelay(t *testing.T, d relaydir.Dir, bin string) *runningRelay {
	t.Helper()
	release, err := d.Lock()
	if err != nil {
		t.Fatalf("Lock() error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		err := relay.New(relay.Options{
			Dir: d, Binary: bin, A2AServe: "test", Out: logWriter{t}, MeshLog: io.Discard, Now: time.Now,
		}).Run(ctx)
		done <- errors.Join(err, release())
	}()
	r := &runningRelay{client: control.NewSocketClient(d.SocketPath(), 10*time.Second)}
	var once bool
	r.stop = func() error {
		if once {
			return nil
		}
		once = true
		cancel()
		return <-done
	}
	t.Cleanup(func() {
		if err := r.stop(); err != nil {
			t.Errorf("relay.Run() error = %v", err)
		}
	})
	deadline := time.Now().Add(time.Minute)
	for {
		st, err := r.client.Status(ctx)
		if err == nil {
			r.url = st.URL
			return r
		}
		select {
		case err := <-done:
			t.Fatalf("relay.Run() error = %v", err)
		case <-time.After(200 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			t.Fatalf("the relay didn't answer on its control socket: %v", err)
		}
	}
}

type joinedAgent struct {
	name       string
	provider   network.Provider
	privateURL string
	publicURL  string
}

func joinAgent(t *testing.T, r *runningRelay, name string) *joinedAgent {
	t.Helper()
	inv, err := r.client.Invite(t.Context(), control.InviteRequest{Name: name})
	if err != nil {
		t.Fatalf("Client.Invite() error = %v", err)
	}
	p, err := network.NewRelay(network.RelayConfig{
		AgentName: name, URL: r.url, StateDir: t.TempDir(), Token: inv.Token, Private: true, Public: true, Logf: t.Logf,
	})
	if err != nil {
		t.Fatalf("network.NewRelay() error = %v", err)
	}
	t.Cleanup(func() {
		if err := p.Close(); err != nil {
			t.Logf("closing %s: %v", name, err)
		}
	})
	a := &joinedAgent{name: name, provider: p}
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if _, err := fmt.Fprintf(w, `{"name":%q}`, name); err != nil {
			t.Errorf("writing: %v", err)
		}
	})
	serve := func(ln net.Listener, u *url.URL, err error) string {
		if err != nil {
			t.Fatalf("listening on the relay as %s: %v", name, err)
		}
		srv := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second}
		go func() {
			if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
				t.Logf("serving %s: %v", name, err)
			}
		}()
		t.Cleanup(func() {
			if err := srv.Close(); err != nil {
				t.Logf("closing: %v", err)
			}
		})
		return u.String()
	}
	a.privateURL = serve(p.ListenPrivate(t.Context()))
	a.publicURL = serve(p.ListenPublic(t.Context()))
	return a
}

func get(ctx context.Context, hc *http.Client, rawURL string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return "", err
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			return
		}
	}()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s: %s", resp.Status, body)
	}
	return string(body), nil
}

// eventually retries fn until it succeeds or a minute passes.
func eventually(t *testing.T, what string, fn func() error) {
	t.Helper()
	deadline := time.Now().Add(time.Minute)
	for {
		err := fn()
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: %v", what, err)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func agentNames(ms []control.Member) []string {
	var out []string
	for _, m := range ms {
		out = append(out, m.Agent)
	}
	slices.Sort(out)
	return out
}

func TestRelayRunsAgentMesh(t *testing.T) {
	bin := agentmeshOne(t)
	port := freePort(t)
	d, err := relaydir.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	relayURL := fmt.Sprintf("http://127.0.0.1:%d", port)
	if _, err := d.SaveSettings(relaydir.Settings{URL: relayURL, Listen: fmt.Sprintf("127.0.0.1:%d", port)}); err != nil {
		t.Fatalf("SaveSettings() error = %v", err)
	}
	r := startRelay(t, d, bin)

	coder := joinAgent(t, r, "coder")
	reviewer := joinAgent(t, r, "reviewer")

	eventually(t, "coder calls reviewer on the relay", func() error {
		got, err := get(t.Context(), coder.provider.HTTPClient(), reviewer.privateURL+"/hello")
		if err == nil && got != `{"name":"reviewer"}` {
			return fmt.Errorf("got %q", got)
		}
		return err
	})
	if !strings.HasPrefix(reviewer.publicURL, relayURL+"/ingress/") {
		t.Fatalf("reviewer's public URL = %s, want one under %s/ingress/", reviewer.publicURL, relayURL)
	}
	eventually(t, "a caller outside the relay calls reviewer's public URL", func() error {
		got, err := get(t.Context(), http.DefaultClient, reviewer.publicURL+"/hello")
		if err == nil && got != `{"name":"reviewer"}` {
			return fmt.Errorf("got %q", got)
		}
		return err
	})
	eventually(t, "members lists both agents", func() error {
		ms, err := r.client.Members(t.Context(), false)
		if err != nil {
			return err
		}
		if diff := cmp.Diff([]string{"coder", "reviewer"}, agentNames(ms)); diff != "" {
			return fmt.Errorf("Members() wrong agents (-want +got) diff = %s", diff)
		}
		return nil
	})

	res, err := r.client.Remove(t.Context(), control.RemoveRequest{Target: "reviewer"})
	if err != nil {
		t.Fatalf("Client.Remove() error = %v", err)
	}
	if len(res.Removed) != 1 || res.Removed[0].Agent != "reviewer" {
		t.Fatalf("Client.Remove() = %+v, want reviewer", res.Removed)
	}
	ms, err := r.client.Members(t.Context(), false)
	if err != nil {
		t.Fatalf("Client.Members() error = %v", err)
	}
	if diff := cmp.Diff([]string{"coder"}, agentNames(ms)); diff != "" {
		t.Fatalf("Client.Members() after removal wrong agents (-want +got) diff = %s", diff)
	}

	if err := r.stop(); err != nil {
		t.Fatalf("relay.Run() error = %v", err)
	}
	r = startRelay(t, d, bin)
	ms, err = r.client.Members(t.Context(), true)
	if err != nil {
		t.Fatalf("Client.Members() after a restart error = %v", err)
	}
	if len(ms) != 2 {
		t.Fatalf("Client.Members() after a restart = %+v, want coder and the removed reviewer", ms)
	}
}
