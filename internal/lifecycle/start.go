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

// Package lifecycle starts, stops and restarts a server, in the
// foreground, in the background or as a user service, and follows its log.
package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/agentsrv"
	"github.com/a2aproject/a2a-serve/internal/clierr"
	"github.com/a2aproject/a2a-serve/internal/controlapi"
	"github.com/a2aproject/a2a-serve/internal/credstore"
	"github.com/a2aproject/a2a-serve/internal/doctor"
	"github.com/a2aproject/a2a-serve/internal/fsutil"
	"github.com/a2aproject/a2a-serve/internal/googleauth"
	"github.com/a2aproject/a2a-serve/internal/logfile"
	"github.com/a2aproject/a2a-serve/internal/service"
)

// maxLogSize is the size at which the server log is rotated.
const maxLogSize = logfile.DefaultLimit

// Mode is how [Start] runs the agent.
type Mode int

const (
	// Foreground runs the agent until the context of [Start] ends.
	Foreground Mode = iota
	// Detached starts the agent detached from the terminal.
	Detached
	// Service installs and starts the agent as a user service.
	Service
)

// StartOptions configure [Start].
type StartOptions struct {
	Mode Mode
	// Out receives progress, including the output of pulling or building
	// the harness image.
	Out io.Writer
	// Quiet keeps the image output in the log only.
	Quiet bool
	// Verbose also writes logs and the harness's stderr to stderr in the
	// foreground.
	Verbose bool
	// Credentials, if set, receives an owner token for the local URL in the
	// foreground.
	Credentials *credstore.Store
	// Ready, if set, is called once the agent serves in the foreground.
	Ready func()
}

// Start runs the agent of dir. In the foreground it runs until ctx ends;
// detached or as a service it returns once the agent serves, and an agent
// which already serves is left running.
func Start(ctx context.Context, dir agentdir.Dir, opts StartOptions) error {
	if opts.Mode == Foreground {
		return foreground(ctx, dir, opts)
	}
	return background(ctx, dir, opts.Mode == Service, opts.Quiet, opts.Out)
}

// foreground logs to state/server.log, and also to stderr when verbose.
func foreground(ctx context.Context, dir agentdir.Dir, opts StartOptions) error {
	cfg, err := dir.LoadConfig()
	if err != nil {
		return err
	}
	secrets, err := dir.LoadSecrets()
	if err != nil {
		return err
	}
	if err := dir.CheckConfig(cfg, secrets); err != nil {
		return err
	}
	if err := dir.Init(); err != nil {
		return err
	}
	// A second start must fail before it touches the running agent's log.
	if err := checkNotRunning(dir); err != nil {
		return err
	}
	if err := logfile.Rotate(dir.LogPath(), maxLogSize, os.Stdout, os.Stderr); err != nil {
		return err
	}
	logFile, err := logfile.Open(dir.LogPath(), maxLogSize, os.Stdout, os.Stderr)
	if err != nil {
		return err
	}
	defer func() {
		if err := logFile.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "closing log: %v\n", err)
		}
	}()
	var logOut io.Writer = logFile
	level := slog.LevelInfo
	if opts.Verbose {
		logOut = io.MultiWriter(logFile, os.Stderr)
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(logOut, &slog.HandlerOptions{Level: level})))
	slog.Info("starting the server", "name", cfg.Name, "agent", cfg.Harness, "dir", dir.Root)
	srvOpts := agentsrv.Options{Dir: dir, Config: cfg, Secrets: secrets, Out: opts.Out, HarnessLog: logOut, ImageProgress: opts.Out, Credentials: opts.Credentials, SetupProblems: doctor.SetupProblems, SharedGoogleClientID: googleauth.SharedClientID}
	if opts.Quiet {
		lw := &logLines{msg: "agent image"}
		defer lw.flush()
		srvOpts.ImageProgress = lw
	}
	if ready := opts.Ready; ready != nil {
		srvOpts.Ready = func(*agentsrv.Server) { ready() }
	}
	if err := agentsrv.Run(ctx, srvOpts); err != nil {
		slog.Error(stopMsg, "error", err)
		return err
	}
	slog.Info("the server stopped")
	return nil
}

// checkNotRunning fails if a server holds the lock of dir. The lock
// is released at once: the server takes it again when it starts.
func checkNotRunning(dir agentdir.Dir) error {
	release, err := dir.Lock()
	if errors.Is(err, fsutil.ErrLocked) {
		return fmt.Errorf("the server is already running in %s (a2a-serve status)", dir.Root)
	}
	if err != nil {
		return fmt.Errorf("locking the agent directory: %w", err)
	}
	return release()
}

// background starts the agent detached from the terminal, or as a user
// service when asService, and relays its progress to out until it serves.
func background(ctx context.Context, dir agentdir.Dir, asService, quiet bool, out io.Writer) error {
	cfg, err := dir.LoadConfig()
	if err != nil {
		return err
	}
	secrets, err := dir.LoadSecrets()
	if err != nil {
		return err
	}
	if err := dir.CheckConfig(cfg, secrets); err != nil {
		return err
	}
	if err := dir.Init(); err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if _, err := controlapi.NewClient(dir).Status(ctx); err == nil {
		_, err := fmt.Fprintf(out, "✓ %s is already running (a2a-serve status)\n", cfg.Name)
		return err
	}
	if err := checkNotRunning(dir); err != nil {
		return err
	}
	// The child's stdout is the log, so it is rotated before the child opens it.
	if err := logfile.Rotate(dir.LogPath(), maxLogSize); err != nil {
		return err
	}
	var offset int64
	if info, err := os.Stat(dir.LogPath()); err == nil {
		offset = info.Size()
	}
	args := []string{"start", "--dir", dir.Root}
	if quiet {
		args = append(args, "--quiet")
	}
	u := service.Unit{
		Name:        dir.ServiceName(),
		Description: fmt.Sprintf("a2a-serve %s (%s)", cfg.Name, dir.Root),
		Exe:         exe,
		Args:        args,
		Dir:         dir.Root,
		LogPath:     dir.LogPath(),
		Env:         []string{"PATH=" + os.Getenv("PATH")},
	}
	var note string
	alive := func() bool { return true }
	if asService {
		if note, err = service.Install(u); err != nil {
			return err
		}
	} else {
		exited, err := service.Detach(u)
		if err != nil {
			return err
		}
		alive = func() bool {
			select {
			case <-exited:
				return false
			default:
				return true
			}
		}
	}
	if err := waitReady(ctx, dir, offset, alive, out); err != nil {
		if asService {
			return fmt.Errorf("%w\n  the service stays installed; remove it with `a2a-serve stop --uninstall`", err)
		}
		return err
	}
	msg := "✓ running in the background (a2a-serve logs, a2a-serve stop)\n"
	if asService {
		msg = "✓ running as a user service (a2a-serve logs)\n"
		if note != "" {
			msg += "  " + note + "\n"
		}
	}
	_, err = io.WriteString(out, msg)
	return err
}

// waitReady relays progress lines from the log until the control socket
// answers or the process dies. The error output of a server that exits is
// held back and returned, so it is reported once.
func waitReady(ctx context.Context, dir agentdir.Dir, offset int64, alive func() bool, out io.Writer) error {
	deadline := time.Now().Add(15 * time.Minute)
	client := controlapi.NewClient(dir)
	r := relay{out: out}
	for {
		next, err := r.read(dir.LogPath(), offset)
		if err != nil {
			return err
		}
		offset = next
		sctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		_, statusErr := client.Status(sctx)
		cancel()
		if statusErr == nil {
			// Print what the server logged while becoming ready.
			if _, err := r.read(dir.LogPath(), offset); err != nil {
				slog.Debug("reading the startup log", "error", err)
				return nil
			}
			return r.flush()
		}
		// A service manager restarts a server that failed, so the process
		// may still be alive.
		if r.stopped || !alive() {
			if _, err := r.read(dir.LogPath(), offset); err != nil {
				slog.Debug("reading the log of the exited server", "error", err)
			}
			return r.exitError()
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the server did not start in time; see %s", dir.LogPath())
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(300 * time.Millisecond):
		}
	}
}

// relay prints the progress lines of a starting server and keeps what it
// needs to report the server's failure.
type relay struct {
	out io.Writer
	// tail is the last log records, which aren't printed.
	tail []string
	// held is the error output the server printed last, such as
	// "Error: ..." and "hint: ...".
	held []string
	// stopErr is the error of the server's stop record.
	stopErr string
	stopped bool
}

// read relays the lines of the log at path after offset, and returns the
// offset after them.
func (r *relay) read(path string, offset int64) (int64, error) {
	lines, next, err := readFrom(path, offset)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return offset, err
	}
	for _, l := range lines {
		if err := r.line(l); err != nil {
			return next, err
		}
	}
	return next, nil
}

func (r *relay) line(l string) error {
	if !isProgress(l) {
		r.tail = append(r.tail, l)
		if len(r.tail) > 20 {
			r.tail = r.tail[1:]
		}
		if isStop(l) {
			r.stopped, r.stopErr = true, logAttr(l, "error")
		}
		return nil
	}
	if strings.HasPrefix(l, errorPrefix) || len(r.held) > 0 && strings.HasPrefix(l, hintPrefix) {
		r.held = append(r.held, l)
		return nil
	}
	if err := r.flush(); err != nil {
		return err
	}
	_, err := fmt.Fprintln(r.out, l)
	return err
}

// flush prints held lines that turned out to be progress.
func (r *relay) flush() error {
	for _, l := range r.held {
		if _, err := fmt.Fprintln(r.out, l); err != nil {
			return err
		}
	}
	r.held = nil
	return nil
}

// errorPrefix and hintPrefix start the lines the CLI prints for an error.
const (
	errorPrefix = "Error: "
	hintPrefix  = "hint: "
)

// exitError returns the error of a server that exited.
func (r *relay) exitError() error {
	if len(r.held) > 0 {
		e := clierr.Wrap(clierr.CodeInternal, errors.New("the server exited: "+strings.TrimPrefix(r.held[0], errorPrefix)))
		if len(r.held) > 1 {
			e.Hint = strings.TrimPrefix(r.held[len(r.held)-1], hintPrefix)
		}
		return e
	}
	if r.stopErr != "" {
		return errors.New("the server exited: " + r.stopErr)
	}
	if len(r.tail) == 0 {
		return errors.New("the server exited; see `a2a-serve logs`")
	}
	return fmt.Errorf("the server exited:\n%s", strings.Join(r.tail, "\n"))
}
