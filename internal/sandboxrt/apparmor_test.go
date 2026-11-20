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

package sandboxrt

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

func TestAppArmorLoaded(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		// policy are the names of the profiles in the policy directory,
		// which has none when it is nil, and list is the list of profiles.
		policy  []string
		list    string
		want    bool
		wantErr bool
	}{
		{name: "in the policy directory", policy: []string{"docker-default", LiveAppArmorProfile}, want: true},
		{name: "not in the policy directory", policy: []string{"docker-default", LiveAppArmorProfile + "-old"}},
		{name: "empty policy directory", policy: []string{}, list: LiveAppArmorProfile + " (enforce)\n"},
		{name: "in the list", list: "docker-default (enforce)\n" + LiveAppArmorProfile + " (enforce)\n", want: true},
		{name: "in the list in complain mode", list: LiveAppArmorProfile + " (complain)\n", want: true},
		{name: "a longer name in the list", list: LiveAppArmorProfile + "-old (enforce)\n"},
		{name: "a name with spaces in the list", list: LiveAppArmorProfile + " x (enforce)\n"},
		{name: "no AppArmor", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			if tc.policy != nil {
				if err := os.MkdirAll(filepath.Join(dir, "policy", "profiles"), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			for i, name := range tc.policy {
				writeFile(t, filepath.Join(dir, "policy", "profiles", name+"."+strconv.Itoa(i), "name"), name+"\n")
			}
			if tc.list != "" {
				writeFile(t, filepath.Join(dir, "profiles"), tc.list)
			}
			got, err := AppArmorLoaded(dir, LiveAppArmorProfile)
			if (err != nil) != tc.wantErr {
				t.Fatalf("AppArmorLoaded() error = %v, want error %v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Fatalf("AppArmorLoaded() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestLiveAppArmorAllowsOnlyTheFUSEMount checks the one change to Docker's
// default profile: the mount rule of the launcher's FUSE mount.
func TestLiveAppArmorAllowsOnlyTheFUSEMount(t *testing.T) {
	t.Parallel()
	profile := LiveAppArmor()
	mounts := regexp.MustCompile(`(?m)^\s*(deny\s+)?(mount|remount|pivot_root)\b.*$`).FindAllString(profile, -1)
	want := "mount fstype=fuse.a2a options=(rw, nosuid, nodev) a2a-workspace -> /**,"
	if len(mounts) != 1 || strings.TrimSpace(mounts[0]) != want {
		t.Fatalf("LiveAppArmor() mount rules = %q, want only %q", mounts, want)
	}
	if !strings.Contains(profile, `profile "`+LiveAppArmorProfile+`" `) {
		t.Fatalf("LiveAppArmor() doesn't declare the profile %q", LiveAppArmorProfile)
	}
	if _, err := exec.LookPath("apparmor_parser"); err != nil {
		t.Skip("needs apparmor_parser to parse the profile")
	}
	if _, err := os.Stat("/etc/apparmor.d/abstractions/base"); err != nil {
		t.Skip("needs the AppArmor abstractions that the profile includes")
	}
	parse := exec.Command("apparmor_parser", "--skip-kernel-load", "--skip-cache", "--names")
	parse.Stdin = strings.NewReader(profile)
	out, err := parse.CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != LiveAppArmorProfile {
		t.Fatalf("apparmor_parser --names = %q, %v, want %q", out, err, LiveAppArmorProfile)
	}
}
