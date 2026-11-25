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

package agentprofile

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/egressrules"
	"github.com/a2aproject/a2a-serve/internal/projectfiles"
	"github.com/a2aproject/a2a-serve/internal/sandboxrt"
	"github.com/a2aproject/a2a-serve/internal/testutil"
)

// recordingBackend records the specs it prepares, and fails those with an
// egress boundary with noBoundary when it is set.
type recordingBackend struct {
	noBoundary error

	mu    sync.Mutex
	specs []sandboxrt.Spec
}

func (b *recordingBackend) Probe(context.Context) (sandboxrt.Capabilities, error) {
	return sandboxrt.Capabilities{}, nil
}

func (b *recordingBackend) Prepare(_ context.Context, spec sandboxrt.Spec) (sandboxrt.Sandbox, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.specs = append(b.specs, spec)
	if spec.Network.Boundary != nil && b.noBoundary != nil {
		return nil, b.noBoundary
	}
	return recordedSandbox{info: sandboxrt.Info{Summary: "bubblewrap", Boundary: spec.Network.Boundary != nil}}, nil
}

func (b *recordingBackend) prepared() []sandboxrt.Spec {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.specs
}

type recordedSandbox struct{ info sandboxrt.Info }

func (recordedSandbox) Start(context.Context) (io.ReadWriteCloser, error) {
	return nil, errors.New("not started in tests")
}

func (recordedSandbox) Close(context.Context) error { return nil }

func (s recordedSandbox) Info() sandboxrt.Info { return s.info }

func openEgress(sandboxrt.HostGatewayFunc, bool) (sandboxrt.EgressEndpoint, error) {
	return sandboxrt.EgressEndpoint{}, errors.New("the fake backend doesn't open the boundary")
}

func TestPrepareBwrapSandbox(t *testing.T) {
	t.Parallel()
	ws, docs, home, state, sockDir := t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()
	nested := filepath.Join(ws, "sub", projectfiles.AgentDir)
	if err := os.MkdirAll(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	policyDir := filepath.Join(state, "policy")
	if err := WritePolicy(claude{}, policyDir, Policy{LockHome: true}); err != nil {
		t.Fatalf("WritePolicy() error = %v", err)
	}
	mcpMask, configMask := filepath.Join(state, "mcp-mask.json"), filepath.Join(state, "config-mask.json")
	sock := filepath.Join(sockDir, "a2a.sock")
	b := &recordingBackend{}
	p, err := Prepare(context.Background(), Spec{
		Profile:    claude{},
		Sandbox:    SandboxBwrap,
		Backend:    b,
		Base:       sandboxrt.Spec{Name: "a2a-test", Image: "ignored:1"},
		Mounts:     []Mount{{Src: ws, Dst: ws}, {Src: docs, Dst: "/docs", ReadOnly: true}},
		Home:       home,
		Hidden:     []string{filepath.Join(ws, projectfiles.AgentDir)},
		MCPMask:    mcpMask,
		ConfigMask: configMask,
		PolicyDir:  policyDir,
		LockHome:   true,
		Server:     sandboxrt.Socket{Path: sock},
		EgressMode: egressrules.ModeInternet,
		OpenEgress: openEgress,
	})
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	specs := b.prepared()
	if len(specs) != 1 {
		t.Fatalf("Prepare() prepared %d sandboxes, want 1 behind the boundary", len(specs))
	}
	got := specs[0]
	if got.Image != "" || got.Build != nil {
		t.Fatalf("Prepare() image = %q, build %v, want none for bubblewrap", got.Image, got.Build)
	}
	if got.Network.Boundary == nil {
		t.Fatalf("Prepare() network = %+v, want the egress boundary", got.Network)
	}
	if got.Home != home {
		t.Fatalf("Prepare() home = %q, want %q", got.Home, home)
	}
	if diff := cmp.Diff([]sandboxrt.Socket{{Path: sock, Env: EnvUnixSocket}}, got.Expose); diff != "" {
		t.Fatalf("Prepare() exposed sockets wrong result (-want +got) diff = %s", diff)
	}
	wantMounts := []sandboxrt.Mount{
		{Src: ws, Dst: ws},
		{Src: docs, Dst: "/docs", ReadOnly: true},
		{Src: filepath.Join(ws, ".claude"), Dst: filepath.Join(ws, ".claude")},
		{Src: policyDir, Dst: claudePolicyDir, ReadOnly: true},
	}
	var mounts []sandboxrt.Mount
	for _, m := range got.Mounts {
		if !strings.HasPrefix(m.Dst, ContainerHome) {
			mounts = append(mounts, m)
		}
	}
	if diff := cmp.Diff(wantMounts, mounts); diff != "" {
		t.Fatalf("Prepare() mounts wrong result (-want +got) diff = %s", diff)
	}
	if !slices.Contains(got.Mounts, sandboxrt.Mount{Src: filepath.Join(home, ".claude", "settings.json"), Dst: filepath.Join(ContainerHome, ".claude", "settings.json"), ReadOnly: true}) {
		t.Fatalf("Prepare() mounts = %+v, want the home's settings read-only", got.Mounts)
	}
	wantMasks := []sandboxrt.Mask{
		{Dst: filepath.Join(ws, projectfiles.AgentDir)},
		{Dst: nested},
		{Src: mcpMask, Dst: filepath.Join(ws, projectfiles.MCPFile)},
		{Src: configMask, Dst: filepath.Join(ws, ".claude", "settings.json")},
		{Src: configMask, Dst: filepath.Join(ws, ".claude", "settings.local.json")},
	}
	if diff := cmp.Diff(wantMasks, got.Masks); diff != "" {
		t.Fatalf("Prepare() masks wrong result (-want +got) diff = %s", diff)
	}
	if !p.Egress || !strings.Contains(p.Summary, "egress internet through the server") {
		t.Fatalf("Prepare() = egress %v, summary %q, want the egress through the server", p.Egress, p.Summary)
	}
}

func TestPrepareBwrapEgress(t *testing.T) {
	t.Parallel()
	noBoundary := &sandboxrt.EgressSetupError{Reason: "bubblewrap can't set up the sandbox's network here: no tun"}
	tests := []struct {
		name       string
		mode       string
		inject     []InjectRule
		noBoundary error
		// wantNetwork is set when the sandbox gets the boundary.
		wantNetwork bool
		wantSummary string
		wantErr     string
	}{
		{name: "internet", mode: egressrules.ModeInternet, wantNetwork: true, wantSummary: "bubblewrap; egress internet through the server"},
		{name: "default mode", wantNetwork: true, wantSummary: "bubblewrap; egress internet through the server"},
		{name: "allowlist", mode: egressrules.ModeAllowlist, wantNetwork: true, wantSummary: "bubblewrap; egress allowlist through the server"},
		{name: "no boundary", mode: egressrules.ModeInternet, noBoundary: noBoundary, wantErr: "the egress settings can't apply: bubblewrap can't set up the sandbox's network here: no tun"},
		{name: "unrestricted", mode: egressrules.ModeUnrestricted, wantSummary: "bubblewrap; egress unrestricted, with this machine's network"},
		{name: "unrestricted with inject rules", mode: egressrules.ModeUnrestricted, inject: []InjectRule{{Host: "api.github.com"}}, wantErr: "has no egress boundary"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ws := t.TempDir()
			b := &recordingBackend{noBoundary: tc.noBoundary}
			p, err := Prepare(context.Background(), Spec{
				Profile:    opencode{},
				Sandbox:    SandboxBwrap,
				Backend:    b,
				Mounts:     []Mount{{Src: ws, Dst: ws}},
				Home:       t.TempDir(),
				EgressMode: tc.mode,
				Inject:     tc.inject,
				OpenEgress: openEgress,
			})
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("Prepare() error = %v, want %q", err, tc.wantErr)
				}
				for _, s := range b.prepared() {
					if s.Network.Boundary == nil && tc.mode != egressrules.ModeUnrestricted {
						t.Fatalf("Prepare() prepared a sandbox without the boundary, which would have this machine's network")
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("Prepare() error = %v", err)
			}
			specs := b.prepared()
			if len(specs) != 1 {
				t.Fatalf("Prepare() prepared %d sandboxes, want 1", len(specs))
			}
			if got := specs[0].Network.Boundary != nil; got != tc.wantNetwork {
				t.Fatalf("Prepare() boundary = %v, want %v", got, tc.wantNetwork)
			}
			if p.Summary != tc.wantSummary {
				t.Fatalf("Prepare() summary = %q, want %q", p.Summary, tc.wantSummary)
			}
			if p.NoBoundary() != "" {
				t.Fatalf("Prepare().NoBoundary() = %q, want none", p.NoBoundary())
			}
		})
	}
}

func TestPrepareBwrapRunsTheAgentWithTheMasks(t *testing.T) {
	t.Parallel()
	if runtime.GOOS != "linux" {
		t.Skip("bubblewrap runs only on Linux")
	}
	if err := sandboxrt.CheckBubblewrap(context.Background()); err != nil {
		t.Skipf("bubblewrap isn't available: %v", err)
	}
	ws, home, state := t.TempDir(), t.TempDir(), t.TempDir()
	sockDir := testutil.ShortTempDir(t)
	writeTestFile(t, filepath.Join(ws, projectfiles.AgentDir, "secret"), "token")
	writeTestFile(t, filepath.Join(ws, projectfiles.MCPFile), `{"mcpServers": {"github": {"command": "gh-mcp"}}}`)
	writeTestFile(t, filepath.Join(ws, ".claude", "settings.json"), `{"permissions": {"allow": ["Bash"]}}`)
	policyDir := filepath.Join(state, "policy")
	if err := WritePolicy(claude{}, policyDir, Policy{}); err != nil {
		t.Fatalf("WritePolicy() error = %v", err)
	}
	sock := filepath.Join(sockDir, "a2a.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("net.Listen() error = %v", err)
	}
	t.Cleanup(func() {
		if err := ln.Close(); err != nil {
			t.Errorf("closing the listener error = %v", err)
		}
	})
	script := filepath.Join(t.TempDir(), "show-sandbox")
	testutil.WriteExecutable(t, script, []byte(`#!/bin/sh
echo "home=$HOME"
echo "a2a=$(ls -A "$PWD/.a2a")"
echo "mcp=$(cat "$PWD/.mcp.json")"
echo "settings=$(cat "$PWD/.claude/settings.json")"
test -f /etc/claude-code/managed-settings.json && echo policy=yes
test -S "$A2ACLI_UNIX_SOCKET" && echo socket=yes
`))
	profile, err := Lookup("claude", []string{script})
	if err != nil {
		t.Fatalf("Lookup() error = %v", err)
	}
	p, err := Prepare(context.Background(), Spec{
		Profile:    profile,
		Sandbox:    SandboxBwrap,
		Backend:    sandboxrt.NewBubblewrap(sandboxrt.BubblewrapConfig{Dir: filepath.Join(state, "bwrap")}),
		Mounts:     []Mount{{Src: ws, Dst: ws}},
		Home:       home,
		Hidden:     []string{filepath.Join(ws, projectfiles.AgentDir)},
		MCPMask:    filepath.Join(state, "mcp-mask.json"),
		ConfigMask: filepath.Join(state, "config-mask.json"),
		PolicyDir:  policyDir,
		Server:     sandboxrt.Socket{Path: sock},
		EgressMode: egressrules.ModeUnrestricted,
	})
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	t.Cleanup(func() {
		if err := p.Close(context.Background()); err != nil {
			t.Errorf("Prepared.Close() error = %v", err)
		}
	})
	want := []string{
		"home=" + ContainerHome,
		"a2a=",
		"mcp=" + strings.TrimSuffix(emptyMCP, "\n"),
		"settings=" + strings.TrimSuffix(emptyConfig, "\n"),
		"policy=yes",
		"socket=yes",
	}
	if diff := cmp.Diff(want, output(t, p)); diff != "" {
		t.Fatalf("Prepare() sandbox wrong result (-want +got) diff = %s", diff)
	}
}

func TestPrepareBwrapMountsCopilotPolicy(t *testing.T) {
	t.Parallel()
	ws, home, policy := t.TempDir(), t.TempDir(), filepath.Join(t.TempDir(), "policy")
	if err := WritePolicy(copilot{}, policy, Policy{}); err != nil {
		t.Fatalf("WritePolicy() error = %v", err)
	}
	b := &recordingBackend{}
	if _, err := Prepare(context.Background(), Spec{
		Profile:    copilot{},
		Sandbox:    SandboxBwrap,
		Backend:    b,
		Base:       sandboxrt.Spec{Name: "a2a-test"},
		Mounts:     []Mount{{Src: ws, Dst: ws}},
		Home:       home,
		PolicyDir:  policy,
		EgressMode: egressrules.ModeUnrestricted,
	}); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	specs := b.prepared()
	if len(specs) != 1 {
		t.Fatalf("Prepare() prepared %d sandboxes, want 1", len(specs))
	}
	want := sandboxrt.Mount{Src: policy, Dst: copilotPolicyDir, ReadOnly: true}
	if !slices.Contains(specs[0].Mounts, want) {
		t.Fatalf("Prepare() mounts %+v, want %+v", specs[0].Mounts, want)
	}
}
