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
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/a2aproject/a2a-serve/internal/clierr"
	"github.com/a2aproject/a2a-serve/internal/flagparse"
	"github.com/a2aproject/a2a-serve/internal/googleauth"
	"github.com/a2aproject/a2a-serve/internal/logfile"
	"github.com/a2aproject/a2a-serve/internal/relay"
	"github.com/a2aproject/a2a-serve/internal/relay/control"
	"github.com/a2aproject/a2a-serve/internal/relay/meshbackend"
	"github.com/a2aproject/a2a-serve/internal/relaydir"
	"github.com/a2aproject/a2a-serve/internal/service"
)

func newRelayRunCmd(cfg *globalConfig, f *relayFlags) *cobra.Command {
	var (
		s          relaydir.Settings
		runService bool
	)
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Run the relay of this directory, creating it on the first run",
		Long: `Run a relay: one process that serves HTTP on --listen and runs Agent Mesh's
agentmesh-one behind it on loopback, at the version this a2a-serve pins. The
first run creates the relay directory here and downloads agentmesh-one.

The relay needs an address that doesn't change, since agents and pairing
tokens are tied to it. Serve it behind a reverse proxy or a tunnel that ends
TLS, and give that address as --url. --tunnel cloudflare runs a Cloudflare
quick tunnel for a demo; its URL changes on every start.

People sign in to the relay from their agents with Google, limited to
--allow-email, once you register a Google app for the relay and give it with
--google-client-id and --google-client-secret. Agents that can't sign in
join with ` + "`a2a-serve relay invite`" + `.

The flags are saved in .a2a/relay.yaml, so later runs need none. A newer
a2a-serve that pins a newer Agent Mesh backs up the relay's database first,
and an older one refuses to run on it.

` + flagparse.SecretHelp,
		Example: `  a2a-serve relay run --url https://relay.example.com
  a2a-serve relay run --url https://relay.example.com --allow-email '*@example.com' \
    --google-client-id <id> --google-client-secret env:GOOGLE_CLIENT_SECRET
  a2a-serve relay run --tunnel cloudflare
  a2a-serve relay run --service`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) (err error) {
			d, err := relayDirForRun(f.dir)
			if err != nil {
				return err
			}
			release, err := lockRelay(d)
			if err != nil {
				return err
			}
			defer func() { err = errors.Join(err, release()) }()
			if err := saveRelaySettings(cmd, cfg, d, s); err != nil {
				return err
			}
			bin, err := agentmeshOne(cmd.Context(), cfg)
			if err != nil {
				return err
			}
			if runService {
				if err := release(); err != nil {
					return err
				}
				return installRelayService(cmd.Context(), cfg, d)
			}
			return runRelay(cmd.Context(), cfg, d, bin, release)
		},
	}
	fs := cmd.Flags()
	fs.StringVar(&f.dir, "dir", "", "Relay directory (default: the nearest relay directory up from the current one, or else the current directory)")
	fs.StringVar(&s.URL, "url", "", "The relay's address, which agents join: https://..., or http:// on loopback")
	fs.StringVar(&s.Listen, "listen", "", "Address to serve HTTP on (default "+relaydir.DefaultListen+")")
	fs.StringVar(&s.Tunnel, "tunnel", "", "Publish the relay through a Cloudflare quick tunnel (cloudflare, or none); needs cloudflared on PATH")
	fs.StringVar(&s.AdminToken, "admin-token", "", "The relay's admin token, at least 32 characters, as a "+flagparse.SecretSyntax+" (default: generated on the first run)")
	fs.StringVar(&s.Database, "database", "", "A PostgreSQL DSN, as a "+flagparse.SecretSyntax+" (default: SQLite in the relay directory; none goes back to it)")
	fs.StringSliceVar(&s.AllowEmails, "allow-email", nil, "People who may sign in to the relay with Google: an `email`, or *@domain for the accounts of a Google Workspace domain; replaces the saved list, and none turns sign-in off and forgets the Google app (repeatable; commas also separate values)")
	fs.StringVar(&s.GoogleClientID, "google-client-id", "", "Client ID of the relay's Google app, which people sign in with (none removes the app)")
	fs.StringVar(&s.GoogleClientSecret, "google-client-secret", "", "Client secret of the relay's Google app, as a "+flagparse.SecretSyntax)
	fs.BoolVar(&runService, "service", false, "Install and start the relay as a launchd or systemd user service")
	must(cmd.RegisterFlagCompletionFunc("tunnel", fixedCompletions(relaydir.TunnelCloudflare, "none")))
	return cmd
}

// relayDirForRun returns the relay of --dir, the nearest one up from the
// cwd, or a new one in the cwd.
func relayDirForRun(dir string) (relaydir.Dir, error) {
	if dir != "" {
		return relaydir.Open(dir)
	}
	wd, err := os.Getwd()
	if err != nil {
		return relaydir.Dir{}, err
	}
	d, err := relaydir.Find(wd)
	if errors.Is(err, relaydir.ErrNotFound) {
		return relaydir.Open(wd)
	}
	return d, err
}

// lockRelay creates the relay directory d if needed and takes the lock that
// the running relay holds. The returned release may be called again.
func lockRelay(d relaydir.Dir) (release func() error, err error) {
	if err := d.Init(); err != nil {
		return nil, err
	}
	unlock, err := d.Lock()
	if errors.Is(err, relaydir.ErrRunning) {
		return nil, fmt.Errorf("the relay in %s is already running; see `a2a-serve relay status`", d.Root)
	}
	if err != nil {
		return nil, err
	}
	var once sync.Once
	return func() error {
		var err error
		once.Do(func() { err = unlock() })
		return err
	}, nil
}

// saveRelaySettings saves the flags of `relay run`, with the relay's lock
// held. Agents and pairing tokens name the relay's URL, so changing it on a
// relay that has run asks first.
func saveRelaySettings(cmd *cobra.Command, cfg *globalConfig, d relaydir.Dir, s relaydir.Settings) error {
	if s.URL != "" && d.Exists() {
		conf, err := d.ReadConfig()
		if err != nil {
			return err
		}
		rec, err := d.RecordedVersion()
		if err != nil {
			return err
		}
		if rec != "" && conf.URL != "" && strings.TrimSuffix(s.URL, "/") != conf.URL {
			if err := confirmURLChange(cfg, conf.URL, s.URL); err != nil {
				return err
			}
		}
	}
	for _, v := range []struct {
		flag string
		dst  *string
	}{
		{"admin-token", &s.AdminToken},
		{"database", &s.Database},
		{"google-client-secret", &s.GoogleClientSecret},
	} {
		if *v.dst != flagparse.SecretStdin {
			continue
		}
		secret, err := cfg.secretReader(cfg.stderr()).Read("--" + v.flag)
		if err != nil {
			return err
		}
		*v.dst = secret
	}
	_, err := d.SaveSettings(s)
	return err
}

func confirmURLChange(cfg *globalConfig, from, to string) error {
	msg := fmt.Sprintf("the relay's URL is %s; agents and pairing tokens name it, so after a change to %s every agent has to join again", from, to)
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return clierr.Usage(msg + "; change it from a terminal, which asks to confirm")
	}
	if _, err := fmt.Fprintf(cfg.stderr(), "%s.\nChange it? [y/N] ", msg); err != nil {
		return err
	}
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
		return errors.New("the relay's URL is unchanged")
	}
	return nil
}

// agentmeshOnePath returns where the pinned agentmesh-one is, or is
// downloaded to.
func agentmeshOnePath() (string, error) {
	f, err := newFetcher(nil, nil)
	if err != nil {
		return "", err
	}
	return f.Location(), nil
}

func newFetcher(hc *http.Client, logf func(string, ...any)) (*meshbackend.Fetcher, error) {
	cache, err := os.UserCacheDir()
	if err != nil {
		return nil, err
	}
	return meshbackend.NewFetcher(filepath.Join(cache, "a2a-serve"), os.Getenv(meshbackend.BinaryEnv), hc, runtime.GOOS, runtime.GOARCH, logf), nil
}

// agentmeshOne returns the pinned agentmesh-one, downloading it if needed.
func agentmeshOne(ctx context.Context, cfg *globalConfig) (string, error) {
	hc := &http.Client{Timeout: 10 * time.Minute, Transport: cfg.httpTransport}
	f, err := newFetcher(hc, func(format string, args ...any) {
		if _, err := fmt.Fprintf(cfg.progress(), format+"\n", args...); err != nil {
			slog.Debug("writing progress", "error", err)
		}
	})
	if err != nil {
		return "", err
	}
	bin, err := f.Get(ctx)
	if err != nil {
		return "", err
	}
	if err := meshbackend.CheckBinary(ctx, bin); err != nil {
		return "", err
	}
	return bin, nil
}

// runRelay runs the relay in the foreground, with its lock held until
// release. When `relay upgrade` asks it to restart, it runs the a2a-serve
// binary on disk in its place.
func runRelay(ctx context.Context, cfg *globalConfig, d relaydir.Dir, bin string, release func() error) error {
	conf, err := d.ReadConfig()
	if err != nil {
		return err
	}
	var cloudflared string
	if conf.Tunnel == relaydir.TunnelCloudflare {
		if cloudflared, err = exec.LookPath("cloudflared"); err != nil {
			return errors.New("--tunnel cloudflare needs cloudflared on PATH; install it from Cloudflare's downloads page")
		}
	}
	meshLog, closeLog, err := relayMeshLog(cfg, d)
	if err != nil {
		return err
	}
	defer closeLog()
	ctx, stop := signalContext(ctx)
	defer stop()
	err = relay.New(relay.Options{
		Dir: d, Backend: meshbackend.New(meshbackend.Options{Binary: bin, Log: meshLog, Now: time.Now}),
		Cloudflared: cloudflared, A2AServe: buildVersionInfo().Version, Google: googleauth.Google,
		Out: cfg.Out, TunnelLog: meshLog, Now: time.Now,
	}).Run(ctx)
	if !errors.Is(err, relay.ErrRestart) {
		return err
	}
	closeLog()
	if err := release(); err != nil {
		return err
	}
	return relay.Reexec(d.Root)
}

// relayMeshLog returns where the output of agentmesh-one goes: the relay's
// log, rotated, when someone watches the terminal or a service manager
// writes stderr to the log, and stderr otherwise, which Cloud Run collects.
func relayMeshLog(cfg *globalConfig, d relaydir.Dir) (io.Writer, func(), error) {
	toLog := writesTo(os.Stderr, d.LogPath())
	if out, ok := cfg.Out.(*os.File); !toLog && (cfg.verbose || !ok || !term.IsTerminal(int(out.Fd()))) {
		return cfg.stderr(), func() {}, nil
	}
	if err := logfile.Rotate(d.LogPath(), logfile.DefaultLimit, os.Stdout, os.Stderr); err != nil {
		return nil, nil, err
	}
	log, err := logfile.Open(d.LogPath(), logfile.DefaultLimit, os.Stdout, os.Stderr)
	if err != nil {
		return nil, nil, err
	}
	if !toLog {
		if _, err := fmt.Fprintf(cfg.Out, "  (Agent Mesh logs to %s)\n", d.LogPath()); err != nil {
			return nil, nil, errors.Join(err, log.Close())
		}
	}
	var once sync.Once
	return log, func() {
		once.Do(func() {
			if err := log.Close(); err != nil {
				slog.Debug("closing the relay log", "error", err)
			}
		})
	}, nil
}

// installRelayService installs the relay as a user service and waits until
// it answers on its control socket.
func installRelayService(ctx context.Context, cfg *globalConfig, d relaydir.Dir) error {
	exe, err := service.Executable()
	if err != nil {
		return err
	}
	env := []string{"PATH=" + os.Getenv("PATH")}
	if v := os.Getenv(meshbackend.BinaryEnv); v != "" {
		env = append(env, meshbackend.BinaryEnv+"="+v)
	}
	note, err := service.Install(service.Unit{
		Name:        d.ServiceName(),
		Description: "a2a-serve relay (" + d.Root + ")",
		Exe:         exe,
		Args:        []string{"relay", "run", "--dir", d.Root},
		Dir:         d.Root,
		LogPath:     d.LogPath(),
		Env:         env,
	})
	if err != nil {
		return err
	}
	c := control.NewSocketClient(d.SocketPath(), 5*time.Second)
	deadline := time.Now().Add(3 * time.Minute)
	for {
		st, err := c.Status(ctx)
		if err == nil {
			msg := fmt.Sprintf("relay %s\n✓ running as a user service (logs in %s)\n", st.URL, d.LogPath())
			if note != "" {
				msg += "  " + note + "\n"
			}
			_, err = io.WriteString(cfg.Out, msg)
			return err
		}
		if !isRelayNotRunning(err) || time.Now().After(deadline) {
			return fmt.Errorf("the relay service didn't start: %w; see %s", err, d.LogPath())
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}
