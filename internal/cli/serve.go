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
	"github.com/a2aproject/a2a-serve/internal/launch"
	"github.com/a2aproject/a2a-serve/internal/schedule"
	"github.com/a2aproject/a2a-serve/internal/wizard"
)

// serverFlags holds the agent directory and control timeout of the commands
// which manage an agent.
type serverFlags struct {
	dir     string
	timeout time.Duration
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

  a2a serve run opencode --name coder     first run in a project: saves .a2a/config.yaml
  a2a serve run                           later runs, from the project or a subdirectory

Management commands (pair, status, contact, ...) find the agent the way
git finds a repository, or take --dir.`,
		Version: buildVersionInfo().Version,
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
	}
	cmd.AddCommand(
		newRunCmd(cfg, &f),
		newStatusCmd(cfg, &f),
		newURLCmd(cfg, &f),
		newOpenCmd(cfg, &f),
		newLogsCmd(cfg, &f),
		newStopCmd(cfg, &f),
		newRestartCmd(cfg, &f),
		newConfigCmd(cfg, &f),
		newPairCmd(cfg, &f),
		newPeersCmd(cfg, &f),
		newContactCmd(cfg, &f),
		newScheduleCmd(cfg, &f),
		newChannelCmd(cfg, &f),
		newBackupCmd(cfg, &f),
		newRestoreCmd(cfg),
		newMailboxCmd(cfg),
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

// manager changes what an agent keeps in its database: pairings, contacts
// and schedules. [controlapi.Client] does so through the running agent,
// and [agentctl.Offline] in the database of a stopped one.
type manager interface {
	Pair(ctx context.Context, req controlapi.PairCodeRequest) (*controlapi.PairCodeResponse, error)
	Pairings(ctx context.Context) ([]controlapi.Pairing, error)
	Pairing(ctx context.Context, name string) (*controlapi.Pairing, error)
	UpdatePairing(ctx context.Context, name string, req controlapi.PairingUpdate) (*controlapi.PairingUpdateResponse, error)
	Revoke(ctx context.Context, name string) (*controlapi.RevokeResponse, error)
	Contacts(ctx context.Context) ([]controlapi.ContactInfo, error)
	AddContact(ctx context.Context, req controlapi.ContactRequest) (*controlapi.ContactInfo, error)
	DeleteContact(ctx context.Context, name string) error
	Schedules(ctx context.Context) ([]controlapi.ScheduleInfo, error)
	AddSchedule(ctx context.Context, req schedule.Request) (*controlapi.ScheduleInfo, error)
	DeleteSchedule(ctx context.Context, name string) error
}

// manager returns the control client of the running agent, or else the
// database of the stopped agent, which done closes. offline tells which.
func (f *serverFlags) manager(ctx context.Context) (m manager, offline bool, done func() error, err error) {
	d, err := f.agentDir()
	if err != nil {
		return nil, false, nil, err
	}
	c := f.controlClient(d)
	_, err = c.Status(ctx)
	if err == nil {
		return c, false, func() error { return nil }, nil
	}
	if !errors.Is(err, controlapi.ErrNotRunning) {
		return nil, false, nil, err
	}
	o, err := agentctl.OpenOffline(d)
	if err != nil {
		return nil, false, nil, err
	}
	return o, true, o.Close, nil
}

// joinClose adds the error of done to *err.
func joinClose(err *error, done func() error) {
	*err = errors.Join(*err, done())
}

// controlContext returns ctx, in which the calls of agentctl to the running
// agent use --timeout.
func (f *serverFlags) controlContext(ctx context.Context) context.Context {
	return agentctl.WithControlOptions(ctx, controlapi.WithTimeout(f.timeout))
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
			return clierr.Usage("no agent here: start one with `a2a serve run <harness>` (" + strings.Join(harness.ProfileNames(), ", ") + "), or a test server with `a2a serve run echo`, `proxy` or `exec`")
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
	dockerCtx, cancel := context.WithTimeout(cmd.Context(), 10*time.Second)
	defer cancel()
	_, dockerErr := harness.CheckDocker(dockerCtx)
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

// printAgentLine names the agent and the directory it runs from, since a
// run finds the nearest agent up from the current directory. A detached or
// service child writes to the log, which its parent relays: there the line
// would repeat the parent's.
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
	_, err = fmt.Fprintln(out, line)
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
