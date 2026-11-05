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

//go:build e2e

package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/go-cmp/cmp"
)

// recipeRepo copies the observability example's recipe into a new git
// repository and returns a function that runs git in it.
func recipeRepo(t *testing.T) (dir string, git func(args ...string)) {
	t.Helper()
	src, err := filepath.Abs(filepath.Join("..", "examples", "04-observability-agent", "recipe"))
	if err != nil {
		t.Fatal(err)
	}
	dir = filepath.Join(t.TempDir(), "recipes")
	if out, err := exec.Command("cp", "-a", src, dir).CombinedOutput(); err != nil {
		t.Fatalf("cp -a %s: %v: %s", src, err, out)
	}
	git = func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.name=e2e", "-c", "user.email=e2e@example.com", "-c", "commit.gpgsign=false"}, args...)...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	git("init", "-q", "-b", "main")
	git("add", "-A")
	git("commit", "-q", "-m", "v1")
	return dir, git
}

func TestRecipeCreateAndUpdate(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	owner := newUser(t)
	repo, git := recipeRepo(t)
	dir := filepath.Join(owner.home, "o11y")
	o11y := &agent{u: owner, name: "o11y", dir: dir}

	out := owner.run(owner.home, "a2a", "serve", "recipe", "file://"+repo+"#main", "--dir", dir, "--yes",
		"--agent", "acp", "--command", filepath.Join(binDir, "fakeacp"), "--sandbox", "local",
		"--env", "GRAFANA_URL=http://grafana.invalid", "--secret", "GRAFANA_SERVICE_ACCOUNT_TOKEN=token")
	if !strings.Contains(out, "✓ created o11y") || !strings.Contains(out, "with github, kubernetes off") {
		t.Fatalf("a2a serve recipe output = %s, want the agent created with the optional servers off", out)
	}
	t.Cleanup(o11y.cleanup)
	o11y.serve("start", "--detach")
	o11y.url = strings.TrimSpace(o11y.serve("status", "--url=local"))
	got := resultOf(owner.mustSend(o11y.url, "hello"))
	if diff := cmp.Diff(taskResult{State: a2a.TaskStateCompleted, Text: "Hello from fakeacp."}, got); diff != "" {
		t.Fatalf("a2a send hello wrong result (-want +got) diff = %s", diff)
	}
	if status := o11y.serve("status"); !strings.Contains(status, "recipe ") || !strings.Contains(status, "at main (") {
		t.Fatalf("a2a serve status = %s, want the recipe line", status)
	}

	if err := os.WriteFile(filepath.Join(dir, "ENVIRONMENT.md"), []byte("# Our services\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{"AGENTS.md": "# Observability agent v2\n", "ENVIRONMENT.md": "# Environment v2\n"} {
		if err := os.WriteFile(filepath.Join(repo, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git("commit", "-q", "-am", "v2")

	out = o11y.serve("recipe", "--yes")

	for _, want := range []string{"~ AGENTS.md", "kept          ENVIRONMENT.md, changed here and in the recipe", "the recipe is at main ("} {
		if !strings.Contains(out, want) {
			t.Fatalf("a2a serve recipe update output = %s, want %q", out, want)
		}
	}
	for name, want := range map[string]string{"AGENTS.md": "# Observability agent v2\n", "ENVIRONMENT.md": "# Our services\n", "ENVIRONMENT.md.recipe": "# Environment v2\n"} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || string(data) != want {
			t.Fatalf("%s = %q, %v, want %q", name, data, err, want)
		}
	}
	got = resultOf(owner.mustSend(o11y.url, "hello"))
	if got.State != a2a.TaskStateCompleted {
		t.Fatalf("a2a send hello after the update state = %v, want %v", got.State, a2a.TaskStateCompleted)
	}
}
