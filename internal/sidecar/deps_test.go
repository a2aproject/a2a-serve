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

package sidecar_test

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const module = "github.com/a2aproject/a2a-serve/"

// TestImportsOnlyTheCore checks that a2a-serve-sidecar is built from the core alone:
// besides its own packages, it imports only packages the core imports,
// whose list core/deps_test.go keeps.
func TestImportsOnlyTheCore(t *testing.T) {
	t.Parallel()
	for _, dir := range []string{".", "../../cmd/a2a-serve-sidecar", "../../core"} {
		readSources(t, dir)
	}
	for _, goos := range []string{"linux", "darwin"} {
		allowed := moduleDeps(t, goos, "../../core/...")
		var frontend []string
		for _, pkg := range moduleDeps(t, goos, "./...", "../../cmd/a2a-serve-sidecar") {
			if strings.HasPrefix(pkg, module+"internal/sidecar") || pkg == module+"cmd/a2a-serve-sidecar" || slices.Contains(allowed, pkg) {
				continue
			}
			frontend = append(frontend, pkg)
		}
		if len(frontend) > 0 {
			t.Fatalf("GOOS=%s go list -deps -test of a2a-serve-sidecar lists packages the core doesn't import %q, want none", goos, frontend)
		}
	}
}

// moduleDeps returns the packages of this module that patterns and their
// tests import on goos, with the packages themselves.
func moduleDeps(t *testing.T, goos string, patterns ...string) []string {
	t.Helper()
	cmd := exec.Command("go", append([]string{"list", "-e", "-deps", "-test"}, patterns...)...)
	cmd.Env = append(os.Environ(), "GOOS="+goos)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("GOOS=%s go list -deps -test %s error = %v", goos, strings.Join(patterns, " "), err)
	}
	var pkgs []string
	for pkg := range strings.FieldsSeq(string(out)) {
		// -test lists packages under test as "p [p.test]", and adds p.test
		// and p_test.
		pkg, _, _ = strings.Cut(pkg, " ")
		pkg = strings.TrimSuffix(strings.TrimSuffix(pkg, ".test"), "_test")
		if strings.HasPrefix(pkg, module) && !slices.Contains(pkgs, pkg) {
			pkgs = append(pkgs, pkg)
		}
	}
	return pkgs
}

// readSources reads the Go files under dir, so that the test cache, which
// doesn't see what go list reads, runs the test again when one changes.
func readSources(t *testing.T, dir string) {
	t.Helper()
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Ext(path) != ".go" {
			return err
		}
		_, err = os.ReadFile(path)
		return err
	})
	if err != nil {
		t.Fatalf("reading the sources in %s: %v", dir, err)
	}
}
