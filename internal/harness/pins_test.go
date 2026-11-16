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

package harness

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/sandbox"
	"github.com/a2aproject/a2a-serve/internal/sandbox/sandboxtest"
)

func mkdir(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func symlink(t *testing.T, target, link string) string {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	return link
}

// replace renames dir away and creates an empty directory in its place, as
// an agent can through a writable mount that contains dir.
func replace(t *testing.T, dir string) {
	t.Helper()
	if err := os.Rename(dir, dir+".old"); err != nil {
		t.Fatal(err)
	}
	mkdir(t, dir)
}

// startDocker runs Prepare with the docker sandbox and stops what it
// prepared when the test ends.
func startDocker(t *testing.T, spec Spec) (*Prepared, error) {
	t.Helper()
	spec.Profile = claude{}
	spec.Base.Name = "a2a-test"
	p, err := Prepare(context.Background(), spec)
	if err == nil {
		t.Cleanup(func() {
			if err := p.Close(context.Background()); err != nil {
				t.Errorf("Prepared.Close() error = %v", err)
			}
		})
	}
	return p, err
}

func TestPrepareDockerRefusesSymlinksInWritableMounts(t *testing.T) {
	sandboxtest.FakeDocker(t, runningDocker)
	tests := []struct {
		name string
		// setup returns the mounts, hidden paths and home, or "" for a
		// temporary home, with outside a directory out of every mount.
		setup   func(t *testing.T, ws, outside string) ([]agentdir.Mount, []string, string)
		wantErr string
	}{
		{
			name: "mount in the workspace",
			setup: func(t *testing.T, ws, _ string) ([]agentdir.Mount, []string, string) {
				data := mkdir(t, filepath.Join(ws, "data"))
				return []agentdir.Mount{{Src: ws, Dst: ws}, {Src: data, Dst: "/data", ReadOnly: true}}, nil, ""
			},
		},
		{
			name: "mount that is a symlink in the workspace",
			setup: func(t *testing.T, ws, outside string) ([]agentdir.Mount, []string, string) {
				shared := symlink(t, outside, filepath.Join(ws, "shared"))
				return []agentdir.Mount{{Src: ws, Dst: ws}, {Src: shared, Dst: "/shared"}}, nil, ""
			},
			wantErr: "goes through the symlink",
		},
		{
			name: "mount that is a dangling symlink in the workspace",
			setup: func(t *testing.T, ws, outside string) ([]agentdir.Mount, []string, string) {
				shared := symlink(t, filepath.Join(outside, "gone"), filepath.Join(ws, "shared"))
				return []agentdir.Mount{{Src: ws, Dst: ws}, {Src: shared, Dst: "/shared", ReadOnly: true}}, nil, ""
			},
			wantErr: "goes through the symlink",
		},
		{
			name: "mount under a symlink in the workspace",
			setup: func(t *testing.T, ws, outside string) ([]agentdir.Mount, []string, string) {
				mkdir(t, filepath.Join(outside, "sub"))
				link := symlink(t, outside, filepath.Join(ws, "link"))
				return []agentdir.Mount{{Src: ws, Dst: ws}, {Src: filepath.Join(link, "sub"), Dst: "/sub"}}, nil, ""
			},
			wantErr: "goes through the symlink",
		},
		{
			name: "mount through a symlink out of the mounts to a symlink in the workspace",
			setup: func(t *testing.T, ws, outside string) ([]agentdir.Mount, []string, string) {
				inner := symlink(t, t.TempDir(), filepath.Join(ws, "inner"))
				outer := symlink(t, inner, filepath.Join(outside, "outer"))
				return []agentdir.Mount{{Src: ws, Dst: ws}, {Src: outer, Dst: "/outer"}}, nil, ""
			},
			wantErr: "goes through the symlink",
		},
		{
			name: "mount through a symlink out of the mounts to a directory in the workspace",
			setup: func(t *testing.T, ws, outside string) ([]agentdir.Mount, []string, string) {
				data := mkdir(t, filepath.Join(ws, "data"))
				link := symlink(t, data, filepath.Join(outside, "link"))
				return []agentdir.Mount{{Src: ws, Dst: ws}, {Src: link, Dst: "/data"}}, nil, ""
			},
		},
		{
			name: "mount that is a symlink in a read-only workspace",
			setup: func(t *testing.T, ws, outside string) ([]agentdir.Mount, []string, string) {
				shared := symlink(t, outside, filepath.Join(ws, "shared"))
				return []agentdir.Mount{{Src: ws, Dst: ws, ReadOnly: true}, {Src: shared, Dst: "/shared"}}, nil, ""
			},
		},
		{
			name: "mount that is a symlink out of every other mount",
			setup: func(t *testing.T, ws, outside string) ([]agentdir.Mount, []string, string) {
				docs := symlink(t, outside, filepath.Join(t.TempDir(), "docs"))
				return []agentdir.Mount{{Src: ws, Dst: ws}, {Src: docs, Dst: "/docs"}}, nil, ""
			},
		},
		{
			name: "agent directory that is a symlink in the workspace",
			setup: func(t *testing.T, ws, outside string) ([]agentdir.Mount, []string, string) {
				mkdir(t, filepath.Join(outside, agentdir.Name))
				agent := symlink(t, outside, filepath.Join(ws, "agent"))
				return []agentdir.Mount{{Src: ws, Dst: ws}}, []string{filepath.Join(agent, agentdir.Name)}, ""
			},
			wantErr: "goes through the symlink",
		},
		{
			name: "home under a symlink in the workspace",
			setup: func(t *testing.T, ws, outside string) ([]agentdir.Mount, []string, string) {
				a2a := mkdir(t, filepath.Join(ws, agentdir.Name))
				state := symlink(t, outside, filepath.Join(a2a, "state"))
				return []agentdir.Mount{{Src: ws, Dst: ws}}, []string{a2a}, filepath.Join(state, "home")
			},
			wantErr: "goes through the symlink",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ws, outside := t.TempDir(), t.TempDir()
			mounts, hidden, home := tc.setup(t, ws, outside)
			if home == "" {
				home = t.TempDir()
			}
			_, err := startDocker(t, Spec{Mounts: mounts, Hidden: hidden, Home: home, Backend: sandbox.NewDocker(t.TempDir(), nil)})
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Prepare() error = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Prepare() error = %v, want %q", err, tc.wantErr)
			}
			entries, err := os.ReadDir(outside)
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range entries {
				if e.Name() != agentdir.Name && e.Name() != "sub" && e.Name() != "outer" {
					t.Fatalf("Prepare() wrote %s through the symlink", e.Name())
				}
			}
		})
	}
}

func TestPrepareDockerNamesResolvedMountSource(t *testing.T) {
	sandboxtest.FakeDocker(t, runningDocker)
	ws, outside := t.TempDir(), t.TempDir()
	shared := symlink(t, outside, filepath.Join(ws, "shared"))
	resolved, err := filepath.EvalSymlinks(outside)
	if err != nil {
		t.Fatal(err)
	}

	_, err = startDocker(t, Spec{Mounts: []agentdir.Mount{{Src: ws, Dst: ws}, {Src: shared, Dst: "/shared"}}, Home: t.TempDir(), Backend: sandbox.NewDocker(t.TempDir(), nil)})

	if want := "use the resolved path " + resolved + " instead"; err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("Prepare() error = %v, want %q", err, want)
	}
}

// agentLayout is an agent directory in a workspace two levels up, the
// layout of --workspace ../.., in which the agent can rename the agent
// directory.
type agentLayout struct {
	ws, root, a2a, state, home string
}

func newAgentLayout(t *testing.T) agentLayout {
	t.Helper()
	ws := t.TempDir()
	root := filepath.Join(ws, "code", "proj")
	a2a := filepath.Join(root, agentdir.Name)
	state := filepath.Join(a2a, "state")
	home := mkdir(t, filepath.Join(state, "home"))
	return agentLayout{ws: ws, root: root, a2a: a2a, state: state, home: home}
}

func (l agentLayout) spec(t *testing.T, pins *Pins, home string) Spec {
	t.Helper()
	return Spec{Mounts: []agentdir.Mount{{Src: l.ws, Dst: l.ws}}, Hidden: []string{l.a2a}, Home: home, Backend: sandbox.NewDocker(t.TempDir(), nil), Pins: pins}
}

func (l agentLayout) pins(t *testing.T) *Pins {
	t.Helper()
	pins, err := NewPins(l.root, l.a2a, l.state, l.home)
	if err != nil {
		t.Fatalf("NewPins() error = %v", err)
	}
	return pins
}

func TestPrepareDockerChecksPins(t *testing.T) {
	sandboxtest.FakeDocker(t, runningDocker)
	tests := []struct {
		name    string
		replace func(l agentLayout) string
		wantErr bool
	}{
		{name: "unchanged", replace: func(agentLayout) string { return "" }},
		{name: "agent directory replaced", replace: func(l agentLayout) string { return l.root }, wantErr: true},
		{name: "home replaced", replace: func(l agentLayout) string { return l.home }, wantErr: true},
		{name: "unpinned directory replaced", replace: func(l agentLayout) string { return mkdir(t, filepath.Join(l.ws, "code", "other")) }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			l := newAgentLayout(t)
			pins := l.pins(t)
			if dir := tc.replace(l); dir != "" {
				replace(t, dir)
				mkdir(t, l.home)
			}

			_, err := startDocker(t, l.spec(t, pins, l.home))

			if (err != nil) != tc.wantErr {
				t.Fatalf("Prepare() error = %v, want error %v", err, tc.wantErr)
			}
			if tc.wantErr && !strings.Contains(err.Error(), "was replaced") {
				t.Fatalf("Prepare() error = %v, want it to name the replaced directory", err)
			}
		})
	}
}

func TestPrepareDockerPinsUserHome(t *testing.T) {
	sandboxtest.FakeDocker(t, runningDocker)
	l := newAgentLayout(t)
	pins := l.pins(t)
	home := filepath.Join(l.state, "homes", "anna")
	spec := l.spec(t, pins, home)

	if _, err := startDocker(t, spec); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	replace(t, home)
	if _, err := startDocker(t, spec); err == nil || !strings.Contains(err.Error(), "was replaced") {
		t.Fatalf("Prepare() after the home was replaced error = %v, want it replaced", err)
	}
	pins.Unpin(home)
	if _, err := startDocker(t, spec); err != nil {
		t.Fatalf("Prepare() after Unpin() error = %v", err)
	}
}

func TestPreparedCheck(t *testing.T) {
	sandboxtest.FakeDocker(t, runningDocker)
	tests := []struct {
		name    string
		change  func(t *testing.T, l agentLayout, data string)
		wantErr bool
	}{
		{name: "unchanged", change: func(*testing.T, agentLayout, string) {}},
		{
			name: "mount replaced by a symlink",
			change: func(t *testing.T, _ agentLayout, data string) {
				if err := os.Rename(data, data+".old"); err != nil {
					t.Fatal(err)
				}
				symlink(t, t.TempDir(), data)
			},
			wantErr: true,
		},
		{
			name: "agent directory replaced",
			change: func(t *testing.T, l agentLayout, _ string) {
				replace(t, l.root)
				mkdir(t, l.home)
			},
			wantErr: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			l := newAgentLayout(t)
			data := mkdir(t, filepath.Join(l.ws, "data"))
			spec := l.spec(t, l.pins(t), l.home)
			spec.Mounts = append(spec.Mounts, agentdir.Mount{Src: data, Dst: "/data", ReadOnly: true})
			p, err := startDocker(t, spec)
			if err != nil {
				t.Fatalf("Prepare() error = %v", err)
			}

			tc.change(t, l, data)

			if err := startErr(t, p); (err != nil) != tc.wantErr {
				t.Fatalf("Prepared.Start() error = %v, want error %v", err, tc.wantErr)
			}
		})
	}
}

func TestPrepareRefusesHomeThatIsNoDirectory(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		setup   func(t *testing.T, home, target string)
		wantErr string
	}{
		{name: "symlink", setup: func(t *testing.T, home, target string) { symlink(t, target, home) }, wantErr: "is a symlink"},
		{
			name: "file",
			setup: func(t *testing.T, home, _ string) {
				if err := os.WriteFile(home, nil, 0o600); err != nil {
					t.Fatal(err)
				}
			},
			wantErr: "is not a directory",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ws, target := t.TempDir(), t.TempDir()
			home := filepath.Join(t.TempDir(), "home")
			tc.setup(t, home, target)

			_, err := Prepare(context.Background(), Spec{Profile: claude{}, Sandbox: SandboxLocal, Mounts: []agentdir.Mount{{Src: ws, Dst: ws}}, Home: home})

			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Prepare() error = %v, want %q", err, tc.wantErr)
			}
			if entries, err := os.ReadDir(target); err != nil || len(entries) > 0 {
				t.Fatalf("Prepare() wrote %v through the home, error = %v", entries, err)
			}
		})
	}
}
