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

package meshbackend

import (
	"context"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"time"

	"github.com/a2aproject/a2a-serve/internal/agentmesh"
	"github.com/a2aproject/a2a-serve/internal/download"
)

// BinaryEnv names an agentmesh-one to run in place of the pinned release,
// such as the one in the relay image.
const BinaryEnv = "A2A_SERVE_AGENTMESH_ONE"

// Release is the pinned agentmesh-one.
var Release = download.Release{
	Program:  "agentmesh-one",
	Version:  agentmesh.Version,
	CacheDir: "agentmesh-" + agentmesh.Version,
	Binary: func(goos string) string {
		if goos == "windows" {
			return "agentmesh-one.exe"
		}
		return "agentmesh-one"
	},
	Archive:  agentmesh.ArchiveName,
	Archives: agentmesh.Archives,
	URL:      agentmesh.ArchiveURL,
}

// NewFetcher returns a fetcher of the pinned agentmesh-one for goos and
// goarch, or of override when it is set.
func NewFetcher(cacheDir, override string, hc *http.Client, goos, goarch string, logf func(string, ...any)) *download.Fetcher {
	return download.NewFetcher(Release, cacheDir, override, hc, goos, goarch, logf)
}

// BinaryVersion returns the version agentmesh-one at path reports.
func BinaryVersion(ctx context.Context, path string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "--version").Output()
	if err != nil {
		return "", fmt.Errorf("running %s --version: %w", path, err)
	}
	// It prints "agentmesh-one version v0.2.0".
	fields := strings.Fields(string(out))
	if len(fields) == 0 {
		return "", fmt.Errorf("%s --version printed nothing", path)
	}
	return fields[len(fields)-1], nil
}

// CheckBinary checks that agentmesh-one at path is the pinned version.
func CheckBinary(ctx context.Context, path string) error {
	v, err := BinaryVersion(ctx, path)
	if err != nil {
		return err
	}
	if v != agentmesh.Version {
		return fmt.Errorf("%s is Agent Mesh %s; this a2a-serve runs %s", path, v, agentmesh.Version)
	}
	return nil
}
