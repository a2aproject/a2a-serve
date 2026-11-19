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

//go:build linux

package sandboxrt

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/boundary"
)

func TestBwrapArgs(t *testing.T) {
	t.Parallel()
	var etc []string
	for _, f := range etcFiles {
		etc = append(etc, "--ro-bind-try", "/etc/"+f, "/etc/"+f)
	}
	head := func(behind bool, extra ...string) []string {
		args := []string{"--unshare-all", "--die-with-parent", "--new-session"}
		if behind {
			args = append(args, "--uid", "0", "--gid", "0", "--cap-add", "CAP_NET_ADMIN", "--cap-add", "CAP_SETPCAP")
		}
		args = append(args, extra...)
		if !behind {
			args = append(args, "--share-net")
		}
		args = append(args, "--symlink", "usr/bin", "/bin", "--tmpfs", "/etc")
		args = append(args, etc...)
		if !behind {
			args = append(args, "--ro-bind-try", "/etc/resolv.conf", "/etc/resolv.conf")
		}
		return args
	}
	dirs := []string{"--proc", "/proc", "--dev", "/dev", "--tmpfs", "/tmp", "--dir", "/var", "--tmpfs", "/var/tmp", "--tmpfs", "/run"}
	join := func(parts ...[]string) []string { return slices.Concat(parts...) }
	tests := map[string]struct {
		run   bwrapRun
		check bool
		want  []string
	}{
		"host network": {
			run: bwrapRun{
				mounts:   []Mount{{Src: "/host/ws/sub", Dst: "/ws/sub", ReadOnly: true}, {Src: "/host/ws", Dst: "/ws"}},
				masks:    []Mask{{Dst: "/ws/secret"}, {Src: "/host/mask.json", Dst: "/ws/.mcp.json"}},
				scratch:  []string{"/ws/cache"},
				home:     "/host/home",
				programs: []programDir{{dir: "/opt/agent/bin", onPath: true}},
				expose:   "/state/api/api.sock",
				command:  []string{"/opt/agent/bin/agent", "acp"},
				dir:      "/ws",
			},
			want: join(head(false), dirs,
				[]string{"--ro-bind", "/state/api", "/run/a2a"},
				[]string{"--bind", "/host/ws", "/ws"},
				[]string{"--ro-bind", "/host/ws/sub", "/ws/sub"},
				[]string{"--tmpfs", "/ws/cache"},
				[]string{"--bind", "/host/home", "/home/agent"},
				[]string{"--tmpfs", "/ws/secret", "--remount-ro", "/ws/secret"},
				[]string{"--ro-bind", "/host/mask.json", "/ws/.mcp.json"},
				[]string{"--ro-bind", "/opt/agent/bin", "/opt/agent/bin"},
				[]string{"--chdir", "/ws", "--", "/opt/agent/bin/agent", "acp"},
			),
		},
		"boundary with a live workspace": {
			run: bwrapRun{
				launcher:  "/state/launcher",
				egressDir: "/state/egress/abc",
				served:    []boundary.Mount{{Dst: "/ws", Socket: "/run/a2a-egress/fs-0.sock"}},
				bundle:    &bundleCopy{path: "/etc/ssl/certs/ca-certificates.crt", copy: "/state/bwrap-1/ca-bundle.pem"},
				command:   []string{"/usr/bin/agent"},
				dir:       "/ws",
			},
			want: join(head(true, "--cap-add", "CAP_SYS_ADMIN"),
				[]string{"--bind", "/state/bwrap-1/ca-bundle.pem", "/etc/ssl/certs/ca-certificates.crt"},
				dirs,
				[]string{"--dev-bind", "/dev/net/tun", "/dev/net/tun", "--dev-bind", "/dev/fuse", "/dev/fuse"},
				[]string{"--ro-bind", "/state/launcher", "/run/a2a-init", "--ro-bind", "/state/egress/abc", "/run/a2a-egress"},
				[]string{"--dir", "/ws"},
				[]string{"--dir", "/home/agent"},
				[]string{"--chdir", "/ws", "--", "/run/a2a-init", "sandbox-init", "--confine", "--fs", "/ws=/run/a2a-egress/fs-0.sock"},
				[]string{"/run/a2a-egress/egress.sock", "0", "0", "/usr/bin/agent"},
			),
		},
		"check of the boundary": {
			run:   bwrapRun{launcher: "/state/launcher", egressDir: "/state/egress/abc", command: []string{"/usr/bin/agent"}},
			check: true,
			want: join(head(true), dirs,
				[]string{"--dev-bind", "/dev/net/tun", "/dev/net/tun"},
				[]string{"--ro-bind", "/state/launcher", "/run/a2a-init", "--ro-bind", "/state/egress/abc", "/run/a2a-egress"},
				[]string{"--dir", "/home/agent"},
				[]string{"--", "/run/a2a-init", "sandbox-init", "--check", "--confine", "/run/a2a-egress/egress.sock", "0", "0"},
			),
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			tc.run.system = []string{"--symlink", "usr/bin", "/bin"}
			if diff := cmp.Diff(tc.want, tc.run.args(tc.check)); diff != "" {
				t.Fatalf("bwrapRun.args() wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestHostPrograms(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "versions", "agent-1.2")
	link := filepath.Join(dir, "bin", "agent")
	cli := filepath.Join(dir, "tools", "a2a")
	for _, p := range []string{real, cli} {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", filepath.Dir(link)+":"+filepath.Dir(cli)+":/usr/bin")

	path, dirs, err := hostPrograms(Command{Path: "agent"}, []string{"a2a", "sh"})
	if err != nil {
		t.Fatalf("hostPrograms() error = %v", err)
	}
	if path != real {
		t.Fatalf("hostPrograms() path = %q, want %q", path, real)
	}
	want := []programDir{{dir: filepath.Dir(link), onPath: true}, {dir: filepath.Dir(real)}, {dir: filepath.Dir(cli), onPath: true}}
	if diff := cmp.Diff(want, dirs, cmp.AllowUnexported(programDir{})); diff != "" {
		t.Fatalf("hostPrograms() dirs wrong result (-want +got) diff = %s", diff)
	}
	if got, want := bwrapPath(dirs), filepath.Dir(link)+":"+filepath.Dir(cli)+":"+systemPath; got != want {
		t.Fatalf("bwrapPath() = %q, want %q", got, want)
	}

	if _, _, err := hostPrograms(Command{Path: "./bin/agent"}, nil); err != nil {
		t.Fatalf("hostPrograms() of a path in the sandbox error = %v", err)
	}
	_, _, err = hostPrograms(Command{Path: "agent"}, []string{"a2a-missing"})
	if pe, ok := errors.AsType[*ProgramError](err); !ok || pe.Name != "a2a-missing" {
		t.Fatalf("hostPrograms() with a missing program error = %v, want a ProgramError of a2a-missing", err)
	}
}

func TestHostProgramsRefusesProgramInTheHomeDirectory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	agent := filepath.Join(home, "agent")
	if err := os.WriteFile(agent, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	_, _, err := hostPrograms(Command{Path: agent}, nil)
	if !errors.Is(err, errBroadProgramDir) {
		t.Fatalf("hostPrograms() of a program in the home directory error = %v, want %v", err, errBroadProgramDir)
	}
}
