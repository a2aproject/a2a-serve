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
	"context"
	"fmt"
	"os/exec"
	"runtime"
	"strings"

	"github.com/spf13/cobra"

	"github.com/a2aproject/a2a-serve/internal/clierr"
)

func newInspectCmd(cfg *globalConfig, sf *serverFlags) *cobra.Command {
	var (
		scope     string
		noBrowser bool
	)
	cmd := &cobra.Command{
		Use:   "inspect",
		Short: "Open the web inspector of the agent",
		Long: `Open a page where the owner sees what the agent is doing: its conversations
and tasks, pending approvals with buttons that answer them, and the state of
its messengers, pairings, webhook nodes and contacts.

The link logs in once and works for 5 minutes. The session lasts 12 hours. The
server keeps sessions in memory, so they end when it restarts.
The page is served on the local URL, and with --url private on the private
URL of a tailnet, whose traffic WireGuard encrypts. It is never served on a
relay or the public URL.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := checkInspectorScope(scope); err != nil {
				return clierr.Usage(err.Error())
			}
			c, err := sf.control()
			if err != nil {
				return err
			}
			st, err := c.Status(cmd.Context())
			if err != nil {
				return err
			}
			base, err := inspectorURL(st, scope)
			if err != nil {
				return clierr.Usage(err.Error())
			}
			login, err := c.InspectorLogin(cmd.Context())
			if err != nil {
				return err
			}
			link := strings.TrimSuffix(base, "/") + login.Path
			if cfg.IsJSON() {
				return cfg.PrintJSON(map[string]string{"url": link})
			}
			opened := !noBrowser && openBrowser(cmd.Context(), link) == nil
			msg := "→ open this link once to log in (valid for 5 minutes):\n  %s\n"
			if opened {
				msg = "✓ opened the inspector in your browser; if it didn't open, use this link once (valid for 5 minutes):\n  %s\n"
			}
			_, err = fmt.Fprintf(cfg.Out, msg, link)
			return err
		},
	}
	f := cmd.Flags()
	f.StringVar(&scope, "url", scopeLocal, "Open the inspector at the agent's `scope` URL: local, or private on a tailnet")
	must(cmd.RegisterFlagCompletionFunc("url", fixedCompletions(scopeLocal, scopePrivate)))
	f.BoolVar(&noBrowser, "no-browser", false, "Print the link without opening a browser")
	sf.addControlFlags(f)
	return cmd
}

// openBrowser opens url in the default browser.
func openBrowser(ctx context.Context, url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.CommandContext(ctx, "open", url)
	case "windows":
		cmd = exec.CommandContext(ctx, "rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.CommandContext(ctx, "xdg-open", url)
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}
