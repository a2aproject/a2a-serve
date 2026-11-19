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

package a2aserve

import (
	"path/filepath"
	"slices"

	"github.com/a2aproject/a2a-serve/internal/agentprofile"
)

// Profile is a coding agent that can be served: its command, its image, and
// how its home is prepared. The zero value is invalid; get one from
// [Opencode], [Claude], [Command] or [LookupProfile].
type Profile struct {
	profile agentprofile.Profile
}

// Opencode returns the profile of opencode.
func Opencode() Profile { return Profile{agentprofile.Opencode()} }

// Claude returns the profile of Claude Code.
func Claude() Profile { return Profile{agentprofile.Claude()} }

// Command returns the profile of any agent that speaks ACP on stdio when
// started as argv. It has no image. Without argv it returns the invalid
// zero Profile.
func Command(argv ...string) Profile {
	if len(argv) == 0 {
		return Profile{}
	}
	return Profile{agentprofile.Custom(filepath.Base(argv[0]), argv)}
}

// LookupProfile returns the profile named name. A non-empty argv replaces
// the command line of the profile, keeping its image and home setup, and
// allows an agent that has no profile.
func LookupProfile(name string, argv []string) (Profile, error) {
	p, err := agentprofile.Lookup(name, slices.Clone(argv))
	if err != nil {
		return Profile{}, err
	}
	return Profile{p}, nil
}

// ProfileNames lists the names of the known profiles, sorted.
func ProfileNames() []string { return agentprofile.ProfileNames() }

// Name returns the name of the agent, such as "opencode".
func (p Profile) Name() string {
	if p.profile == nil {
		return ""
	}
	return p.profile.Name()
}

// Image returns the default container image of the agent, or "" for one
// without an image.
func (p Profile) Image() string {
	if p.profile == nil {
		return ""
	}
	return p.profile.Image()
}

// Argv returns the command line that starts the agent in ACP mode. Model
// selects the model for an agent whose command line carries it, which none
// of the known agents do.
func (p Profile) Argv(model string) []string {
	if p.profile == nil {
		return nil
	}
	cmd := p.profile.Command(agentprofile.Workspace{})
	return append([]string{cmd.Path}, cmd.Args...)
}
