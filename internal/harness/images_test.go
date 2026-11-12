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
	"strings"
	"testing"

	"github.com/a2aproject/a2a-serve/internal/sandbox/sandboxtest"
)

// noImagesNoBuildDocker is sandboxtest.NoImagesDocker whose builds fail too.
const noImagesNoBuildDocker = `echo "fake docker: $1 failed" >&2
exit 1
`

func TestEnsureImage(t *testing.T) {
	tests := []struct {
		name    string
		docker  string
		image   string
		want    string
		wantErr string
	}{
		{name: "profile default", docker: sandboxtest.NoImagesDocker, image: opencode{}.Image(), want: LocalImage("opencode")},
		{name: "empty means the profile default", docker: sandboxtest.NoImagesDocker, want: LocalImage("opencode")},
		{name: "user image", docker: sandboxtest.NoImagesDocker, image: "registry.example/custom:1", wantErr: "docker pull registry.example/custom:1: fake docker: pull failed"},
		{name: "build fails", docker: noImagesNoBuildDocker, wantErr: "fake docker: pull failed"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sandboxtest.FakeDocker(t, tc.docker)
			var progress strings.Builder
			got, _, err := EnsureImage(context.Background(), tc.image, opencode{}, &progress)
			if strings.Contains(progress.String(), "pull failed") {
				t.Fatalf("EnsureImage() progress = %q, want no pull error", progress.String())
			}
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("EnsureImage() error = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("EnsureImage() error = %v", err)
			}
			if got != tc.want {
				t.Fatalf("EnsureImage() = %q, want %q", got, tc.want)
			}
		})
	}
}
