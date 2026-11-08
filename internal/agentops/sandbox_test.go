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

package agentops

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/controlapi"
)

func newSandboxService(t *testing.T) (*Service, *agentdir.Runtime, agentdir.Dir) {
	t.Helper()
	d := agentdir.Dir{Root: t.TempDir()}
	if err := os.MkdirAll(d.StateDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := &agentdir.Config{Name: "coder", Harness: "opencode", Secrets: []string{"KEY", "GUEST"}}
	if err := d.SaveConfig(cfg); err != nil {
		t.Fatalf("SaveConfig() error = %v", err)
	}
	rt := agentdir.NewRuntime(d, cfg, agentdir.Secrets{"KEY": "k", "GUEST": "g"})
	s, _ := newServiceWith(t, rt, Hooks{})
	return s, rt, d
}

func TestPairSetsTheSandboxRule(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, rt, d := newSandboxService(t)

	if _, err := s.Pair(ctx, controlapi.PairCodeRequest{Name: "ben", Sandbox: &controlapi.SandboxChange{Storage: agentdir.StorageOwn, Secrets: new([]string{"KEY=secret:GUEST"})}}); err != nil {
		t.Fatalf("Pair() error = %v", err)
	}
	got, err := s.UpdatePairing(ctx, "ben", controlapi.PairingUpdate{Sandbox: &controlapi.SandboxChange{Secrets: new([]string{"KEY", "GUEST"})}})
	if err != nil {
		t.Fatalf("UpdatePairing() error = %v", err)
	}

	want := &controlapi.PairingSandbox{Storage: agentdir.StorageOwn, Secrets: []string{"KEY", "GUEST"}, Summary: "own copy of the workspace, secrets KEY, GUEST"}
	if diff := cmp.Diff(want, got.Pairing.Sandbox); diff != "" {
		t.Fatalf("UpdatePairing() wrong sandbox (-want +got) diff = %s", diff)
	}
	saved, err := d.LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	wantRules := []agentdir.SandboxRule{{Match: []string{"pairing:ben"}, Storage: agentdir.StorageOwn, Secrets: []string{"KEY", "GUEST"}}}
	if diff := cmp.Diff(wantRules, saved.Policy.Sandbox); diff != "" {
		t.Fatalf("saved rules wrong result (-want +got) diff = %s", diff)
	}

	if _, err := s.Revoke(ctx, "ben"); err != nil {
		t.Fatalf("Revoke() error = %v", err)
	}
	if rules := rt.Config().Policy.Sandbox; len(rules) != 0 {
		t.Fatalf("rules after Revoke() = %v, want none", rules)
	}
}

func TestPairSandboxRefusals(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, rt, _ := newSandboxService(t)
	if _, err := s.Pair(ctx, controlapi.PairCodeRequest{Name: "anna", Owner: true}); err != nil {
		t.Fatalf("Pair() error = %v", err)
	}
	tests := []struct {
		name    string
		run     func() error
		wantErr string
	}{
		{name: "new owner", wantErr: "owners share your agent container", run: func() error {
			_, err := s.Pair(ctx, controlapi.PairCodeRequest{Name: "cy", Owner: true, Sandbox: &controlapi.SandboxChange{Storage: agentdir.StorageOwn}})
			return err
		}},
		{name: "existing owner", wantErr: "owners share your agent container", run: func() error {
			_, err := s.UpdatePairing(ctx, "anna", controlapi.PairingUpdate{Sandbox: &controlapi.SandboxChange{Storage: agentdir.StorageOwn}})
			return err
		}},
		{name: "unknown secret", wantErr: "secret OTHER is not one of the agent's secrets", run: func() error {
			_, err := s.Pair(ctx, controlapi.PairCodeRequest{Name: "dan", Sandbox: &controlapi.SandboxChange{Secrets: new([]string{"OTHER"})}})
			return err
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.run(); err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want %q", err, tt.wantErr)
			}
		})
	}
	if _, err := s.Pairing(ctx, "dan"); err == nil {
		t.Fatalf("Pairing(dan) error = nil, want the refused invite to create no pairing")
	}
	if rules := rt.Config().Policy.Sandbox; len(rules) != 0 {
		t.Fatalf("rules = %v, want none", rules)
	}
}
