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

// Package doctor checks the setup of this machine and of an agent: the a2a
// CLI and its config, the sandbox, the agent's config and secrets, the
// running server and its network, and applies the local fixes it can.
package doctor

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/controlapi"
	"github.com/a2aproject/a2a-serve/internal/credstore"
	"github.com/a2aproject/a2a-serve/internal/harness"
	"github.com/a2aproject/a2a-serve/internal/messenger/configured"
	"github.com/a2aproject/a2a-serve/internal/netinstall"
	"github.com/a2aproject/a2a-serve/internal/relay/control"
	"github.com/a2aproject/a2a-serve/internal/relaydir"
	"github.com/a2aproject/a2a-serve/internal/sandboxnet"
	"github.com/a2aproject/a2a-serve/internal/service"
)

// Status is the outcome of a check.
type Status string

// Check outcomes. Only [StatusFail] makes doctor exit non-zero.
const (
	StatusOK   Status = "ok"
	StatusWarn Status = "warn"
	StatusFail Status = "fail"
	StatusSkip Status = "skip"
)

// Groups of checks.
const (
	GroupCLI        = "a2a CLI"
	GroupSandbox    = "Sandbox"
	GroupNetwork    = "Network"
	GroupMessengers = "Messengers"
)

// Result is the outcome of one check.
type Result struct {
	ID      string `json:"id"`
	Status  Status `json:"status"`
	Message string `json:"message"`
	// Fix is the command, or the action, which fixes a failure or a warning.
	Fix string `json:"fix"`
	// Group is the heading the result is listed under.
	Group string `json:"-"`
	// Detail explains a failure in the text output.
	Detail string `json:"-"`

	apply func() (string, error)
}

// Fixable reports whether --fix can fix the result.
func (r Result) Fixable() bool {
	return r.apply != nil && (r.Status == StatusFail || r.Status == StatusWarn)
}

// Apply applies the local fix of the result and describes what it did.
func (r Result) Apply() (string, error) {
	if r.apply == nil {
		return "", fmt.Errorf("%s has no automatic fix", r.ID)
	}
	return r.apply()
}

// Env is what the checks read from the machine. Nil functions of the
// optional dependencies skip the checks which need them.
type Env struct {
	// Home is the home directory of the a2a CLI config.
	Home string
	// Credentials holds the owner token of this machine.
	Credentials *credstore.Store
	LookupEnv   func(string) (string, bool)
	LookPath    func(string) (string, error)
	Executable  func() (string, error)
	// DirWritable reports whether this user can write files into a
	// directory. Optional: without it no directory counts as writable.
	DirWritable func(dir string) bool
	// CheckLauncher checks that Docker can run the a2a-serve binary at a
	// path as the launcher of containers, as [harness.CheckLauncher] does.
	CheckLauncher func(exe string) (string, error)
	// CheckTUN checks the host's TUN device, as [harness.CheckTUN] does.
	CheckTUN func() error
	// CheckInternetRoute checks that this host routes to the internet, as
	// [sandboxnet.CheckInternetRoute] does.
	CheckInternetRoute func() error
	// Run runs a command and returns its stdout.
	Run  func(ctx context.Context, name string, args ...string) ([]byte, error)
	HTTP *http.Client
	// FindNetworkProvider finds the binary of a network provider without
	// downloading it, as [netinstall.Installer.Find] does. Optional.
	FindNetworkProvider func(spec string) (netinstall.Provider, error)
	// Status returns the status of the running agent, or
	// [agenterr.ErrNotRunning].
	Status func(context.Context, agentdir.Dir) (*controlapi.Status, error)
	// PortFree reports whether a localhost port can be listened on.
	PortFree func(port int) bool
	// ServiceUnit returns the path of the agent's installed service unit,
	// or "" if none is installed. Optional.
	ServiceUnit func(agentdir.Dir) (string, error)
	// ValidateMessenger checks the credentials of a messenger. Optional.
	ValidateMessenger func(ctx context.Context, cfg *agentdir.Config, secrets agentdir.Secrets, kind string) error
	// RelayStatus returns the status of the running relay, or
	// [control.ErrNotRunning]. Optional.
	RelayStatus func(context.Context, relaydir.Dir) (*control.Status, error)
}

// System returns the environment of this process.
func System() (Env, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Env{}, fmt.Errorf("resolving home directory: %w", err)
	}
	return Env{
		Home:               home,
		Credentials:        credstore.At(home),
		LookupEnv:          os.LookupEnv,
		LookPath:           exec.LookPath,
		Executable:         os.Executable,
		DirWritable:        dirWritable,
		CheckLauncher:      harness.CheckLauncher,
		CheckTUN:           harness.CheckTUN,
		CheckInternetRoute: sandboxnet.CheckInternetRoute,
		Run:                runCommand,
		HTTP:               &http.Client{Timeout: 5 * time.Second},
		Status: func(ctx context.Context, d agentdir.Dir) (*controlapi.Status, error) {
			return controlapi.NewClient(d).Status(ctx)
		},
		PortFree:          portFree,
		ServiceUnit:       serviceUnit,
		ValidateMessenger: configured.Validate,
		FindNetworkProvider: func(spec string) (netinstall.Provider, error) {
			inst, err := netinstall.New(nil)
			if err != nil {
				return netinstall.Provider{}, err
			}
			return inst.Find(spec)
		},
		RelayStatus: func(ctx context.Context, d relaydir.Dir) (*control.Status, error) {
			return control.NewSocketClient(d.SocketPath(), 5*time.Second).Status(ctx)
		},
	}, nil
}

func dirWritable(dir string) bool {
	f, err := os.CreateTemp(dir, ".a2a-serve-doctor-*")
	if err != nil {
		return false
	}
	name := f.Name()
	if err := f.Close(); err != nil {
		slog.Warn("closing the probe file", "path", name, "err", err)
	}
	if err := os.Remove(name); err != nil {
		slog.Warn("removing the probe file", "path", name, "err", err)
	}
	return true
}

func runCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return out, fmt.Errorf("%w: %s", err, msg)
		}
		return out, err
	}
	return out, nil
}

func portFree(port int) bool {
	l, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return false
	}
	if err := l.Close(); err != nil {
		slog.Debug("closing the port probe", "error", err)
	}
	return true
}

func serviceUnit(d agentdir.Dir) (string, error) {
	name := d.ServiceName()
	if !service.Installed(name) {
		return "", nil
	}
	return service.UnitFile(name)
}

// Options select the checks of [Run].
type Options struct {
	// Dir is the agent to check, or nil to check only the machine.
	Dir *agentdir.Dir
	// Online adds checks which call external services.
	Online bool
}

// Run runs the checks of the machine, and of the agent in opts.Dir.
func Run(ctx context.Context, env Env, opts Options) []Result {
	results := cliChecks(ctx, env)
	var cfg *agentdir.Config
	var agent []Result
	if opts.Dir != nil {
		agent, cfg = agentChecks(ctx, env, *opts.Dir, opts.Online)
	}
	results = append(results, sandboxChecks(ctx, env, opts.Dir, cfg, opts.Online)...)
	results = append(results, agent...)
	if cfg != nil {
		results = append(results, networkChecks(*opts.Dir, cfg)...)
		if opts.Online {
			results = append(results, messengerChecks(ctx, env, *opts.Dir, cfg)...)
		}
	}
	return results
}

// CLIChecks runs the offline checks of the a2a CLI and its config.
func CLIChecks(ctx context.Context, env Env) []Result {
	return cliChecks(ctx, env)
}

// CredentialHelperConfigured reports whether the a2a CLI attaches the tokens
// of this machine: the credential-helper check passes.
func CredentialHelperConfigured(env Env) bool {
	path := UserConfigPath(env.Home)
	values, err := readUserConfig(path)
	if err != nil {
		return false
	}
	return checkCredentialHelper(env, path, values).Status == StatusOK
}

// SetupProblems returns one line per failed a2a CLI check of this machine,
// for the server to print on start.
func SetupProblems(ctx context.Context) []string {
	env, err := System()
	if err != nil {
		return []string{err.Error()}
	}
	var out []string
	for _, r := range CLIChecks(ctx, env) {
		if r.Status != StatusFail {
			continue
		}
		line := r.Message
		if r.Fix != "" {
			line += ": " + r.Fix
		}
		out = append(out, line)
	}
	return out
}

// Failed reports whether any check failed.
func Failed(results []Result) bool {
	for _, r := range results {
		if r.Status == StatusFail {
			return true
		}
	}
	return false
}

var symbols = map[Status]string{StatusOK: "✓", StatusWarn: "!", StatusFail: "✗", StatusSkip: "-"}

// Render writes the results as text, one line per check under its group,
// and a summary.
func Render(w io.Writer, results []Result) error {
	var sb strings.Builder
	group := ""
	failed, warned := 0, 0
	for _, r := range results {
		if r.Group != group {
			group = r.Group
			fmt.Fprintln(&sb, group)
		}
		switch r.Status {
		case StatusFail:
			failed++
		case StatusWarn:
			warned++
		}
		line := r.Message
		if r.Status == StatusWarn && r.Fix != "" {
			line += ": " + r.Fix
		}
		fmt.Fprintf(&sb, "  %s %s\n", symbols[r.Status], line)
		if r.Detail != "" && r.Status != StatusOK {
			fmt.Fprintf(&sb, "      %s\n", r.Detail)
		}
		if r.Status == StatusFail && r.Fix != "" {
			fmt.Fprintf(&sb, "      fix: %s\n", r.Fix)
		}
	}
	fmt.Fprintln(&sb, summary(failed, warned))
	_, err := io.WriteString(w, sb.String())
	return err
}

func summary(failed, warned int) string {
	var parts []string
	if failed > 0 {
		parts = append(parts, fmt.Sprintf("%d failed", failed))
	}
	switch {
	case warned == 1:
		parts = append(parts, "1 warning")
	case warned > 1:
		parts = append(parts, fmt.Sprintf("%d warnings", warned))
	}
	if len(parts) == 0 {
		return "no problems found"
	}
	return strings.Join(parts, ", ")
}
