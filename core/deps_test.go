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

// Package core_test checks the boundary between the core and the frontend.
package core_test

import (
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

const module = "github.com/a2aproject/a2a-serve/"

// sharedLeaves are the packages outside core that both sides use. They
// import nothing of the frontend.
var sharedLeaves = []string{
	module + "internal/a2aproxy",
	module + "internal/a2atext",
	module + "internal/agentprofile",
	module + "internal/agentrun",
	module + "internal/boundary",
	module + "internal/cardinfo",
	module + "internal/cryptoutil",
	module + "internal/egressrules",
	module + "internal/fsutil",
	module + "internal/hops",
	module + "internal/httpserver",
	module + "internal/jsonhttp",
	module + "internal/names",
	module + "internal/projectfiles",
	module + "internal/question",
	module + "internal/sandboxrt",
	module + "internal/sandboxrt/sandboxtest",
	module + "internal/testutil",
	module + "internal/toolrules",
	module + "internal/turncount",
	module + "internal/workspaces",
}

// publicImports are the public core packages that each public core
// package may import: egress <- sandbox <- a2aserve, fsys <- a2aserve,
// approval <- contact, and no package imports a2aserve.
var publicImports = map[string][]string{
	"egress":   nil,
	"sandbox":  {"egress"},
	"fsys":     nil,
	"approval": nil,
	"contact":  {"approval"},
	"a2aserve": {"egress", "fsys", "sandbox"},
}

// platforms are the systems whose files the test lists, since go list sees
// only the files of one.
var platforms = []string{"linux", "darwin", "windows"}

func TestCoreImportsNoFrontendPackage(t *testing.T) {
	t.Parallel()
	readSources(t, ".")
	for _, pkg := range sharedLeaves {
		readSources(t, filepath.Join("..", strings.TrimPrefix(pkg, module)))
	}
	var frontend []string
	for _, goos := range platforms {
		cmd := exec.Command("go", "list", "-e", "-deps", "-test", "./...")
		cmd.Env = append(os.Environ(), "GOOS="+goos)
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("GOOS=%s go list -e -deps -test ./... error = %v", goos, err)
		}
		for pkg := range strings.FieldsSeq(string(out)) {
			// -test lists packages under test as "p [p.test]", and adds p.test
			// and p_test.
			pkg, _, _ = strings.Cut(pkg, " ")
			pkg = strings.TrimSuffix(strings.TrimSuffix(pkg, ".test"), "_test")
			if !strings.HasPrefix(pkg, module) || pkg == module+"core" || strings.HasPrefix(pkg, module+"core/") || slices.Contains(sharedLeaves, pkg) || slices.Contains(frontend, pkg) {
				continue
			}
			frontend = append(frontend, pkg)
		}
	}
	if len(frontend) > 0 {
		t.Fatalf("go list -deps -test ./... of the core lists frontend packages %q, want none", frontend)
	}
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

func TestPublicPackagesImportOnlyThePackagesBelowThem(t *testing.T) {
	t.Parallel()
	readSources(t, ".")
	for _, pkg := range sharedLeaves {
		readSources(t, filepath.Join("..", strings.TrimPrefix(pkg, module)))
	}
	cmd := exec.Command("go", "list", "-f", "{{.Name}}", "./...")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list ./... error = %v", err)
	}
	var listed []string
	for name := range strings.FieldsSeq(string(out)) {
		if name != "core_test" && name != "core" {
			listed = append(listed, name)
		}
	}
	slices.Sort(listed)
	if diff := cmp.Diff(slices.Sorted(maps.Keys(publicImports)), listed); diff != "" {
		t.Fatalf("go list ./... of the core lists other packages than publicImports (-want +got) diff = %s", diff)
	}
	for _, goos := range platforms {
		for pkg, allowed := range publicImports {
			cmd := exec.Command("go", "list", "-deps", "./"+pkg)
			cmd.Env = append(os.Environ(), "GOOS="+goos)
			out, err := cmd.Output()
			if err != nil {
				t.Fatalf("GOOS=%s go list -deps ./%s error = %v", goos, pkg, err)
			}
			var got []string
			for dep := range strings.FieldsSeq(string(out)) {
				name, ok := strings.CutPrefix(dep, module+"core/")
				if ok && name != pkg {
					got = append(got, name)
				}
			}
			slices.Sort(got)
			if diff := cmp.Diff(allowed, got, cmpopts.EquateEmpty()); diff != "" {
				t.Fatalf("GOOS=%s go list -deps ./%s lists wrong public packages (-want +got) diff = %s", goos, pkg, diff)
			}
		}
	}
}
