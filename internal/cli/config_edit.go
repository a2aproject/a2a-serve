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
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/google/shlex"
	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/a2aproject/a2a-serve/internal/agentctl"
	"github.com/a2aproject/a2a-serve/internal/clierr"
)

func newConfigEditCmd(cfg *globalConfig, sf *serverFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "edit",
		Short: "Edit config.yaml in $EDITOR, validating it on save",
		Long: `Open a copy of .a2a/config.yaml in $VISUAL or $EDITOR (default vi). On save
it is validated and applied like ` + "`a2a-serve config set`" + `; if it is invalid,
the file reopens with the errors on top, like crontab -e. Secret values
aren't in config.yaml: store them with ` + "`a2a-serve config set --secret NAME`" + `.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			editor, err := editorCommand(os.Getenv, term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd())))
			if err != nil {
				return err
			}
			d, err := sf.agentDir()
			if err != nil {
				return err
			}
			res, err := agentctl.Edit(cmd.Context(), d, sf.controlClient(d), func(path string) error {
				c := exec.CommandContext(cmd.Context(), editor[0], append(editor[1:], path)...)
				c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
				if err := c.Run(); err != nil {
					return fmt.Errorf("running the editor %s: %w", editor[0], err)
				}
				return nil
			}, func(invalid error) (bool, error) {
				return askEditAgain(cfg, invalid)
			})
			if err != nil {
				return err
			}
			return printConfigResult(cfg, res)
		},
	}
	markTextOnly(cmd)
	return cmd
}

// editorCommand returns the editor to run: $VISUAL, $EDITOR or vi. Without
// a terminal the default vi would wait for input forever, so it needs an
// editor which doesn't.
func editorCommand(getenv func(string) string, terminal bool) ([]string, error) {
	editor := getenv("VISUAL")
	if editor == "" {
		editor = getenv("EDITOR")
	}
	if editor == "" && !terminal {
		return nil, clierr.Usage("no terminal to run an editor in; set $VISUAL or $EDITOR, or change settings with `a2a-serve config set`")
	}
	if editor == "" {
		editor = "vi"
	}
	argv, err := shlex.Split(editor)
	if err != nil || len(argv) == 0 {
		return nil, clierr.Usage(fmt.Sprintf("invalid editor %q in $VISUAL or $EDITOR", editor))
	}
	return argv, nil
}

// askEditAgain prints why the edit was rejected and, on a terminal, asks
// whether to edit again.
func askEditAgain(cfg *globalConfig, invalid error) (bool, error) {
	if _, err := fmt.Fprintf(cfg.stderr(), "✗ invalid config:\n  %s\n", strings.ReplaceAll(invalid.Error(), "\n", "\n  ")); err != nil {
		return false, err
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return false, nil
	}
	if _, err := fmt.Fprint(cfg.stderr(), "Edit again? [Y/n] "); err != nil {
		return false, err
	}
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		return false, err
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "" || answer == "y" || answer == "yes", nil
}
