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

// Package a2acli finds the a2a CLI installed next to the server: harnesses
// call other agents with it, and nodes download it from the server.
package a2acli

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// Binary is the name of the a2a CLI on PATH.
const Binary = "a2a"

// Path returns the path of the a2a CLI on PATH.
func Path() (string, error) {
	p, err := exec.LookPath(Binary)
	if err != nil {
		return "", fmt.Errorf("the a2a CLI is not on PATH (https://github.com/a2aproject/a2a-cli#installation): %w", err)
	}
	return p, nil
}

// Skill returns the agent skill of the installed a2a CLI.
func Skill(ctx context.Context) (string, error) {
	return run(ctx, "skill")
}

// Version returns the version of the installed a2a CLI as a Go module
// version, such as v0.4.0, or "latest" when it is unknown or a development
// build.
func Version(ctx context.Context) string {
	out, err := run(ctx, "version", "-o", "json")
	if err != nil {
		return "latest"
	}
	var info struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal([]byte(out), &info); err != nil {
		return "latest"
	}
	return moduleVersion(info.Version)
}

func moduleVersion(v string) string {
	if v == "" || v == "dev" || v == "(devel)" {
		return "latest"
	}
	if !strings.HasPrefix(v, "v") {
		return "v" + v
	}
	return v
}

func run(ctx context.Context, args ...string) (string, error) {
	p, err := Path()
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, p, args...).Output()
	if err != nil {
		return "", fmt.Errorf("%s %s: %w", Binary, strings.Join(args, " "), err)
	}
	return string(out), nil
}
