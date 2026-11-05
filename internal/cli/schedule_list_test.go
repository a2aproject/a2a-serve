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

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/controlapi"
)

func TestScheduleListShowsDisplayNames(t *testing.T) {
	t.Parallel()
	d := newTestAgent(t, &agentdir.Config{Name: "coder", Harness: "opencode"})
	run := func(args ...string) string {
		t.Helper()
		out, err := runCMD(t, append(args, "--dir", d.Root)...)
		if err != nil {
			t.Fatalf("a2a-serve %s error = %v", strings.Join(args, " "), err)
		}
		return out
	}
	run("pair", "invite", "anna")
	run("schedule", "add", "brief", "--cron", "@daily", "--prompt", "hello", "--as", "anna")

	table := run("schedule", "list")
	fields := strings.Fields(strings.Split(strings.TrimSpace(table), "\n")[1])
	if diff := cmp.Diff([]string{"brief", "@daily", "anna"}, fields[:3]); diff != "" {
		t.Fatalf("a2a-serve schedule list row wrong columns (-want +got) diff = %s", diff)
	}

	var all []controlapi.ScheduleInfo
	if err := json.Unmarshal([]byte(run("-o", "json", "schedule", "list")), &all); err != nil {
		t.Fatalf("a2a-serve schedule list -o json error = %v", err)
	}
	if len(all) != 1 || all[0].User != "pair:anna" {
		t.Fatalf("a2a-serve schedule list -o json = %+v, want one schedule of user pair:anna", all)
	}
}
