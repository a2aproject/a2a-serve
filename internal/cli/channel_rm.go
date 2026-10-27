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
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/clierr"
	"github.com/a2aproject/a2a-serve/internal/controlapi"
)

func newChannelRmCmd(cfg *globalConfig, sf *serverFlags) *cobra.Command {
	return &cobra.Command{
		Use:       "rm <telegram|discord|email>",
		Short:     "Remove a messenger",
		Args:      cobra.ExactArgs(1),
		ValidArgs: agentdir.ChannelKinds,
		RunE: func(cmd *cobra.Command, args []string) error {
			d, err := sf.agentDir()
			if err != nil {
				return err
			}
			err = sf.controlClient(d).DeleteChannel(cmd.Context(), args[0])
			if errors.Is(err, controlapi.ErrNotRunning) {
				err = removeChannelOffline(d, args[0])
			}
			if err != nil {
				return err
			}
			return cfg.PrintRemoved("channel", args[0], "✓ removed "+args[0])
		},
	}
}

func removeChannelOffline(d agentdir.Dir, kind string) error {
	conf, err := d.LoadConfig()
	if err != nil {
		return err
	}
	had := conf.HasChannel(kind)
	if err := conf.SetChannel(kind, "", nil); err != nil {
		return clierr.Usage(err.Error())
	}
	if !had {
		return fmt.Errorf("%s is not configured", kind)
	}
	return d.SaveConfig(conf)
}
