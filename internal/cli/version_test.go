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

package cli

import (
	"encoding/json"
	"testing"
)

func TestVersionOutput(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"json", "jsonl"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			out, err := runCMD(t, "version", "-o", mode)
			if err != nil {
				t.Fatalf("version -o %s error = %v", mode, err)
			}
			var got versionInfo
			if err := json.Unmarshal([]byte(out), &got); err != nil {
				t.Fatalf("version -o %s = %q, want JSON: %v", mode, out, err)
			}
			if got.Version == "" || got.Platform == "" {
				t.Fatalf("version -o %s = %+v, want version and platform", mode, got)
			}
		})
	}
}

func TestVersionFlagMatchesCommand(t *testing.T) {
	t.Parallel()
	flag, err := runCMD(t, "--version")
	if err != nil {
		t.Fatalf("--version error = %v", err)
	}
	cmd, err := runCMD(t, "version")
	if err != nil {
		t.Fatalf("version error = %v", err)
	}
	if flag != cmd {
		t.Fatalf("--version = %q, want the output of version %q", flag, cmd)
	}
}
