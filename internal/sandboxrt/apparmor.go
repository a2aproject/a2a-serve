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
	"bufio"
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// LiveAppArmorProfile is the name of the AppArmor profile of [LiveAppArmor].
const LiveAppArmorProfile = "a2a-serve-live"

//go:embed a2a-serve-live.apparmor
var liveAppArmor string

// LiveAppArmor returns the source of the AppArmor profile for Docker
// containers with a live workspace, which apparmor_parser loads: Docker's
// default profile, which also allows the FUSE mount of the launcher.
func LiveAppArmor() string { return liveAppArmor }

// AppArmorDir is where the kernel shows its AppArmor policy.
const AppArmorDir = "/sys/kernel/security/apparmor"

// AppArmorLoaded reports whether the kernel whose AppArmor policy dir
// shows has loaded the profile name. It reads the names in the policy
// directory, which every user can read, or the list of profiles, which
// only root can, on kernels without the policy directory.
func AppArmorLoaded(dir, name string) (bool, error) {
	entries, err := os.ReadDir(filepath.Join(dir, "policy", "profiles"))
	if errors.Is(err, fs.ErrNotExist) {
		return listsProfile(filepath.Join(dir, "profiles"), name)
	}
	if err != nil {
		return false, err
	}
	for _, e := range entries {
		got, err := os.ReadFile(filepath.Join(dir, "policy", "profiles", e.Name(), "name"))
		if err != nil {
			return false, err
		}
		if strings.TrimSuffix(string(got), "\n") == name {
			return true, nil
		}
	}
	return false, nil
}

// listsProfile reports whether the list of profiles in file has name.
func listsProfile(file, name string) (bool, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return false, err
	}
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		// Each line is "<name> (<mode>)", and a name may have spaces.
		line := sc.Text()
		if i := strings.LastIndex(line, " ("); i >= 0 {
			line = line[:i]
		}
		if line == name {
			return true, nil
		}
	}
	return false, sc.Err()
}

// ErrLiveAppArmorNotLoaded is the error of [LiveDockerAppArmor] where
// Docker applies AppArmor on this machine and its kernel hasn't loaded
// [LiveAppArmorProfile].
var ErrLiveAppArmorNotLoaded = errors.New("the containers get an AppArmor profile, and the profile " + LiveAppArmorProfile + ", which allows the FUSE mount of a live workspace, isn't loaded")

// LiveDockerAppArmor returns the AppArmor profile of Docker containers
// with a live workspace: "" where Docker applies no AppArmor, and
// otherwise [LiveAppArmorProfile]. Where the daemon runs on this machine,
// it checks that the kernel whose policy dir shows has loaded the profile,
// and fails with [ErrLiveAppArmorNotLoaded] where it hasn't. A daemon on
// another machine or in a VM gets the profile unchecked, and refuses to
// start the containers where its kernel hasn't loaded it.
func LiveDockerAppArmor(ctx context.Context, dir string) (string, error) {
	applies, err := dockerAppArmor(ctx)
	if err != nil || !applies {
		return "", err
	}
	endpoint, err := DockerQuery(ctx, "context", "inspect", "--format", "{{.Endpoints.docker.Host}}")
	if err != nil {
		return "", err
	}
	if !bytes.HasPrefix(endpoint, []byte("unix://")) {
		return LiveAppArmorProfile, nil
	}
	loaded, err := AppArmorLoaded(dir, LiveAppArmorProfile)
	if errors.Is(err, fs.ErrNotExist) {
		// This machine shows no AppArmor policy, as where the daemon runs
		// in a VM or securityfs isn't mounted, so only the daemon can tell.
		return LiveAppArmorProfile, nil
	}
	if err != nil {
		return "", fmt.Errorf("checking whether the AppArmor profile %s is loaded: %w", LiveAppArmorProfile, err)
	}
	if !loaded {
		return "", ErrLiveAppArmorNotLoaded
	}
	return LiveAppArmorProfile, nil
}

// dockerAppArmor reports whether Docker applies AppArmor to its containers.
func dockerAppArmor(ctx context.Context) (bool, error) {
	out, err := DockerQuery(ctx, "info", "--format", "{{.SecurityOptions}}")
	if err != nil {
		return false, err
	}
	return bytes.Contains(out, []byte("name=apparmor")), nil
}
