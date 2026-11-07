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

	"github.com/a2aproject/a2a-serve/internal/agentctl"
	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/clierr"
	"github.com/a2aproject/a2a-serve/internal/controlapi"
	"github.com/a2aproject/a2a-serve/internal/harness"
	"github.com/a2aproject/a2a-serve/internal/lifecycle"
	"github.com/a2aproject/a2a-serve/internal/wizard"
)

// serverFlags holds the agent directory and control timeout of the commands
// which manage an agent, and the time limit of the backup commands.
type serverFlags struct {
	dir      string
	timeout  time.Duration
	deadline time.Duration
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
	f.addTimeoutFlag(fs, "Timeout of each call to the running agent, a `duration` such as 30s or 2m")
}

// addTimeoutFlag adds --timeout with usage. A command that shadows the
// --timeout of its parent uses it to say what the timeout covers.
func (f *serverFlags) addTimeoutFlag(fs *pflag.FlagSet, usage string) {
	fs.DurationVar(&f.timeout, "timeout", controlapi.DefaultTimeout, usage)
}

// serveShort describes the command in its help and in `a2a plugin list`.
const serveShort = "Run a coding agent as an A2A server, or a local test server"

func newServeCmd(cfg *globalConfig) *cobra.Command {
	var f serverFlags
	cmd := &cobra.Command{
		Use:   "a2a-serve",
		Short: serveShort,
		Long: `Run a coding agent as an A2A server: one agent per process, started from its
project directory, sandboxed in Docker, reachable over A2A on localhost, a
private network (Tailscale, an Agent Mesh relay) and the internet
(Funnel, a relay), and from Telegram, Discord and email.

  a2a-serve start opencode --name coder     first run in a project: saves .a2a/config.yaml
  a2a-serve start                           later runs, from the project or a subdirectory

Management commands (pair, status, contact, ...) find the agent the way
git finds a repository, or take --dir.`,
		Version: buildVersionInfo().Version,
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
	}
	cmd.AddCommand(
		newStartCmd(cfg, &f),
		newStatusCmd(cfg, &f),
		newCardCmd(cfg, &f),
		newInspectCmd(cfg, &f),
		newLogsCmd(cfg, &f),
		newStopCmd(cfg, &f),
		newRestartCmd(cfg, &f),
		newConfigCmd(cfg, &f),
		newRecipeCmd(cfg, &f),
		newPairCmd(cfg, &f),
		newPeersCmd(cfg, &f),
		newContactCmd(cfg, &f),
		newScheduleCmd(cfg, &f),
		newMessengerCmd(cfg, &f),
		newBackupCmd(cfg, &f),
		newMailboxCmd(cfg),
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
	return f.lookupDir(agentdir.Dir.Exists, agentdir.Find)
}

// serverDir is [serverFlags.agentDir] that also finds an agent whose
// config.yaml is gone, from the state its server left, so that the server
// can still be found and stopped.
func (f *serverFlags) serverDir() (agentdir.Dir, error) {
	return f.lookupDir(func(d agentdir.Dir) bool { return d.Exists() || d.HasServerState() }, agentdir.FindWithState)
}

func (f *serverFlags) lookupDir(exists func(agentdir.Dir) bool, find func(string) (agentdir.Dir, error)) (agentdir.Dir, error) {
	if f.dir != "" {
		d, err := agentdir.Open(f.dir)
		if err != nil {
			return agentdir.Dir{}, err
		}
		if !exists(d) {
			return agentdir.Dir{}, agentdir.NotFoundIn(d.Root)
		}
		return d, nil
	}
	wd, err := os.Getwd()
	if err != nil {
		return agentdir.Dir{}, err
	}
	return find(wd)
}

// control returns a client for the running agent.
func (f *serverFlags) control() (*controlapi.Client, error) {
	d, err := f.agentDir()
	if err != nil {
		return nil, err
	}
	return f.controlClient(d), nil
}

// manager returns the [agentctl.Manager] of the agent, which closer closes.
func (f *serverFlags) manager(ctx context.Context) (_ agentctl.Manager, closer io.Closer, err error) {
	d, err := f.agentDir()
	if err != nil {
		return nil, nil, err
	}
	return agentctl.Open(ctx, d, f.controlClient(d))
}

// joinClose adds the error of closing c to *err.
func joinClose(err *error, c io.Closer) {
	*err = errors.Join(*err, c.Close())
}

// controlClient returns a client for the agent in d, with --timeout.
func (f *serverFlags) controlClient(d agentdir.Dir) *controlapi.Client {
	return controlapi.NewClient(d, controlapi.WithTimeout(f.timeout))
}

// runConfiguredAgent starts the agent saved in .a2a/config.yaml, or asks
// for its settings on the first run in a terminal.
func runConfiguredAgent(cmd *cobra.Command, cfg *globalConfig, f *serverFlags, m runMode) error {
	d, err := f.agentDir()
	if errors.Is(err, agentdir.ErrNotFound) && f.dir == "" {
		if !term.IsTerminal(int(os.Stdin.Fd())) {
			e := clierr.NoAgent(errors.New("no agent here"))
			e.Hint = "start one with `a2a-serve start <agent>` (" + strings.Join(harness.ProfileNames(), ", ") + "), or a test server with `a2a-serve start echo`, `proxy` or `exec`"
			return e
		}
		return runWizard(cmd, cfg, m.quiet)
	}
	if err != nil {
		return err
	}
	return runAgent(cmd.Context(), cfg, d, m)
}

func runWizard(cmd *cobra.Command, cfg *globalConfig, quiet bool) error {
	wd, err := os.Getwd()
	if err != nil {
		return err
	}
	d, err := agentdir.Open(wd)
	if err != nil {
		return err
	}
	_, dockerErr := harness.CheckDocker(cmd.Context())
	res, err := wizard.Run(os.Stdin, cfg.Out, wizard.Options{
		DefaultName: d.DefaultName(),
		Harnesses:   harness.ProfileNames(),
		Getenv:      os.Getenv,
		DockerErr:   dockerErr,
		ReadSecret:  cfg.secretReader(cfg.Out).Read,
		Workspace:   d.Root,
		Command: func(s agentdir.Settings) ([]string, error) {
			conf, secrets, err := d.ApplySettings(s)
			if err != nil {
				return nil, err
			}
			var lines []string
			for _, args := range conf.Commands(secrets) {
				lines = append(lines, agentdir.ShellJoin(args))
			}
			return lines, nil
		},
	})
	if err != nil {
		return err
	}
	if err := saveSettings(d, res.Settings); err != nil {
		return err
	}
	return runAgent(cmd.Context(), cfg, d, runMode{detach: res.Mode == wizard.ModeDetach, service: res.Mode == wizard.ModeService, quiet: quiet, created: true})
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

// runMode is how runAgent starts the agent. created reports that this run
// created the agent.
type runMode struct {
	detach, service, quiet, created bool
}

// runAgent runs the agent in the foreground, or starts it in the background.
func runAgent(ctx context.Context, cfg *globalConfig, d agentdir.Dir, m runMode) error {
	if err := printAgentLine(cfg.Out, d, m.created); err != nil {
		return err
	}
	if m.detach || m.service {
		mode := lifecycle.Detached
		if m.service {
			mode = lifecycle.Service
		}
		return lifecycle.Start(ctx, d, lifecycle.StartOptions{Mode: mode, Quiet: m.quiet, Out: cfg.Out})
	}
	ctx, stop := signalContext(ctx)
	defer stop()
	creds, err := cfg.credentialStore()
	if err != nil {
		slog.Warn("no credential store for the owner token of this machine", "error", err)
	}
	return lifecycle.Start(ctx, d, lifecycle.StartOptions{
		Out:         cfg.Out,
		Quiet:       m.quiet,
		Verbose:     cfg.verbose,
		Credentials: creds,
		Ready:       func() { printForegroundHint(cfg.Out) },
	})
}

// printAgentLine names the agent and the directory it runs from, since a
// run finds the nearest agent up from the current directory, and warns
// about keys of config.yaml that the server ignores and settings that leave
// the agent working as the user likely didn't mean. A detached or service
// child writes to the log, which its parent relays: there the lines would
// repeat the parent's.
func printAgentLine(out io.Writer, d agentdir.Dir, created bool) error {
	if writesTo(out, d.LogPath()) {
		return nil
	}
	conf, err := d.ReadConfig()
	if err != nil {
		return err
	}
	line := fmt.Sprintf("agent %s (%s) in %s", conf.Name, conf.Harness, d.Path())
	if created {
		line += " (created)"
	}
	keys, err := d.UnknownConfigKeys()
	if err != nil {
		return err
	}
	var sb strings.Builder
	sb.WriteString(line + "\n")
	for _, k := range keys {
		writeWarnings(&sb, []string{k.String()})
	}
	writeWarnings(&sb, harness.Warnings(conf))
	_, err = fmt.Fprint(out, sb.String())
	return err
}

// writesTo reports whether out is the file at path.
func writesTo(out io.Writer, path string) bool {
	f, ok := out.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	pi, err := os.Stat(path)
	return err == nil && os.SameFile(fi, pi)
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
