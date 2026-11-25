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

package agentprofile

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/a2aproject/a2a-serve/internal/sandboxrt/sandboxtest"
)

// noImagesNoBuildDocker is sandboxtest.NoImagesDocker whose builds fail too.
const noImagesNoBuildDocker = `echo "fake docker: $1 failed" >&2
exit 1
`

func TestEnsureImage(t *testing.T) {
	tests := []struct {
		name     string
		docker   string
		profile  Profile
		image    string
		want     string
		wantWhat string
		wantErr  string
		wantPull bool
	}{
		{name: "profile default", docker: sandboxtest.NoImagesDocker, profile: opencode{}, image: opencode{}.Image(), want: LocalImage("opencode"), wantPull: true},
		{name: "empty means the profile default", docker: sandboxtest.NoImagesDocker, profile: opencode{}, want: LocalImage("opencode"), wantPull: true},
		{name: "user image", docker: sandboxtest.NoImagesDocker, profile: opencode{}, image: "registry.example/custom:1", wantErr: "docker pull registry.example/custom:1: fake docker: pull failed", wantPull: true},
		{name: "build fails", docker: noImagesNoBuildDocker, profile: opencode{}, wantErr: "fake docker: pull failed", wantPull: true},
		{name: "unpublished profile image is built without a pull", docker: sandboxtest.NoImagesDocker, profile: claude{}, want: LocalImage("claude"), wantWhat: "built " + LocalImage("claude")},
		{name: "unpublished profile image build fails", docker: noImagesNoBuildDocker, profile: claude{}, wantErr: "building " + LocalImage("claude") + ": exit status 1"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			log := sandboxtest.FakeDocker(t, tc.docker)
			var progress strings.Builder
			got, what, err := EnsureImage(context.Background(), tc.image, tc.profile, &progress)
			calls, readErr := os.ReadFile(log)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if pulled := strings.Contains(string(calls), "pull "); pulled != tc.wantPull {
				t.Fatalf("EnsureImage() ran docker pull = %v, want %v; calls:\n%s", pulled, tc.wantPull, calls)
			}
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
			if tc.wantWhat != "" && what != tc.wantWhat {
				t.Fatalf("EnsureImage() what = %q, want %q", what, tc.wantWhat)
			}
		})
	}
}
