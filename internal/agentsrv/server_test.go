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
	"errors"
	"net"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/harness"
	"github.com/a2aproject/a2a-serve/internal/network"
)

// loginProvider is a private network whose login waits until it is
// interrupted, and then fails with an error that doesn't wrap the
// interruption, as a killed login process does.
type loginProvider struct {
	network.Provider
	started chan struct{}
}

func (p *loginProvider) Name() string { return "fake" }

func (p *loginProvider) ListenPrivate(ctx context.Context) (net.Listener, *url.URL, error) {
	close(p.started)
	<-ctx.Done()
	return nil, nil, errors.New("login: signal: killed")
}

func (p *loginProvider) Close() error { return nil }

func TestRunCanceledDuringStartup(t *testing.T) {
	t.Parallel()
	root, err := os.MkdirTemp("", "a2a")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(root); err != nil {
			t.Errorf("removing %s: %v", root, err)
		}
	})
	dir, err := agentdir.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &agentdir.Config{Name: "coder", Harness: "fake", Command: []string{fakeACP}, Sandbox: harness.SandboxLocal, Port: freePort(t)}
	if err := dir.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	private := &loginProvider{started: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)

	go func() {
		done <- Run(ctx, Options{Dir: dir, Config: cfg, Secrets: agentdir.Secrets{}, Private: private, Timings: testTimings})
	}()
	select {
	case <-private.started:
	case err := <-done:
		t.Fatalf("Run() error = %v before the network login", err)
	case <-time.After(30 * time.Second):
		t.Fatal("the server did not reach the network login")
	}
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run() error = %v, want nil", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the server did not stop")
	}
}
