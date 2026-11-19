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

package agentrun

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/core/egress"
	"github.com/a2aproject/a2a-serve/core/sandbox"
)

// dialingRuntime creates sandboxes that open one connection through their
// egress endpoint to dest as they are created.
type dialingRuntime struct{ dest string }

func (dialingRuntime) Probe(context.Context) (sandbox.Capabilities, error) {
	return sandbox.Capabilities{}, nil
}

func (r dialingRuntime) Create(_ context.Context, spec sandbox.Spec) (sandbox.Sandbox, error) {
	guest, host := net.Pipe()
	go spec.Egress.ServeConn(host)
	defer func() {
		if err := guest.Close(); err != nil {
			slog.Debug("closing the test connection", "error", err)
		}
	}()
	if _, err := fmt.Fprintf(guest, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", r.dest, r.dest); err != nil {
		return nil, err
	}
	resp, err := http.ReadResponse(bufio.NewReader(guest), &http.Request{Method: http.MethodConnect})
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusForbidden {
		return nil, fmt.Errorf("CONNECT %s status = %d, want the router's refusal", r.dest, resp.StatusCode)
	}
	return nopSandbox{}, nil
}

type nopSandbox struct{}

func (nopSandbox) Start(context.Context) (io.ReadWriteCloser, error) {
	return nil, errors.New("not started")
}

func (nopSandbox) Close(context.Context) error { return nil }

func TestEgressFlowsCarryTheProcessKey(t *testing.T) {
	t.Parallel()
	flows := make(chan egress.Flow, 1)
	b, err := egress.New(egress.Config{Router: egress.RouterFunc(func(_ context.Context, f egress.Flow) (egress.Route, error) {
		flows <- f
		return egress.Refuse(http.StatusForbidden, "test"), nil
	}), Log: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatalf("egress.New() error = %v", err)
	}
	t.Cleanup(func() {
		if err := b.Close(); err != nil {
			t.Errorf("Boundary.Close() error = %v", err)
		}
	})
	env := Environment{Workspace: sharedDir(t.TempDir()), Name: "a2a-coder-anna", Home: t.TempDir(), Pool: "users"}
	r := newTestRunner(t, Config{
		Runtime: dialingRuntime{dest: "api.test:443"},
		Egress:  b,
		Callers: fakeCallers{envs: map[string]Environment{"user:anna": env}},
		Pools:   map[string]Pool{"users": {}},
	})
	p, err := r.prepare(context.Background(), io.Discard, io.Discard, "user:anna", env)
	if err != nil {
		t.Fatalf("Runner.prepare() error = %v", err)
	}
	t.Cleanup(func() {
		if err := p.stop(context.Background()); err != nil {
			t.Errorf("stopping the process error = %v", err)
		}
	})
	want := egress.Flow{Sandbox: "a2a-coder-anna", Key: "user:anna", Host: "api.test", Port: 443}
	if diff := cmp.Diff(want, <-flows); diff != "" {
		t.Fatalf("router got wrong flow (-want +got) diff = %s", diff)
	}
}
