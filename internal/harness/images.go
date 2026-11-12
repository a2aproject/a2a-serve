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

package harness

import (
	"context"
	"embed"
	"fmt"
	"io"
	"strings"

	"github.com/a2aproject/a2a-serve/internal/sandbox"
)

//go:embed images/*/Dockerfile
var dockerfiles embed.FS

// Dockerfile returns the embedded Dockerfile of a harness image.
func Dockerfile(harness string) ([]byte, bool) {
	data, err := dockerfiles.ReadFile("images/" + harness + "/Dockerfile")
	return data, err == nil
}

// LocalImage is the tag of a harness image built on this machine.
func LocalImage(harness string) string {
	return "a2a-harness-" + harness + ":" + ImageTag
}

// EnsureImage makes image, or the profile's image when empty, available to
// docker: it uses a local copy or pulls it. Only the profile's own image
// falls back to building the harness's embedded Dockerfile when the pull
// fails. It returns the image to run and a line describing what it did.
func EnsureImage(ctx context.Context, image string, profile Profile, progress io.Writer) (string, string, error) {
	harness := profile.Name()
	if image == "" {
		image = profile.Image()
	}
	if image == "" {
		return "", "", fmt.Errorf("agent %q has no container image; pass --image or --sandbox local", harness)
	}
	var local *sandbox.LocalBuild
	if df, ok := Dockerfile(harness); ok && image == profile.Image() {
		local = &sandbox.LocalBuild{Tag: LocalImage(harness), Dockerfile: df, Reason: strings.TrimPrefix(image, ImageRepo) + " is not published"}
	}
	return sandbox.EnsureImage(ctx, image, local, progress)
}
