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

package main_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestBinaryAnswersPluginInfo(t *testing.T) {
	t.Parallel()
	bin := filepath.Join(t.TempDir(), "a2a-serve-sidecar")
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build error = %v\n%s", err, out)
	}

	out, err := exec.Command(bin, "info").Output()
	if err != nil {
		t.Fatalf("a2a-serve-sidecar info error = %v", err)
	}

	var info struct{ Name string }
	if err := json.Unmarshal(out, &info); err != nil || info.Name != "serve-sidecar" {
		t.Fatalf("a2a-serve-sidecar info = %q, want JSON with the name serve-sidecar", out)
	}
}
