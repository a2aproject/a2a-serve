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
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/controlapi"
	"github.com/a2aproject/a2a-serve/internal/output"
)

func TestPrintRestartedJSON(t *testing.T) {
	t.Parallel()
	var out strings.Builder
	cfg := &globalConfig{Printer: output.NewPrinter(&out, output.ModeJson)}

	if err := printRestarted(cfg, &controlapi.Status{Name: "coder", Harness: "acp", Dir: "/work", PID: 42}); err != nil {
		t.Fatalf("printRestarted() error = %v", err)
	}

	type restarted struct {
		Name    string `json:"name"`
		Agent   string `json:"agent"`
		Dir     string `json:"dir"`
		Running bool   `json:"running"`
		PID     int    `json:"pid"`
	}
	var got restarted
	if err := json.Unmarshal([]byte(out.String()), &got); err != nil {
		t.Fatalf("json.Unmarshal(%q) error = %v", out.String(), err)
	}
	want := restarted{Name: "coder", Agent: "acp", Dir: "/work", Running: true, PID: 42}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("printRestarted() wrong result (-want +got) diff = %s", diff)
	}
}
