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

	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
)

func TestStatusStoppedJSON(t *testing.T) {
	t.Parallel()
	d := newTestAgent(t, &agentdir.Config{Name: "coder", Harness: "opencode"})

	out, err := runCMD(t, "-o", "json", "status", "--dir", d.Root)
	if err != nil {
		t.Fatalf("a2a-serve status error = %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("json.Unmarshal(%q) error = %v", out, err)
	}
	want := map[string]any{"name": "coder", "harness": "opencode", "dir": d.Root, "running": false}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("a2a-serve status -o json wrong result (-want +got) diff = %s", diff)
	}
}
