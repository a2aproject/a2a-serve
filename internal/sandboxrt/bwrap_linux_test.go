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
	// head returns the arguments up to /etc; network is "boundary", "host"
	// or "".
	head := func(network string, extra ...string) []string {
		args := []string{"--unshare-all", "--die-with-parent", "--new-session"}
		if network == "boundary" {
			args = append(args, "--as-pid-1", "--uid", "0", "--gid", "0", "--cap-add", "CAP_NET_ADMIN", "--cap-add", "CAP_SETPCAP")
		}
		args = append(args, extra...)
		if network == "host" {
			args = append(args, "--share-net")
		}
		args = append(args, "--symlink", "usr/bin", "/bin", "--tmpfs", "/etc")
		args = append(args, etc...)
		if network == "host" {
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
				hostNetwork: true,
				mounts:      []Mount{{Src: "/host/ws/sub", Dst: "/ws/sub", ReadOnly: true}, {Src: "/host/ws", Dst: "/ws"}},
				masks:       []Mask{{Dst: "/ws/secret"}, {Src: "/host/mask.json", Dst: "/ws/.mcp.json"}},
				scratch:     []string{"/ws/cache"},
				home:        "/host/home",
				programs:    []programDir{{dir: "/opt/agent/bin", onPath: true}},
				expose:      "/state/api/api.sock",
				command:     []string{"/opt/agent/bin/agent", "acp"},
				dir:         "/ws",
			},
			want: join(head("host"), dirs,
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
			want: join(head("boundary", "--cap-add", "CAP_SYS_ADMIN"),
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
		"no network": {
			run:  bwrapRun{command: []string{"/usr/bin/agent"}},
			want: join(head(""), dirs, []string{"--dir", "/home/agent", "--", "/usr/bin/agent"}),
		},
		"check of the boundary": {
			run:   bwrapRun{launcher: "/state/launcher", egressDir: "/state/egress/abc", command: []string{"/usr/bin/agent"}},
			check: true,
			want: join(head("boundary"), dirs,
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

	path, dirs, err := hostPrograms(Command{Path: "agent"}, []string{"a2a", "sh"}, newSystemView([]string{"/usr", "/bin"}, nil))
	if err != nil {
		t.Fatalf("hostPrograms() error = %v", err)
	}
	if path != real {
		t.Fatalf("hostPrograms() path = %q, want %q", path, real)
	}
	want := []programDir{{dir: filepath.Dir(link), onPath: true}, {dir: filepath.Dir(real)}, {dir: filepath.Dir(cli), onPath: true}, {dir: "/usr/bin", onPath: true, shown: true}}
	if diff := cmp.Diff(want, dirs, cmp.AllowUnexported(programDir{})); diff != "" {
		t.Fatalf("hostPrograms() dirs wrong result (-want +got) diff = %s", diff)
	}
	if got, want := bwrapPath(dirs), filepath.Dir(link)+":"+filepath.Dir(cli)+":"+systemPath; got != want {
		t.Fatalf("bwrapPath() = %q, want %q", got, want)
	}

	if _, _, err := hostPrograms(Command{Path: "./bin/agent"}, nil, systemView{}); err != nil {
		t.Fatalf("hostPrograms() of a path in the sandbox error = %v", err)
	}
	_, _, err = hostPrograms(Command{Path: "agent"}, []string{"a2a-missing"}, systemView{})
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

	_, _, err := hostPrograms(Command{Path: agent}, nil, systemView{})
	if !errors.Is(err, errBroadProgramDir) {
		t.Fatalf("hostPrograms() of a program in the home directory error = %v, want %v", err, errBroadProgramDir)
	}
}

func TestSystemViewLeavesOutHomes(t *testing.T) {
	root := t.TempDir()
	usr := filepath.Join(root, "usr")
	home := filepath.Join(usr, "local", "corp", "home", "me")
	for _, d := range []string{filepath.Join(usr, "bin"), filepath.Join(usr, "local", "bin"), filepath.Join(usr, "local", "corp", "tools"), filepath.Join(home, ".ssh"), filepath.Join(home, "go", "bin")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("usr/bin", filepath.Join(root, "bin")); err != nil {
		t.Fatal(err)
	}

	v := newSystemView([]string{usr, filepath.Join(root, "bin")}, []string{home})
	want := []string{
		"--ro-bind-try", filepath.Join(usr, "bin"), filepath.Join(usr, "bin"),
		"--ro-bind-try", filepath.Join(usr, "local", "bin"), filepath.Join(usr, "local", "bin"),
		"--ro-bind-try", filepath.Join(usr, "local", "corp", "tools"), filepath.Join(usr, "local", "corp", "tools"),
		"--symlink", "usr/bin", filepath.Join(root, "bin"),
	}
	if diff := cmp.Diff(want, v.args); diff != "" {
		t.Fatalf("newSystemView() args wrong result (-want +got) diff = %s", diff)
	}
	for p, want := range map[string]bool{filepath.Join(usr, "bin", "sh"): true, filepath.Join(home, ".ssh"): false, filepath.Join(usr, "local", "corp"): false} {
		if got := v.shows(p); got != want {
			t.Fatalf("systemView.shows(%q) = %v, want %v", p, got, want)
		}
	}

	t.Setenv("PATH", filepath.Join(home, "go", "bin"))
	tool := filepath.Join(home, "go", "bin", "a2a")
	if err := os.WriteFile(tool, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, dirs, err := hostPrograms(Command{Path: "./agent"}, []string{"a2a"}, v)
	if err != nil {
		t.Fatalf("hostPrograms() error = %v", err)
	}
	if diff := cmp.Diff([]programDir{{dir: filepath.Join(home, "go", "bin"), onPath: true}}, dirs, cmp.AllowUnexported(programDir{})); diff != "" {
		t.Fatalf("hostPrograms() dirs of a program in a home under a system directory wrong result (-want +got) diff = %s", diff)
	}
}

func TestBwrapProfileFix(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if got := bwrapProfileFix(dir); got != "" {
		t.Fatalf("bwrapProfileFix() without profiles = %q, want none", got)
	}
	profile := filepath.Join(dir, "bwrap-userns-restrict")
	if err := os.WriteFile(profile, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	want := "the AppArmor profiles " + profile + " can strip the capabilities in bwrap's user namespaces: unload them with sudo apparmor_parser -R " + profile
	if got := bwrapProfileFix(dir); got != want {
		t.Fatalf("bwrapProfileFix() = %q, want %q", got, want)
	}
}

func TestUserNamespaceFix(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		sysctls     map[string]string
		wantFix     string
		wantNetwork string
	}{
		"no restriction": {wantFix: "allow unprivileged user namespaces"},
		"apparmor": {
			sysctls:     map[string]string{"kernel/apparmor_restrict_unprivileged_userns": "1\n"},
			wantFix:     "AppArmor restricts unprivileged user namespaces (kernel.apparmor_restrict_unprivileged_userns=1): give bwrap an AppArmor profile that allows them, or run sudo sysctl -w kernel.apparmor_restrict_unprivileged_userns=0 and put the same setting in a file in /etc/sysctl.d to keep it after a reboot",
			wantNetwork: "AppArmor restricts unprivileged user namespaces (kernel.apparmor_restrict_unprivileged_userns=1): give bwrap an AppArmor profile that allows them, or run sudo sysctl -w kernel.apparmor_restrict_unprivileged_userns=0 and put the same setting in a file in /etc/sysctl.d to keep it after a reboot",
		},
		"apparmor off": {sysctls: map[string]string{"kernel/apparmor_restrict_unprivileged_userns": "0\n"}, wantFix: "allow unprivileged user namespaces"},
		"debian clone sysctl": {
			sysctls: map[string]string{"kernel/unprivileged_userns_clone": "0\n"},
			wantFix: "run sudo sysctl -w kernel.unprivileged_userns_clone=1",
		},
		"no user namespaces": {
			sysctls: map[string]string{"user/max_user_namespaces": "0\n"},
			wantFix: "run sudo sysctl -w user.max_user_namespaces=15000",
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			proc := t.TempDir()
			for p, v := range tc.sysctls {
				file := filepath.Join(proc, "sys", p)
				if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(file, []byte(v), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if got := userNamespaceFix(proc); got != tc.wantFix {
				t.Fatalf("userNamespaceFix() = %q, want %q", got, tc.wantFix)
			}
			if got := appArmorFix(proc); got != tc.wantNetwork {
				t.Fatalf("appArmorFix() = %q, want %q", got, tc.wantNetwork)
			}
		})
	}
}
