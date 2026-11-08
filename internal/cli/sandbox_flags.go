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

package cli

import (
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/clierr"
	"github.com/a2aproject/a2a-serve/internal/controlapi"
)

// sandboxFlags are --storage and --secrets of `pair invite` and `pair set`,
// which set the sandbox rule of a pairing.
type sandboxFlags struct {
	storage string
	secrets []string
}

func (f *sandboxFlags) register(cmd *cobra.Command) {
	fs := cmd.Flags()
	fs.StringVar(&f.storage, "storage", "", "What the pairing's agent container gets of the workspace: shared, read-only, own (a private copy) or empty (default shared)")
	fs.StringSliceVar(&f.secrets, "secrets", nil, "Secrets the pairing's agent container gets: `names` from --secret, all, none, or NAME=secret:OTHER to pass the secret OTHER as NAME (default all)")
	must(cmd.RegisterFlagCompletionFunc("storage", fixedCompletions(agentdir.StorageModes...)))
}

// change returns the change of the sandbox rule the flags ask for, or nil.
func (f *sandboxFlags) change(cmd *cobra.Command) (*controlapi.SandboxChange, error) {
	storage, secrets := cmd.Flags().Changed("storage"), cmd.Flags().Changed("secrets")
	if !storage && !secrets {
		return nil, nil
	}
	ch := &controlapi.SandboxChange{}
	if storage {
		if !slices.Contains(agentdir.StorageModes, f.storage) {
			return nil, clierr.Usage("--storage must be one of shared, read-only, own or empty")
		}
		ch.Storage = f.storage
	}
	if secrets {
		var names []string
		for _, s := range f.secrets {
			if s = strings.TrimSpace(s); s != "" {
				names = append(names, s)
			}
		}
		if len(names) == 0 {
			return nil, clierr.Usage("--secrets needs names, all or none")
		}
		ch.Secrets = &names
	}
	return ch, nil
}
