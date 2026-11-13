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

// Package slimbackend is the SLIM backend of a relay. It runs a SLIM node on
// loopback, admits agents with invites, hands out the keys of the relay's
// members, and lets only members reach the node, over WebSocket at the
// relay's URL.
package slimbackend

import (
	"context"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"time"

	"github.com/a2aproject/a2a-serve/internal/download"
)

// Name is the backend's name in the relay's output.
const Name = "SLIM"

// Version is the release of slimctl that runs the relay's node.
const Version = "v3.4.6"

// BinaryEnv names a slimctl to run in place of the pinned release.
const BinaryEnv = "A2A_SERVE_SLIMCTL"

// archives maps the release archives of Version to their SHA-256, copied
// from the release.
var archives = map[string]string{
	"slimctl-darwin-amd64.tar.gz":     "4d948671e50bea34963bb2586f05fd6a29948e79185c11ea671f6bddaaf74932",
	"slimctl-darwin-arm64.tar.gz":     "397152120d397f8b1ba229ebc20e2037f6e136307cc9725dfca83e07d7774579",
	"slimctl-linux-amd64-musl.tar.gz": "e3bcf95a36dcb51f9e8d0bfe3198f25eff475a25cb92592340355d205d219a86",
	"slimctl-linux-arm64-musl.tar.gz": "d6d65683de4dd19c914c103c805ca3e066b3ce6e0aca7e8ca2c4960fefedd339",
}

// Release is the pinned slimctl.
var Release = download.Release{
	Program:  "slimctl",
	Version:  Version,
	CacheDir: "slimctl-" + Version,
	Binary:   func(string) string { return "slimctl" },
	Archive:  archiveName,
	Archives: archives,
	URL: func(name string) string {
		return "https://github.com/agntcy/slim/releases/download/slimctl-" + Version + "/" + name
	},
}

// archiveName returns the release archive for goos and goarch. On Linux it
// is the static musl build, which runs on any distribution.
func archiveName(goos, goarch string) (string, error) {
	if goarch != "amd64" && goarch != "arm64" {
		return "", fmt.Errorf("no slimctl %s release for %s/%s", Version, goos, goarch)
	}
	switch goos {
	case "darwin":
		return "slimctl-darwin-" + goarch + ".tar.gz", nil
	case "linux":
		return "slimctl-linux-" + goarch + "-musl.tar.gz", nil
	}
	return "", fmt.Errorf("no slimctl %s release for %s/%s", Version, goos, goarch)
}

// NewFetcher returns a fetcher of the pinned slimctl for goos and goarch, or
// of override when it is set.
func NewFetcher(cacheDir, override string, hc *http.Client, goos, goarch string, logf func(string, ...any)) *download.Fetcher {
	return download.NewFetcher(Release, cacheDir, override, hc, goos, goarch, logf)
}

// CheckBinary checks that slimctl at path is the pinned version.
func CheckBinary(ctx context.Context, path string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "version").Output()
	if err != nil {
		return fmt.Errorf("running %s version: %w", path, err)
	}
	// It prints "Version:\t3.4.6" among other lines.
	for line := range strings.Lines(string(out)) {
		v, ok := strings.CutPrefix(strings.TrimSpace(line), "Version:")
		if !ok {
			continue
		}
		if v = "v" + strings.TrimSpace(v); v != Version {
			return fmt.Errorf("%s is slimctl %s; this a2a-serve runs %s", path, v, Version)
		}
		return nil
	}
	return fmt.Errorf("%s version printed no version", path)
}

// The SLIM name of an agent on the relay is org/namespace/<agent name>.
const (
	org       = "a2a"
	namespace = "relay"
)

// SLIMName returns the SLIM name of the agent named agent.
func SLIMName(agent string) string {
	return org + "/" + namespace + "/" + agent
}
