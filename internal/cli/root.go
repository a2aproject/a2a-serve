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

// Package cli implements the a2a-serve command-line interface, which is also
// an a2a CLI command plugin.
package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/a2aproject/a2a-serve/internal/agenterr"
	"github.com/a2aproject/a2a-serve/internal/clierr"
	"github.com/a2aproject/a2a-serve/internal/credstore"
	"github.com/a2aproject/a2a-serve/internal/output"
	"github.com/a2aproject/a2a-serve/internal/secretref"
)

// pluginName is the name the a2a CLI runs this binary under: a2a-<name>
// on PATH becomes `a2a <name>`.
const pluginName = "serve"

// infoSubcommand is how the a2a CLI asks a command plugin for its metadata.
const infoSubcommand = "info"

type globalConfig struct {
	output  string
	verbose bool
	// timeout bounds the HTTP requests of the mailbox commands.
	timeout time.Duration

	errOut io.Writer
	// stdin overrides os.Stdin for secret values read with -, for tests.
	stdin         *os.File
	httpTransport http.RoundTripper
	// credentials overrides the store of `a2a-serve pair join` tokens, for tests.
	credentials *credstore.Store

	*output.Printer
}

func (g *globalConfig) stderr() io.Writer {
	if g.errOut != nil {
		return g.errOut
	}
	return os.Stderr
}

// progress is where progress lines of long operations go. JSON modes keep
// stdout for the result, so the lines go to stderr there.
func (g *globalConfig) progress() io.Writer {
	if g.IsJSON() {
		return g.stderr()
	}
	return g.Out
}

// secretReader returns a reader of secret values which prompts on prompt.
func (g *globalConfig) secretReader(prompt io.Writer) *secretref.Reader {
	in := os.Stdin
	if g.stdin != nil {
		in = g.stdin
	}
	return secretref.NewReader(in, prompt)
}

// setupLogging makes -v write debug logs to stderr.
func (g *globalConfig) setupLogging() {
	if !g.verbose {
		return
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(g.stderr(), &slog.HandlerOptions{Level: slog.LevelDebug})))
}

func (g *globalConfig) logf(format string, args ...any) {
	if g.verbose {
		if _, err := fmt.Fprintf(g.stderr(), "# "+format+"\n", args...); err != nil {
			return
		}
	}
}

func (g *globalConfig) credentialStore() (*credstore.Store, error) {
	if g.credentials != nil {
		return g.credentials, nil
	}
	return credstore.Default()
}

// Execute runs the CLI with args, the command line without the program
// name, and returns the exit code.
func Execute(args []string) int {
	if isPluginInfoCall(args, os.Stdout) {
		return printPluginInfo(os.Stdout)
	}
	cfg := &globalConfig{Printer: output.NewPrinter(os.Stdout, output.ModeText)}
	root := newRootCmd(cfg)
	root.SetArgs(args)
	if cmd, err := root.ExecuteC(); err != nil {
		return cfg.renderError(err, cmd)
	}
	return 0
}

// isPluginInfoCall reports whether args are the a2a CLI asking for the
// plugin's metadata. The CLI reads it through a pipe; on a terminal, info is
// an unknown command like any other.
func isPluginInfoCall(args []string, stdout *os.File) bool {
	return len(args) == 1 && args[0] == infoSubcommand && !term.IsTerminal(int(stdout.Fd()))
}

// pluginInfo is the document the a2a CLI reads from `a2a-serve info`.
type pluginInfo struct {
	Name        string `json:"name"`
	Version     string `json:"version,omitempty"`
	Description string `json:"description,omitempty"`
}

func printPluginInfo(w io.Writer) int {
	info := pluginInfo{Name: pluginName, Version: buildVersionInfo().Version, Description: serveShort}
	if err := json.NewEncoder(w).Encode(info); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}
	return 0
}

// listCommands names the command which lists what a not-found error names.
var listCommands = map[string]string{
	"pairing":  "pair list",
	"contact":  "contact list",
	"schedule": "schedule list",
	"channel":  "channel list",
}

// classify classifies err of cmd, pointing the hint of a usage error at the
// help of cmd, or at the command which lists what was not found.
func classify(err error, cmd *cobra.Command) *clierr.Error {
	ce := clierr.Classify(err)
	if ce.Code != clierr.CodeUsage || ce.Hint != clierr.UsageHint || cmd == nil {
		return ce
	}
	hinted := *ce
	var nf *agenterr.NotFoundError
	switch {
	case errors.As(err, &nf) && listCommands[nf.Kind] != "":
		hinted.Hint = fmt.Sprintf("run `%s %s` to see the %ss", cmd.Root().CommandPath(), listCommands[nf.Kind], nf.Kind)
	case cmd.HasParent():
		hinted.Hint = fmt.Sprintf("run `%s --help` for its flags", cmd.CommandPath())
	}
	return &hinted
}

func (g *globalConfig) renderError(err error, cmd *cobra.Command) int {
	ce := classify(err, cmd)
	if ce.Silent() {
		return ce.Exit
	}
	if g.IsJSON() {
		if perr := g.PrintJSON(ce); perr != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		}
		return ce.Exit
	}
	fmt.Fprintf(os.Stderr, "Error: %s\n", ce.Message)
	if ce.Hint != "" {
		fmt.Fprintf(os.Stderr, "hint: %s\n", ce.Hint)
	}
	return ce.Exit
}

// newRootCmd returns the `a2a-serve` command. The a2a CLI runs it as
// `a2a serve` with the same arguments, including those of shell completion.
func newRootCmd(cfg *globalConfig) *cobra.Command {
	root := newServeCmd(cfg)
	root.SilenceUsage, root.SilenceErrors = true, true
	root.CompletionOptions = cobra.CompletionOptions{DisableDefaultCmd: true}
	root.SetVersionTemplate(buildVersionInfo().text())
	pf := root.PersistentFlags()
	pf.StringVarP(&cfg.output, "output", "o", "text", "Output format: text, json (indented), or jsonl (one compact JSON object per line); config show also takes yaml")
	pf.BoolVarP(&cfg.verbose, "verbose", "v", false, "Print debug logs and progress details to stderr")
	root.PersistentPreRunE = func(cmd *cobra.Command, args []string) error {
		cfg.setupLogging()
		switch output.Mode(cfg.output) {
		case output.ModeText, output.ModeJson, output.ModeJSONL:
			cfg.Mode = output.Mode(cfg.output)
		default:
			return clierr.Usage(fmt.Sprintf("invalid --output %q (want text, json or jsonl)", cfg.output))
		}
		return checkTextOnly(cmd, cfg)
	}
	must(root.RegisterFlagCompletionFunc("output", fixedCompletions(string(output.ModeText), string(output.ModeJson), string(output.ModeJSONL))))
	help := newHelpCmd(root)
	markTextOnly(help)
	root.SetHelpCommand(help)
	addGroups(root)
	root.SetHelpCommandGroupID(groupSetup)
	markUsageErrors(root)
	return root
}

// textOnlyAnnotation marks a command which only prints text, so -o is an
// error there rather than ignored.
const textOnlyAnnotation = "a2a-serve-text-only"

// markTextOnly marks cmd and its subcommands as printing only text.
func markTextOnly(cmd *cobra.Command) {
	if cmd.Annotations == nil {
		cmd.Annotations = map[string]string{}
	}
	cmd.Annotations[textOnlyAnnotation] = "true"
	for _, sub := range cmd.Commands() {
		markTextOnly(sub)
	}
}

func checkTextOnly(cmd *cobra.Command, cfg *globalConfig) error {
	if cfg.output == string(output.ModeText) || cmd.Annotations[textOnlyAnnotation] == "" {
		return nil
	}
	return clierr.Usage(fmt.Sprintf("-o %s doesn't apply to `%s`, which prints text", cfg.output, cmd.CommandPath()))
}

// newHelpCmd returns `a2a-serve help [command]`, which reports an unknown
// topic as a usage error.
func newHelpCmd(serve *cobra.Command) *cobra.Command {
	return &cobra.Command{
		Use:   "help [command]",
		Short: "Help about any command",
		RunE: func(cmd *cobra.Command, args []string) error {
			target, rest, err := serve.Find(args)
			if err != nil || len(rest) > 0 {
				return clierr.Usage(fmt.Sprintf("unknown help topic %q", strings.Join(args, " ")))
			}
			return target.Help()
		},
	}
}

// markUsageErrors makes cobra's flag- and argument-validation failures surface
// as clierr usage errors.
func markUsageErrors(cmd *cobra.Command) {
	cmd.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		return clierr.Usage(err.Error())
	})
	if validate := cmd.Args; validate != nil {
		cmd.Args = func(c *cobra.Command, args []string) error {
			if err := validate(c, args); err != nil {
				return clierr.Usage(err.Error())
			}
			return nil
		}
	}
	for _, sub := range cmd.Commands() {
		markUsageErrors(sub)
	}
}
