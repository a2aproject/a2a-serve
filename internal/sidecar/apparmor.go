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

package sidecar

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/a2aproject/a2a-serve/internal/sandboxrt"
	"github.com/a2aproject/a2a-serve/internal/sidecar/serve"
)

const appArmorLong = `Print the AppArmor profile that lets rootful Docker mount a live workspace.

Where Docker applies AppArmor, such as rootful Docker on Ubuntu, its default
profile denies the FUSE mount of a live workspace. This profile is Docker's
default with one change: it allows that mount. Once it is loaded,
a2a-serve-sidecar gives it to the containers with a live workspace.
Load it on the machine where the Docker daemon runs, and again after each
reboot.`

func newAppArmorCmd(std IO) *cobra.Command {
	return &cobra.Command{
		Use:     "apparmor-profile",
		Short:   "Print the AppArmor profile of live workspaces in Docker",
		Long:    appArmorLong,
		Example: "a2a-serve-sidecar apparmor-profile | sudo apparmor_parser -r",
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) > 0 {
				return &serve.UsageError{Err: fmt.Errorf("apparmor-profile takes no arguments, got %q", args[0])}
			}
			return nil
		},
		RunE: func(*cobra.Command, []string) error {
			_, err := fmt.Fprint(std.Stdout, sandboxrt.LiveAppArmor())
			return err
		},
	}
}
