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
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/a2aproject/a2a-serve/internal/a2acli"
)

// binDir holds a2a, a2a-serve and fakeacp; it goes first on every user's PATH.
var binDir string

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
	return m.Run()
}

func buildBinaries(dir string) error {
	root, err := filepath.Abs("..")
	if err != nil {
		return err
	}
	builds := [][]string{
		{"build", "-o", filepath.Join(dir, "a2a-serve"), "."},
		{"build", "-o", filepath.Join(dir, "fakeacp"), "./internal/agentsrv/testdata/fakeacp"},
	}
	for _, args := range builds {
		if err := goCmd(root, nil, args...); err != nil {
			return err
		}
	}
	return installCLI(dir)
}

func installCLI(dir string) error {
	dst := filepath.Join(dir, "a2a")
	if src := os.Getenv("E2E_A2A_CLI"); src != "" {
		return os.Symlink(src, dst)
	}
	if err := goCmd(dir, []string{"GOBIN=" + dir}, "install", "github.com/a2aproject/a2a-cli@"+a2acli.PinnedVersion); err != nil {
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
