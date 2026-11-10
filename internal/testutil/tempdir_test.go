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

package testutil

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestRemoveStaleDirs(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		dirs    []string
		files   []string
		running map[int]bool
		want    []string
	}{
		{
			name: "removes the dirs of finished processes",
			dirs: []string{"a2a-test-100-1", "a2a-test-100-2", "a2a-test-200-1"},
			want: nil,
		},
		{
			name:    "keeps the dirs of running processes",
			dirs:    []string{"a2a-test-100-1", "a2a-test-200-1"},
			running: map[int]bool{100: true},
			want:    []string{"a2a-test-100-1"},
		},
		{
			name: "keeps dirs of other names",
			dirs: []string{"a2a123", "a2a-test-", "a2a-test-x-1", "a2a-test-100", "other-a2a-test-100-1"},
			want: []string{"a2a-test-", "a2a-test-100", "a2a-test-x-1", "a2a123", "other-a2a-test-100-1"},
		},
		{
			name:  "keeps files that carry the name",
			files: []string{"a2a-test-100-1"},
			want:  []string{"a2a-test-100-1"},
		},
		{
			name: "keeps a pid too big for an int",
			dirs: []string{"a2a-test-99999999999999999999-1"},
			want: []string{"a2a-test-99999999999999999999-1"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			for _, d := range tc.dirs {
				if err := os.MkdirAll(filepath.Join(root, d, "state"), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(root, d, "state", "f"), []byte("x"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			for _, f := range tc.files {
				if err := os.WriteFile(filepath.Join(root, f), []byte("x"), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			err := removeStaleDirs(root, func(pid int) bool { return tc.running[pid] })
			if err != nil {
				t.Fatalf("removeStaleDirs() error = %v", err)
			}

			var got []string
			entries, err := os.ReadDir(root)
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range entries {
				got = append(got, e.Name())
			}
			sort.Strings(got)
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("removeStaleDirs() wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestShortTempDirNamesTheProcess(t *testing.T) {
	t.Parallel()
	dir := ShortTempDir(t)
	if !strings.HasPrefix(filepath.Base(dir), "a2a-test-"+strconv.Itoa(os.Getpid())+"-") {
		t.Errorf("ShortTempDir() = %q, want a name that starts with a2a-test-<pid>-", dir)
	}
	if filepath.Dir(dir) != filepath.Clean(os.TempDir()) {
		t.Errorf("ShortTempDir() = %q, want a dir under %q", dir, os.TempDir())
	}
}
