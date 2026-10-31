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
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/a2acli"
	"github.com/a2aproject/a2a-serve/internal/agentdir"
)

func TestDockerRunnerCommand(t *testing.T) {
	t.Parallel()
	r := &DockerRunner{
		Name:  "a2a-coder-123",
		Image: "img:1",
		Mounts: []agentdir.Mount{
			{Src: "/src/app", Dst: "/src/app"},
			{Src: "/home/u/docs", Dst: "/docs", ReadOnly: true},
		},
		Home: "/src/app/.a2a/state/home",
		Env:  []string{"OPENAI_API_KEY=sk-secret", "DOCKER_HOST=tcp://evil:2375", "HOME=/root"},
	}
	cmd, err := r.Command(context.Background(), Command{Path: "opencode", Args: []string{"acp"}, Dir: "/src/app", Env: []string{"X=1"}})
	if err != nil {
		t.Fatalf("Command() error = %v", err)
	}
	envFile := argAfter(cmd.Args, "--env-file")
	t.Cleanup(func() {
		if err := os.Remove(envFile); err != nil {
			t.Errorf("removing %s error = %v", envFile, err)
		}
	})
	want := []string{
		"sh", "-c", startScript, "a2a-coder-123",
		"--rm", "-i", "--init",
		"--user", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()),
		"--cap-drop", "ALL", "--security-opt", "no-new-privileges",
		"--memory", "4g", "--cpus", "2", "--pids-limit", "512",
		"--add-host", "host.docker.internal:host-gateway",
		"-v", "/src/app:/src/app",
		"-v", "/home/u/docs:/docs:ro",
		"-v", "/src/app/.a2a/state/home:/home/agent",
		"--env-file", envFile,
		"-e", "HOME=/home/agent",
		"-w", "/src/app",
		"img:1", "opencode", "acp",
	}
	if diff := cmp.Diff(want, cmd.Args); diff != "" {
		t.Fatalf("Command() wrong result (-want +got) diff = %s", diff)
	}
	if diff := cmp.Diff(os.Environ(), cmd.Env); diff != "" {
		t.Fatalf("Command() wrong docker CLI environment (-want +got) diff = %s", diff)
	}
	data, err := os.ReadFile(envFile)
	if err != nil {
		t.Fatalf("reading the env file error = %v", err)
	}
	wantEnv := "OPENAI_API_KEY=sk-secret\nDOCKER_HOST=tcp://evil:2375\nHOME=/root\nX=1\n"
	if diff := cmp.Diff(wantEnv, string(data)); diff != "" {
		t.Fatalf("Command() wrong env file (-want +got) diff = %s", diff)
	}
	fi, err := os.Stat(envFile)
	if err != nil {
		t.Fatalf("os.Stat() error = %v", err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Fatalf("env file mode = %v, want %v", got, os.FileMode(0o600))
	}
}

func TestDockerRunnerCommandRejectsMultilineEnv(t *testing.T) {
	t.Parallel()
	r := &DockerRunner{Name: "n", Image: "img", Env: []string{"KEY=line1\nline2"}}
	if _, err := r.Command(context.Background(), Command{Path: "opencode"}); err == nil {
		t.Fatalf("Command() error = nil, want an error")
	}
}

func argAfter(args []string, flag string) string {
	i := slices.Index(args, flag)
	if i < 0 || i+1 >= len(args) {
		return ""
	}
	return args[i+1]
}

func TestDockerRunnerHidden(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mounts []agentdir.Mount
		hidden []string
		want   []string
	}{
		{
			name:   "inside the workspace",
			mounts: []agentdir.Mount{{Src: "/src/app", Dst: "/src/app"}},
			hidden: []string{"/src/app/.a2a"},
			want:   []string{"type=tmpfs,dst=/src/app/.a2a,readonly"},
		},
		{
			name:   "under a mount with another destination",
			mounts: []agentdir.Mount{{Src: "/src/app", Dst: "/src/app"}, {Src: "/home/u", Dst: "/u", ReadOnly: true}},
			hidden: []string{"/home/u/proj/.a2a"},
			want:   []string{"type=tmpfs,dst=/u/proj/.a2a,readonly"},
		},
		{
			name:   "the mount itself",
			mounts: []agentdir.Mount{{Src: "/src/app/.a2a", Dst: "/cfg"}},
			hidden: []string{"/src/app/.a2a"},
			want:   []string{"type=tmpfs,dst=/cfg,readonly"},
		},
		{
			name:   "outside every mount",
			mounts: []agentdir.Mount{{Src: "/src/app", Dst: "/src/app"}},
			hidden: []string{"/src/application/.a2a", "/other/.a2a"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := &DockerRunner{Name: "n", Image: "img", Mounts: tc.mounts, Hidden: tc.hidden}
			cmd, err := r.Command(context.Background(), Command{Path: "opencode"})
			if err != nil {
				t.Fatalf("Command() error = %v", err)
			}
			var got []string
			for i, a := range cmd.Args {
				if a == "--mount" {
					got = append(got, cmd.Args[i+1])
				}
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Fatalf("Command() wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestDockerRunnerHiddenThroughSymlinks(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	real := filepath.Join(tmp, "real")
	if err := os.MkdirAll(filepath.Join(real, ".a2a"), 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(tmp, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		mounts []agentdir.Mount
		hidden []string
		want   []string
	}{
		{
			name:   "root through a symlink, mount of the real path",
			mounts: []agentdir.Mount{{Src: link, Dst: link}, {Src: real, Dst: "/real"}},
			hidden: []string{filepath.Join(link, ".a2a")},
			want:   []string{"type=tmpfs,dst=" + filepath.Join(link, ".a2a") + ",readonly", "type=tmpfs,dst=/real/.a2a,readonly"},
		},
		{
			name:   "real root, mount through a symlink",
			mounts: []agentdir.Mount{{Src: link, Dst: "/w"}},
			hidden: []string{filepath.Join(real, ".a2a")},
			want:   []string{"type=tmpfs,dst=/w/.a2a,readonly"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := &DockerRunner{Name: "n", Image: "img", Mounts: tc.mounts, Hidden: tc.hidden}
			cmd, err := r.Command(context.Background(), Command{Path: "opencode"})
			if err != nil {
				t.Fatalf("Command() error = %v", err)
			}
			var got []string
			for i, a := range cmd.Args {
				if a == "--mount" {
					got = append(got, cmd.Args[i+1])
				}
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Fatalf("Command() wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestLookup(t *testing.T) {
	t.Parallel()
	ws := Workspace{Dir: "/w", Home: "/h"}
	tests := []struct {
		name    string
		command []string
		want    Command
		image   string
		wantErr bool
	}{
		{name: "opencode", want: Command{Path: "opencode", Args: []string{"acp"}, Dir: "/w",
			Env: []string{"OPENCODE_PERMISSION=" + opencodePermission, "CLOUDCODE_PERMISSION=" + opencodePermission}},
			image: ImageRepo + "opencode:" + ImageTag},
		{name: "claude", want: Command{Path: "claude-agent-acp", Dir: "/w"}, image: ImageRepo + "claude:" + ImageTag},
		{name: "opencode", command: []string{"cloudcode", "acp"}, want: Command{Path: "cloudcode", Args: []string{"acp"}, Dir: "/w",
			Env: []string{"OPENCODE_PERMISSION=" + opencodePermission, "CLOUDCODE_PERMISSION=" + opencodePermission}},
			image: ImageRepo + "opencode:" + ImageTag},
		{name: "gemini", command: []string{"gemini", "--experimental-acp"}, want: Command{Path: "gemini", Args: []string{"--experimental-acp"}, Dir: "/w"}},
		{name: "unknown", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name+strings.Join(tc.command, "_"), func(t *testing.T) {
			t.Parallel()
			p, err := Lookup(tc.name, tc.command)
			if (err != nil) != tc.wantErr {
				t.Fatalf("Lookup() error = %v, want error %v", err, tc.wantErr)
			}
			if err != nil {
				return
			}
			if diff := cmp.Diff(tc.want, p.Command(ws)); diff != "" {
				t.Fatalf("Command() wrong result (-want +got) diff = %s", diff)
			}
			if p.Image() != tc.image {
				t.Fatalf("Image() = %q, want %q", p.Image(), tc.image)
			}
		})
	}
}

func TestBootstrap(t *testing.T) {
	t.Parallel()
	tests := []struct {
		harness string
		file    string
		want    string
	}{
		{harness: "opencode", file: ".config/opencode/opencode.json", want: `"bash": "ask"`},
		{harness: "claude", file: ".claude/settings.json", want: `"defaultMode": "default"`},
	}
	for _, tc := range tests {
		t.Run(tc.harness, func(t *testing.T) {
			t.Parallel()
			home := t.TempDir()
			p, err := Lookup(tc.harness, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := p.Bootstrap(Workspace{Dir: t.TempDir(), Home: home}); err != nil {
				t.Fatalf("Bootstrap() error = %v", err)
			}
			data, err := os.ReadFile(filepath.Join(home, tc.file))
			if err != nil {
				t.Fatalf("reading %s error = %v", tc.file, err)
			}
			if !strings.Contains(string(data), tc.want) {
				t.Fatalf("Bootstrap() wrote %s, want it to contain %s", data, tc.want)
			}

			if err := os.WriteFile(filepath.Join(home, tc.file), []byte("{}"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := p.Bootstrap(Workspace{Home: home}); err != nil {
				t.Fatalf("Bootstrap() error = %v", err)
			}
			if data, _ := os.ReadFile(filepath.Join(home, tc.file)); string(data) != "{}" {
				t.Fatalf("Bootstrap() overwrote user config: %s", data)
			}
		})
	}
}

func TestDockerfiles(t *testing.T) {
	t.Parallel()
	for _, name := range ProfileNames() {
		df, ok := Dockerfile(name)
		if !ok {
			t.Errorf("Dockerfile(%q) missing", name)
			continue
		}
		if want := "\nARG A2A_CLI_VERSION=" + a2acli.PinnedVersion + "\n"; !strings.Contains(string(df), want) {
			t.Errorf("Dockerfile(%q) doesn't contain %q, which CI publishes as tag %s", name, strings.TrimSpace(want), ImageTag)
		}
	}
}

func TestImageTag(t *testing.T) {
	t.Parallel()
	if !regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`).MatchString(ImageTag) {
		t.Fatalf("ImageTag = %q, want a valid docker tag", ImageTag)
	}
}

func TestProfileNamesAreNotReserved(t *testing.T) {
	t.Parallel()
	for _, name := range ProfileNames() {
		if slices.Contains(agentdir.ReservedHarnesses, name) {
			t.Fatalf("ProfileNames() = %v, want none of the reserved %v", ProfileNames(), agentdir.ReservedHarnesses)
		}
	}
}
