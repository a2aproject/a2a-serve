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

package agentmesh

import (
	"os"
	"strings"
	"testing"

	"golang.org/x/mod/modfile"
)

func TestVersionMatchesGoMod(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile("../../go.mod")
	if err != nil {
		t.Fatalf("os.ReadFile() error = %v", err)
	}
	f, err := modfile.Parse("go.mod", data, nil)
	if err != nil {
		t.Fatalf("modfile.Parse() error = %v", err)
	}
	for _, r := range f.Require {
		if r.Mod.Path == "github.com/google/agentmesh" {
			if r.Mod.Version != Version {
				t.Fatalf("go.mod requires agentmesh %s, want Version %s", r.Mod.Version, Version)
			}
			return
		}
	}
	t.Fatal("go.mod doesn't require github.com/google/agentmesh")
}

func TestPinCoversEveryArchiveAndTheImage(t *testing.T) {
	t.Parallel()
	for _, goos := range []string{"linux", "darwin", "windows"} {
		for _, goarch := range []string{"amd64", "arm64"} {
			name, err := ArchiveName(goos, goarch)
			if err != nil {
				t.Fatalf("ArchiveName(%s, %s) error = %v", goos, goarch, err)
			}
			if len(Archives[name]) != 64 {
				t.Fatalf("Archives[%s] = %q, want a SHA-256", name, Archives[name])
			}
		}
	}
	if !strings.Contains(Image, ":"+Version+"@sha256:") {
		t.Fatalf("Image = %s, want the tag %s and a digest", Image, Version)
	}
}

func TestCompatible(t *testing.T) {
	t.Parallel()
	tests := []struct {
		version string
		want    bool
		wantErr bool
	}{
		{version: Version, want: true},
		{version: "v0.2.7", want: true},
		{version: "v0.3.0", want: false},
		{version: "v1.2.0", want: false},
		{version: "0.2", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.version, func(t *testing.T) {
			t.Parallel()
			got, err := Compatible(tt.version)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Compatible(%q) error = %v, want error %v", tt.version, err, tt.wantErr)
			}
			if got != tt.want {
				t.Fatalf("Compatible(%q) = %v, want %v", tt.version, got, tt.want)
			}
		})
	}
}

func TestRelayImageUsesThePinnedImage(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile("../relay/image/Dockerfile")
	if err != nil {
		t.Fatalf("os.ReadFile() error = %v", err)
	}
	if !strings.Contains(string(data), "\nFROM "+Image+" AS agentmesh\n") {
		t.Fatalf("the relay image doesn't build from %s", Image)
	}
}
