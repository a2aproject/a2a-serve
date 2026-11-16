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

package sandbox

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

// LocalBuild is an image to build on this machine when a pull fails.
type LocalBuild struct {
	Tag        string
	Dockerfile []byte
	// Reason says why the image is built, in what EnsureImage reports.
	Reason string
}

// EnsureImage makes image available to docker: it uses a local copy or
// pulls it. When the pull fails, it uses local, building it when it is
// missing. It returns the image to run and a line describing what it did.
func EnsureImage(ctx context.Context, image string, local *LocalBuild, progress io.Writer) (string, string, error) {
	if ImageExists(ctx, image) {
		return image, "using " + image, nil
	}
	pullErr := PullImage(ctx, image, progress)
	if pullErr == nil {
		return image, "pulled " + image, nil
	}
	if local == nil {
		return "", "", fmt.Errorf("docker pull %s: %w", image, pullErr)
	}
	if ImageExists(ctx, local.Tag) {
		return local.Tag, "using " + local.Tag, nil
	}
	build := exec.CommandContext(ctx, "docker", "build", "-t", local.Tag, "-")
	build.Stdin = bytes.NewReader(local.Dockerfile)
	build.Stdout, build.Stderr = progress, progress
	if err := build.Run(); err != nil {
		return "", "", fmt.Errorf("docker pull %s failed (%v), and building it locally failed: %w", image, pullErr, err)
	}
	what := "built " + local.Tag
	if local.Reason != "" {
		what += " (" + local.Reason + ")"
	}
	return local.Tag, what, nil
}

// PullImage pulls image, streaming docker's progress. It keeps docker's
// error output for the returned error, since an unpublished image fails
// the pull as expected before it is built.
func PullImage(ctx context.Context, image string, progress io.Writer) error {
	var stderr bytes.Buffer
	pull := exec.CommandContext(ctx, "docker", "pull", image)
	pull.Stdout, pull.Stderr = progress, &stderr
	err := pull.Run()
	if err == nil {
		return nil
	}
	if msg := strings.TrimSpace(stderr.String()); msg != "" {
		return fmt.Errorf("%s (%w)", msg, err)
	}
	return err
}

// ImageExists reports whether docker has image on this machine.
func ImageExists(ctx context.Context, image string) bool {
	return exec.CommandContext(ctx, "docker", "image", "inspect", image).Run() == nil
}
