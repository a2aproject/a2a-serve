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
	"cmp"
	"context"
	"fmt"
	"runtime"
	"strings"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/egress"
	"github.com/a2aproject/a2a-serve/internal/harness"
	"github.com/a2aproject/a2a-serve/internal/sandbox"
)

// useLocalSandbox is the fix of a missing Docker in an agent, and
// startLocalSandbox outside one, where there is no agent to configure.
const (
	useLocalSandbox   = "a2a-serve config set --sandbox local"
	startLocalSandbox = "run a2a-serve start <agent> --sandbox local in your project directory"
)

// sandboxChecks checks Docker and the harness image, or the harness command
// of an agent without a sandbox. cfg is nil outside an agent or when its
// config can't be read.
func sandboxChecks(ctx context.Context, env Env, dir *agentdir.Dir, cfg *agentdir.Config, online bool) []Result {
	var results []Result
	if cfg != nil && cfg.Sandbox == agentdir.SandboxLocal {
		if r, ok := checkHarnessCommand(env, *dir, cfg); ok {
			results = append(results, r)
		}
		if w := harness.DecideEgress(cfg, egressEnv(ctx, env, cfg).Probe()).Warning; w != "" {
			results = append(results, Result{ID: "egress", Status: StatusWarn, Message: w, Fix: "a2a-serve config set --sandbox docker"})
		}
	} else {
		local := useLocalSandbox
		if dir == nil {
			local = startLocalSandbox
		}
		docker := checkDocker(ctx, env, local)
		results = append(results, docker)
		if docker.Status == StatusOK && cfg != nil {
			if r, ok := checkImage(ctx, env, cfg, online); ok {
				results = append(results, r)
			}
			results = append(results, checkEgress(ctx, env, dir, cfg))
		}
	}
	for i := range results {
		results[i].Group = GroupSandbox
	}
	return results
}

// checkDocker checks that Docker answers. local is the fix that runs the
// agent without a sandbox instead.
func checkDocker(ctx context.Context, env Env, local string) Result {
	r := Result{ID: "docker"}
	if _, err := env.LookPath("docker"); err != nil {
		r.Status, r.Message, r.Fix = StatusFail, "docker not installed", "install Docker, or "+local
		return r
	}
	out, err := env.Run(ctx, "docker", "info", "--format", "{{.ServerVersion}}")
	if err != nil {
		r.Status, r.Message, r.Fix = StatusFail, "docker daemon not reachable", "start Docker, or "+local
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
		r.Status, r.Message, r.Fix = StatusFail, fmt.Sprintf("agent %s has no container image", cfg.Harness), "a2a-serve config set --image IMAGE, or "+useLocalSandbox
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

// checkEgress checks that the Docker sandbox's connections go through the
// server.
func checkEgress(ctx context.Context, env Env, dir *agentdir.Dir, cfg *agentdir.Config) Result {
	r := Result{ID: "egress", Status: StatusOK}
	d := harness.DecideEgress(cfg, egressEnv(ctx, env, cfg).Probe())
	if dir != nil && env.Status != nil {
		if st, err := env.Status(ctx, *dir); err == nil && st.Egress != nil && st.Egress.Problem != "" {
			d.Warning = st.Egress.Problem
		}
	}
	switch {
	case cfg.Egress.Mode == egress.ModeUnrestricted:
		r.Status, r.Message = StatusWarn, "--egress unrestricted: "+harness.UnrestrictedEffect
		r.Fix = "a2a-serve config unset egress --restart"
	case d.Problem != "" && harness.StrictEgress(cfg):
		r.Status, r.Message = StatusFail, d.Warning
	case d.Warning != "":
		r.Status, r.Message = StatusWarn, d.Warning
	case d.Boundary && env.CheckInternetRoute() != nil:
		r.Status, r.Message = StatusWarn, env.CheckInternetRoute().Error()
	default:
		r.Message = "the sandbox connects only through the server (--egress " + cmp.Or(cfg.Egress.Mode, egress.ModeInternet) + ")"
	}
	return r
}

// egressEnv is how [harness.DecideEgress] checks the machine of env:
// its a2a-serve, its TUN device, and the architecture of the agent's
// image when docker has it.
func egressEnv(ctx context.Context, env Env, cfg *agentdir.Config) sandbox.EgressEnv {
	return sandbox.EgressEnv{
		GOOS: runtime.GOOS,
		Launcher: func() (string, error) {
			exe, err := env.Executable()
			if err != nil {
				return "", err
			}
			return env.CheckLauncher(exe)
		},
		TUN:        env.CheckTUN,
		ImageArch:  imageArch(ctx, env, cfg),
		ForceRelay: lookupEnv(env, sandbox.EnvForceRelay) == "1",
		RelayImage: func() error { return sandbox.CheckRelayImage(ctx, env.Run, true) },
	}
}

func lookupEnv(env Env, name string) string {
	v, _ := env.LookupEnv(name)
	return v
}

// imageArch returns the CPU architecture of the agent's image, or "" when
// docker doesn't have it.
func imageArch(ctx context.Context, env Env, cfg *agentdir.Config) string {
	if cfg.Sandbox == agentdir.SandboxLocal {
		return ""
	}
	profile, err := harness.Lookup(cfg.Harness, cfg.Command)
	if err != nil {
		return ""
	}
	for _, image := range []string{cfg.Image, profile.Image(), harness.LocalImage(profile.Name())} {
		if image == "" {
			continue
		}
		if out, err := env.Run(ctx, "docker", "image", "inspect", "--format", "{{.Architecture}}", image); err == nil {
			return strings.TrimSpace(string(out))
		}
	}
	return ""
}
