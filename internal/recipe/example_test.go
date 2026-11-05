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

package recipe

import (
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

const exampleRecipe = "../../examples/04-observability-agent/recipe"

// pinned matches a package with a version, such as mcp-grafana@2.2.0.
var pinned = regexp.MustCompile(`@\d+\.\d+\.\d+$`)

func TestExampleRecipe(t *testing.T) {
	t.Parallel()
	r, err := Load(exampleRecipe)
	if err != nil {
		t.Fatalf("Load(%s) error = %v", exampleRecipe, err)
	}
	if owner := r.Config.OwnerSettings(); !slices.Equal(owner, []string{"name"}) {
		t.Fatalf("the example's config.yaml sets owner settings %v, want only name", owner)
	}
	if err := r.Config.Validate(); err != nil {
		t.Fatalf("the example's config.yaml is invalid: %v", err)
	}
	var skills []string
	for _, s := range r.Skills {
		skills = append(skills, s.Name)
	}
	if diff := cmp.Diff([]string{"learn-environment", "logql", "promql", "traceql", "triage"}, skills); diff != "" {
		t.Fatalf("the example's skills (-want +got) diff = %s", diff)
	}
	for _, s := range r.MCP {
		if _, ok := r.Manifest.MCP[s.Name]; !ok {
			t.Fatalf("recipe.yaml doesn't describe the MCP server %s", s.Name)
		}
		if s.Command == "" {
			continue
		}
		if !slices.ContainsFunc(s.Args, pinned.MatchString) {
			t.Fatalf("the MCP server %s runs %s %s, want a pinned version", s.Name, s.Command, strings.Join(s.Args, " "))
		}
		for _, v := range s.Vars() {
			if r.Manifest.Vars[v].Description == "" {
				t.Fatalf("recipe.yaml doesn't describe the variable %s of %s", v, s.Name)
			}
		}
	}
	if got := r.Files["CLAUDE.md"]; got.Link != "AGENTS.md" {
		t.Fatalf("CLAUDE.md = %+v, want a link to AGENTS.md", got)
	}
	if _, ok := r.Files["ENVIRONMENT.md"]; !ok {
		t.Fatalf("the example has no ENVIRONMENT.md")
	}
}
