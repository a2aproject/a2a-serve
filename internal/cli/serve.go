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
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"golang.org/x/term"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/clierr"
	"github.com/a2aproject/a2a-serve/internal/controlapi"
	"github.com/a2aproject/a2a-serve/internal/harness"
	"github.com/a2aproject/a2a-serve/internal/launch"
	"github.com/a2aproject/a2a-serve/internal/wizard"
)

// serverFlags holds the flags of `a2a serve` itself, and the agent
// directory and control timeout of the commands which manage an agent.
type serverFlags struct {
	dir     string
	timeout time.Duration
	detach  bool
	service bool
	quiet   bool

	// legacyEcho, legacyProxy and legacyExec are the flags which the echo,
	// proxy and script commands replaced.
	legacyEcho  bool
	legacyProxy string
	legacyExec  string
}

const dirUsage = "Agent directory (default: the nearest directory with .a2a, walking up from the current one)"

// addDirFlag adds --dir to a command which uses an agent directory.
func (f *serverFlags) addDirFlag(fs *pflag.FlagSet) {
	fs.StringVar(&f.dir, "dir", "", dirUsage)
}

// addControlFlags adds --dir and --timeout to a command which calls the
// running agent.
func (f *serverFlags) addControlFlags(fs *pflag.FlagSet) {
	f.addDirFlag(fs)
	fs.DurationVar(&f.timeout, "timeout", controlapi.DefaultTimeout, "Timeout of each call to the running agent")
}

// serveShort describes the command in its help and in `a2a plugin list`.
const serveShort = "Run a coding agent as an A2A server, or a local test server"

func newServeCmd(cfg *globalConfig) *cobra.Command {
	var f serverFlags
	cmd := &cobra.Command{
		Use:   "serve",
		Short: serveShort,
		Long: `Run a coding agent as an A2A server: one agent per process, started from its
project directory, sandboxed in Docker, reachable over A2A on localhost, a
private network (Tailscale, LAN) and the internet (Funnel, ngrok), and from
Telegram, Discord and email.

  a2a serve opencode --name coder     first run in a project: saves .a2a/config.yaml
  a2a serve                           later runs, from the project or a subdirectory

Management commands (pair, status, contact, ...) find the agent the way
git finds a repository, or take --dir.

echo, proxy and script run a local test server instead.`,
		Version: buildVersionInfo().Version,
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := f.checkLegacyTestServer(); err != nil {
				return err
			}
			return runConfiguredAgent(cmd, cfg, &f)
		},
	}
	fl := cmd.Flags()
	f.addDirFlag(fl)
	fl.BoolVar(&f.detach, "detach", false, "Run the agent in the background")
	fl.BoolVar(&f.service, "service", false, "Install and start the agent as a launchd or systemd user service")
	fl.BoolVar(&f.quiet, "quiet", false, "Keep the harness image pull and build output in the log instead of printing it")
	fl.BoolVar(&f.legacyEcho, "echo", false, "Replaced by `a2a serve echo`")
	fl.StringVar(&f.legacyProxy, "proxy", "", "Replaced by `a2a serve proxy <agent-url>`")
	fl.StringVar(&f.legacyExec, "exec", "", "Replaced by `a2a serve script <command>`")
	for _, name := range []string{"echo", "proxy", "exec"} {
		must(fl.MarkHidden(name))
	}

	for _, name := range harness.ProfileNames() {
		cmd.AddCommand(newHarnessCmd(cfg, &f, name))
	}
	cmd.AddCommand(
		newHarnessCmd(cfg, &f, "acp"),
		newEchoCmd(cfg),
		newProxyCmd(cfg),
		newScriptCmd(cfg),
		newStatusCmd(cfg, &f),
		newURLCmd(cfg, &f),
		newLogsCmd(cfg, &f),
		newStopCmd(cfg, &f),
		newRestartCmd(cfg, &f),
		newConfigCmd(cfg, &f),
		newPairCmd(cfg, &f),
		newTokensCmd(cfg),
		newPeersCmd(cfg, &f),
		newContactCmd(cfg, &f),
		newChannelCmd(cfg, &f),
		newRelayCmd(cfg),
		newDoctorCmd(cfg, &f),
		newCredentialCmd(cfg),
		newVersionCmd(cfg),
		newCompletionCmd(),
	)
	return cmd
}

// agentDir returns the agent of --dir, or the nearest one up from the cwd.
func (f *serverFlags) agentDir() (agentdir.Dir, error) {
	if f.dir != "" {
		d, err := agentdir.Open(f.dir)
		if err != nil {
			return agentdir.Dir{}, err
		}
		if !d.Exists() {
			return agentdir.Dir{}, agentdir.NotFoundIn(d.Root)
		}
		return d, nil
	}
	wd, err := os.Getwd()
	if err != nil {
		return agentdir.Dir{}, err
	}
	return agentdir.Find(wd)
}

// control returns a client for the running agent.
func (f *serverFlags) control() (*controlapi.Client, error) {
	d, err := f.agentDir()
	if err != nil {
		return nil, err
	}
	return f.controlClient(d), nil
}

// controlClient returns a client for the agent in d, with --timeout.
func (f *serverFlags) controlClient(d agentdir.Dir) *controlapi.Client {
	return controlapi.NewClient(d, controlapi.WithTimeout(f.timeout))
}

// runConfiguredAgent starts the agent saved in .a2a/config.yaml, or asks
// for its settings on the first run in a terminal.
func runConfiguredAgent(cmd *cobra.Command, cfg *globalConfig, f *serverFlags) error {
	d, err := f.agentDir()
	if errors.Is(err, agentdir.ErrNotFound) && f.dir == "" {
		if !term.IsTerminal(int(os.Stdin.Fd())) {
			return clierr.Usage("no agent here: start one with `a2a serve <harness>` (" + strings.Join(harness.ProfileNames(), ", ") + "), or a test server with `a2a serve echo`, `proxy` or `script`")
		}
		return runWizard(cmd, cfg)
	}
	if err != nil {
		return err
	}
	return runAgent(cmd.Context(), cfg, d, runMode{detach: f.detach, service: f.service, quiet: f.quiet})
}

func runWizard(cmd *cobra.Command, cfg *globalConfig) error {
	wd, err := os.Getwd()
	if err != nil {
		return err
	}
	d, err := agentdir.Open(wd)
	if err != nil {
		return err
	}
	dockerCtx, cancel := context.WithTimeout(cmd.Context(), 10*time.Second)
	defer cancel()
	_, dockerErr := harness.CheckDocker(dockerCtx)
	res, err := wizard.Run(os.Stdin, cfg.Out, wizard.Options{
		DefaultName: d.DefaultName(),
		Harnesses:   harness.ProfileNames(),
		Getenv:      os.Getenv,
		DockerErr:   dockerErr,
		ReadSecret:  newSecretReader(cfg.Out).read,
		Command: func(s agentdir.Settings) (string, error) {
			conf, secrets, err := d.ApplySettings(s)
			if err != nil {
				return "", err
			}
			return agentdir.ShellJoin(conf.AsCommand(secrets)), nil
		},
	})
	if err != nil {
		return err
	}
	if err := saveSettings(d, res.Settings); err != nil {
		return err
	}
	return runAgent(cmd.Context(), cfg, d, runMode{detach: res.Mode == wizard.ModeDetach, service: res.Mode == wizard.ModeService})
}

// saveSettings saves start flags, reporting invalid ones as usage errors.
func saveSettings(d agentdir.Dir, s agentdir.Settings) error {
	err := d.SaveSettings(s)
	var se *agentdir.SettingsError
	if errors.As(err, &se) {
		return clierr.Usage(se.Error())
	}
	return err
}

// runMode is how runAgent starts the agent.
type runMode struct {
	detach, service, quiet bool
}

// runAgent runs the agent in the foreground, or starts it in the background.
func runAgent(ctx context.Context, cfg *globalConfig, d agentdir.Dir, m runMode) error {
	if m.detach || m.service {
		return launch.Background(ctx, d, m.service, m.quiet, cfg.Out)
	}
	ctx, stop := signalContext(ctx)
	defer stop()
	creds, err := cfg.credentialStore()
	if err != nil {
		slog.Warn("no credential store for the owner token of this machine", "error", err)
	}
	return launch.Foreground(ctx, d, cfg.Out, cfg.verbose, m.quiet, creds, func() { printForegroundHint(cfg.Out) })
}

// printForegroundHint tells someone watching the terminal how to stop the
// agent. A detached or service child writes to the log, which the parent
// relays: there the hint would contradict how it was started.
func printForegroundHint(out io.Writer) {
	if f, ok := out.(*os.File); !ok || !term.IsTerminal(int(f.Fd())) {
		return
	}
	if _, err := fmt.Fprintln(out, "  (running in the foreground; Ctrl-C to stop, or start with --detach)"); err != nil {
		slog.Debug("writing progress", "error", err)
	}
}

func signalContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
}

// checkLegacyTestServer rejects --echo, --proxy and --exec, naming the
// command which replaced them.
func (f *serverFlags) checkLegacyTestServer() error {
	var flag, replacement string
	switch {
	case f.legacyEcho:
		flag, replacement = "--echo", "a2a serve echo"
	case f.legacyProxy != "":
		flag, replacement = "--proxy", "a2a serve proxy "+agentdir.ShellJoin([]string{f.legacyProxy})
	case f.legacyExec != "":
		flag, replacement = "--exec", "a2a serve script "+agentdir.ShellJoin([]string{f.legacyExec})
	default:
		return nil
	}
	return clierr.Usage(fmt.Sprintf("%s was replaced by a command: run `%s`", flag, replacement))
}
