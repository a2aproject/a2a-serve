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

package fsys

import (
	"os"
	"path/filepath"
	"testing"
)

func TestHostRootReadlinkOfFileFails(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "f"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := openHostRoot(dir)
	if err != nil {
		t.Fatalf("openHostRoot() error = %v", err)
	}

	if _, err := r.Readlink("f"); err == nil {
		t.Fatalf("hostRoot.Readlink() of a file error = nil, want one")
	}
}
