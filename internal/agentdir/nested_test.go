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

package agentdir

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestFindBelow(t *testing.T) {
	t.Parallel()
	root, elsewhere := t.TempDir(), t.TempDir()
	for _, dir := range []string{".a2a/state", "a/.a2a/state", "a/.a2a/inner/.a2a", "b/c/.a2a", "d", "e"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(elsewhere, filepath.Join(root, "d", ".a2a")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "e", ".a2a"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := FindBelow(root, 100)
	if err != nil {
		t.Fatalf("FindBelow() error = %v", err)
	}

	want := []string{filepath.Join(root, ".a2a"), filepath.Join(root, "a", ".a2a"), filepath.Join(root, "b", "c", ".a2a"), filepath.Join(root, "d", ".a2a")}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("FindBelow() wrong result (-want +got) diff = %s", diff)
	}
}

func TestFindBelowLimit(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	for _, dir := range []string{"a", "b", "c"} {
		if err := os.Mkdir(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	_, err := FindBelow(root, 3)

	if err == nil || !strings.Contains(err.Error(), "more than 3 directories") {
		t.Fatalf("FindBelow() error = %v, want more than 3 directories", err)
	}
}
