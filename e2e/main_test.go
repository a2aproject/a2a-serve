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

//go:build e2e

package e2e

import (
	"cmp"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/a2aproject/a2a-serve/internal/a2acli"
)

// binDir holds a2a, a2a-serve and fakeacp; it goes first on every user's PATH.
var binDir string

// sandbox is where the agents run: local, or docker with E2E_SANDBOX=docker.
var sandbox = cmp.Or(os.Getenv("E2E_SANDBOX"), "local")

// dockerHost is the daemon the docker CLI of the users talks to, which their
// own HOME doesn't name.
var dockerHost string

// fakeacpImage is the image the agents run in with the docker sandbox.
const fakeacpImage = "a2a-e2e-fakeacp:latest"

// fakeacpDockerfile builds fakeacpImage from a context holding fakeacp.
const fakeacpDockerfile = `FROM scratch
COPY fakeacp /usr/local/bin/fakeacp
`

func TestMain(m *testing.M) {
	os.Exit(run(m))
}

func run(m *testing.M) int {
	dir, err := os.MkdirTemp("", "a2a-e2e-bin-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "e2e:", err)
		return 1
	}
	defer func() {
		if err := os.RemoveAll(dir); err != nil {
			fmt.Fprintln(os.Stderr, "e2e: remove bin dir:", err)
		}
	}()
	if err := buildBinaries(dir); err != nil {
		fmt.Fprintln(os.Stderr, "e2e:", err)
		return 1
	}
	binDir = dir
	if sandbox == "docker" {
		if err := buildImage(dir); err != nil {
			fmt.Fprintln(os.Stderr, "e2e:", err)
			return 1
		}
	}
	return m.Run()
}

func buildBinaries(dir string) error {
	root, err := filepath.Abs("..")
	if err != nil {
		return err
	}
	if err := goCmd(root, nil, "build", "-o", filepath.Join(dir, "a2a-serve"), "."); err != nil {
		return err
	}
	// fakeacp is static, so that it also runs in an empty image.
	if err := goCmd(root, []string{"CGO_ENABLED=0"}, "build", "-o", filepath.Join(dir, "fakeacp"), "./internal/agentsrv/testdata/fakeacp"); err != nil {
		return err
	}
	return installCLI(dir)
}

// buildImage builds fakeacpImage and finds the docker daemon.
func buildImage(dir string) error {
	ctxDir := filepath.Join(dir, "image")
	if err := os.Mkdir(ctxDir, 0o700); err != nil {
		return err
	}
	if err := os.Link(filepath.Join(dir, "fakeacp"), filepath.Join(ctxDir, "fakeacp")); err != nil {
		return err
	}
	build := exec.Command("docker", "build", "-t", fakeacpImage, "-f", "-", ctxDir)
	build.Stdin = strings.NewReader(fakeacpDockerfile)
	if out, err := build.CombinedOutput(); err != nil {
		return fmt.Errorf("docker build: %w\n%s", err, out)
	}
	dockerHost = os.Getenv("DOCKER_HOST")
	if dockerHost != "" {
		return nil
	}
	out, err := exec.Command("docker", "context", "inspect", "--format", "{{.Endpoints.docker.Host}}").Output()
	if err != nil {
		return fmt.Errorf("docker context inspect: %w", err)
	}
	dockerHost = strings.TrimSpace(string(out))
	return nil
}

func installCLI(dir string) error {
	dst := filepath.Join(dir, "a2a")
	if src := os.Getenv("E2E_A2A_CLI"); src != "" {
		return os.Symlink(src, dst)
	}
	if err := goCmd(dir, []string{"GOBIN=" + dir}, "install", "github.com/a2aproject/a2a-cli@"+a2acli.MinVersion); err != nil {
		return err
	}
	return os.Rename(filepath.Join(dir, "a2a-cli"), dst)
}

func goCmd(dir string, env []string, args ...string) error {
	cmd := exec.Command("go", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("go %v: %w\n%s", args, err, out)
	}
	return nil
}
