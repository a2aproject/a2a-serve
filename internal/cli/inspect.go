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

	"github.com/a2aproject/a2a-serve/internal/controlapi"
)

func newInspectCmd(cfg *globalConfig, sf *serverFlags) *cobra.Command {
	var private, noBrowser bool
	cmd := &cobra.Command{
		Use:   "inspect",
		Short: "Open the web inspector of the agent",
		Long: `Open a page where the owner sees what the agent is doing: its conversations
and tasks, pending approvals with Approve and Deny, and the state of its
messengers, pairings, webhook nodes and contacts.

The link logs in once and works for 5 minutes; the session lasts 12 hours.
The page is served on the local URL, and with --private on the private
network, never on the public URL.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := sf.control()
			if err != nil {
				return err
			}
			st, err := c.Status(cmd.Context())
			if err != nil {
				return err
			}
			scope := controlapi.ScopeLocal
			if private {
				scope = controlapi.ScopePrivate
			}
			base, err := st.URL(scope)
			if err != nil {
				return err
			}
			if private && st.PrivateNote != "" {
				if _, err := fmt.Fprintf(cfg.stderr(), "note: a browser can't open this private URL (%s); run a2a-serve inspect without --private\n", st.Private); err != nil {
					return err
				}
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
	f.BoolVar(&private, "private", false, "Use the agent's URL on the private network")
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
