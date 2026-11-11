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
	"strings"

	"github.com/spf13/pflag"

	"github.com/a2aproject/a2a-serve/internal/clierr"
	"github.com/a2aproject/a2a-serve/internal/netinstall"
)

const yesUsage = "Install the network provider of --network-provider from a release URL or a path without asking"

// addYesFlag adds --yes, which confirms the install of a network provider.
func addYesFlag(f *pflag.FlagSet, yes *bool) {
	f.BoolVar(yes, "yes", false, yesUsage)
}

// installNetworkProvider installs the provider of a --network-provider
// that names a release URL or a path. It shows where the provider comes
// from and its fingerprint, and installs it after the user confirms, or
// with --yes; nothing runs it before. A provider the user already trusted
// with this fingerprint installs without asking. It returns spec as
// config.yaml keeps it, with a path made absolute.
func installNetworkProvider(ctx context.Context, cfg *globalConfig, spec string, yes bool) (string, error) {
	if !netinstall.IsCustom(spec) {
		return spec, nil
	}
	if err := netinstall.Check(spec); err != nil {
		return "", clierr.Usage(fmt.Sprintf("invalid --network-provider %q: %v", spec, err))
	}
	logf := func(format string, args ...any) {
		if _, err := fmt.Fprintf(cfg.stderr(), format+"\n", args...); err != nil {
			slog.Debug("printing progress", "error", err)
		}
	}
	inst, err := netinstall.New(logf)
	if err != nil {
		return "", err
	}
	c, err := inst.Fetch(ctx, spec)
	if err != nil {
		return "", err
	}
	if c.Trusted {
		return c.Spec, nil
	}
	what := fmt.Sprintf("network provider from %s\n  sha256  %s", c.Source, c.SHA256)
	if c.Changed {
		what += "\n  ! this is not the provider you installed from there before"
	}
	if !yes {
		if !cfg.isTerminal() {
			return "", fmt.Errorf("%s\nrun the command again with --yes to install it, or from a terminal to confirm", what)
		}
		if _, err := fmt.Fprintf(cfg.stderr(), "%s\nIt runs as you, with the agent's network token. Install it? [y/N] ", what); err != nil {
			return "", err
		}
		line, err := bufio.NewReader(cfg.input()).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return "", err
		}
		if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
			return "", errors.New("the network provider wasn't installed")
		}
	}
	p, version, err := inst.Trust(ctx, c)
	if err != nil {
		return "", err
	}
	logf("✓ installed network provider %s %s (sha256 %s…) at %s", p.Name, version, c.SHA256[:12], p.Path)
	return c.Spec, nil
}
