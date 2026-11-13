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

// Package agentmesh pins the Agent Mesh release that agents join relays with
// and that relays run, and checks the URLs agents join relays at.
package agentmesh

import (
	"fmt"
	"strconv"
	"strings"
)

// Version is the Agent Mesh version that relays run and the Agent Mesh
// network provider joins them with. It equals the version of
// github.com/google/agentmesh in providers/agentmesh/go.mod.
const Version = "v0.2.0"

// Image is the agentmesh-one image of Version, by digest, which the relay
// image is built from.
const Image = "ghcr.io/google/agentmesh-one:v0.2.0@sha256:2d462256b7da24c29dd8477343d7032cc1e4e55a97d335117faedf5438666ca3"

// Archives maps the release archives of Version to their SHA-256. The
// release's checksums.txt is unsigned, so the hashes are copied here when
// the pin changes.
var Archives = map[string]string{
	"agentmesh_Darwin_arm64.tar.gz":  "cad60616e6348d40ddd56f34fed5454981d68088ca21598140a93590ad3ed105",
	"agentmesh_Darwin_x86_64.tar.gz": "a05faf19c69da32a3d7ed7086ca5ed1c31a7c096bd5d4f2cc14200df2271c7ce",
	"agentmesh_Linux_arm64.tar.gz":   "563ae09105de5b0b38c27452a5225d84bc73264708e6de054416a3af7a47a913",
	"agentmesh_Linux_x86_64.tar.gz":  "447b58a9257966f61ccc4957f0a28825dc8a30d48c81ac7acfe01228a68dd3d7",
	"agentmesh_Windows_arm64.zip":    "39f939dab302afa16d92ad9464982a8dd0d61012c1818c587903c6bd3f1d9860",
	"agentmesh_Windows_x86_64.zip":   "5239a1179a55e3e974ba3899a72d59ec0f195115eead7d94ec92cecea1f2f659",
}

// ArchiveName returns the name of the release archive for goos and goarch.
func ArchiveName(goos, goarch string) (string, error) {
	osName := map[string]string{"linux": "Linux", "darwin": "Darwin", "windows": "Windows"}[goos]
	arch := map[string]string{"amd64": "x86_64", "arm64": "arm64"}[goarch]
	if osName == "" || arch == "" {
		return "", fmt.Errorf("no Agent Mesh %s release for %s/%s", Version, goos, goarch)
	}
	ext := ".tar.gz"
	if goos == "windows" {
		ext = ".zip"
	}
	return "agentmesh_" + osName + "_" + arch + ext, nil
}

// ArchiveURL returns where the release archive name is downloaded from.
func ArchiveURL(name string) string {
	return "https://github.com/google/agentmesh/releases/download/" + Version + "/" + name
}

// SemVer is a parsed vMAJOR.MINOR.PATCH version.
type SemVer struct{ Major, Minor, Patch int }

// Parse parses a version such as v0.2.0.
func Parse(v string) (SemVer, error) {
	parts := strings.Split(strings.TrimPrefix(v, "v"), ".")
	if len(parts) != 3 {
		return SemVer{}, fmt.Errorf("invalid Agent Mesh version %q", v)
	}
	var n [3]int
	for i, p := range parts {
		x, err := strconv.Atoi(p)
		if err != nil || x < 0 {
			return SemVer{}, fmt.Errorf("invalid Agent Mesh version %q", v)
		}
		n[i] = x
	}
	return SemVer{n[0], n[1], n[2]}, nil
}

// Compare returns -1, 0 or 1 as s is older than, equal to or newer than o.
func (s SemVer) Compare(o SemVer) int {
	for _, d := range [3]int{s.Major - o.Major, s.Minor - o.Minor, s.Patch - o.Patch} {
		switch {
		case d < 0:
			return -1
		case d > 0:
			return 1
		}
	}
	return 0
}

// Compatible reports whether agents on Version can join a relay that runs
// v. Agent Mesh allows breaking changes between 0.x minor versions, so the
// major and minor versions must match.
func Compatible(v string) (bool, error) {
	got, err := Parse(v)
	if err != nil {
		return false, err
	}
	want, err := Parse(Version)
	if err != nil {
		return false, err
	}
	return got.Major == want.Major && got.Minor == want.Minor, nil
}

// Info is what an a2a-serve relay that runs Agent Mesh reports at its info
// path.
type Info struct {
	A2AServe  string `json:"a2a_serve"`
	AgentMesh string `json:"agentmesh"`
	// SignIn reports whether people can sign in to the relay.
	SignIn bool `json:"signin"`
}
