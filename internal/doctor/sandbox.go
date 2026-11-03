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

package doctor

import (
	"context"
	"fmt"
	"strings"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/harness"
)

const useLocalSandbox = "a2a-serve config set --sandbox local"

// sandboxChecks checks Docker and the harness image, or the harness command
// of an agent without a sandbox. cfg is nil outside an agent or when its
// config can't be read.
func sandboxChecks(ctx context.Context, env Env, dir *agentdir.Dir, cfg *agentdir.Config, online bool) []Result {
	var results []Result
	if cfg != nil && cfg.Sandbox == agentdir.SandboxLocal {
		if r, ok := checkHarnessCommand(env, *dir, cfg); ok {
			results = append(results, r)
		}
	} else {
		docker := checkDocker(ctx, env)
		results = append(results, docker)
		if docker.Status == StatusOK && cfg != nil {
			if r, ok := checkImage(ctx, env, cfg, online); ok {
				results = append(results, r)
			}
		}
	}
	for i := range results {
		results[i].Group = GroupSandbox
	}
	return results
}

func checkDocker(ctx context.Context, env Env) Result {
	r := Result{ID: "docker"}
	if _, err := env.LookPath("docker"); err != nil {
		r.Status, r.Message, r.Fix = StatusFail, "docker not installed", "install Docker, or "+useLocalSandbox
		return r
	}
	out, err := env.Run(ctx, "docker", "info", "--format", "{{.ServerVersion}}")
	if err != nil {
		r.Status, r.Message, r.Fix = StatusFail, "docker daemon not reachable", "start Docker, or "+useLocalSandbox
		if host, herr := env.Run(ctx, "docker", "context", "inspect", "--format", "{{.Endpoints.docker.Host}}"); herr == nil && len(strings.TrimSpace(string(host))) > 0 {
			r.Message += " at " + strings.TrimSpace(string(host))
		}
		r.Detail = firstLine(err.Error())
		return r
	}
	r.Status, r.Message = StatusOK, "docker "+strings.TrimSpace(string(out))
	return r
}

// checkImage checks that the harness image is present, or with online that
// it can be pulled. It reports nothing when the harness is unknown, which
// the config check reports.
func checkImage(ctx context.Context, env Env, cfg *agentdir.Config, online bool) (Result, bool) {
	profile, err := harness.Lookup(cfg.Harness, cfg.Command)
	if err != nil {
		return Result{}, false
	}
	r := Result{ID: "harness-image"}
	image := cfg.Image
	if image == "" {
		image = profile.Image()
	}
	if image == "" {
		r.Status, r.Message, r.Fix = StatusFail, fmt.Sprintf("harness %s has no container image", cfg.Harness), "a2a-serve config set --image IMAGE, or "+useLocalSandbox
		return r, true
	}
	present := func(img string) bool {
		_, err := env.Run(ctx, "docker", "image", "inspect", "--format", "{{.Id}}", img)
		return err == nil
	}
	if present(image) {
		r.Status, r.Message = StatusOK, "image "+image+" present"
		return r, true
	}
	_, buildable := harness.Dockerfile(profile.Name())
	buildable = buildable && image == profile.Image()
	if buildable {
		if local := harness.LocalImage(profile.Name()); present(local) {
			r.Status, r.Message = StatusOK, "image "+local+" present"
			return r, true
		}
	}
	if !online {
		r.Status, r.Message = StatusWarn, "image "+image+" not pulled yet; the first start pulls it"
		return r, true
	}
	if _, err := env.Run(ctx, "docker", "manifest", "inspect", image); err == nil {
		r.Status, r.Message = StatusOK, "image "+image+" pullable"
		return r, true
	}
	if buildable {
		r.Status, r.Message = StatusWarn, "image "+image+" can't be pulled; the first start builds it"
		return r, true
	}
	r.Status, r.Message, r.Fix = StatusFail, "image "+image+" can't be pulled", "a2a-serve config set --image IMAGE"
	return r, true
}

func checkHarnessCommand(env Env, dir agentdir.Dir, cfg *agentdir.Config) (Result, bool) {
	profile, err := harness.Lookup(cfg.Harness, cfg.Command)
	if err != nil {
		return Result{}, false
	}
	name := profile.Command(harness.Workspace{Dir: dir.Root, Home: dir.HomeDir()}).Path
	path, err := env.LookPath(name)
	if err != nil {
		return Result{
			ID:      "harness-command",
			Status:  StatusFail,
			Message: name + " not on PATH (sandbox local)",
			Fix:     "install " + name + ", or a2a-serve config set --sandbox docker",
		}, true
	}
	return Result{ID: "harness-command", Status: StatusOK, Message: fmt.Sprintf("%s at %s (sandbox local)", name, path)}, true
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return line
}
