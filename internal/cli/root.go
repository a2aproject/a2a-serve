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

// Package cli implements the a2a-serve command-line interface: an a2a CLI
// command plugin, run as `a2a serve`.
package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/a2aproject/a2a-serve/internal/clierr"
	"github.com/a2aproject/a2a-serve/internal/credstore"
	"github.com/a2aproject/a2a-serve/internal/output"
)

// pluginName is the name the a2a CLI runs this binary under: a2a-<name>
// on PATH becomes `a2a <name>`.
const pluginName = "serve"

// infoSubcommand is how the a2a CLI asks a command plugin for its metadata.
const infoSubcommand = "info"

type globalConfig struct {
	output  string
	verbose bool
	// timeout bounds the HTTP requests of tokens add and the mailbox commands.
	timeout time.Duration

	errOut io.Writer
	// stdin overrides os.Stdin for secret values read with -, for tests.
	stdin         *os.File
	httpTransport http.RoundTripper
	// credentials overrides the store of `a2a serve tokens add` tokens, for tests.
	credentials *credstore.Store

	*output.Printer
}

func (g *globalConfig) stderr() io.Writer {
	if g.errOut != nil {
		return g.errOut
	}
	return os.Stderr
}

// secretReader returns a reader of secret values which prompts on prompt.
func (g *globalConfig) secretReader(prompt io.Writer) *secretReader {
	in := os.Stdin
	if g.stdin != nil {
		in = g.stdin
	}
	return &secretReader{in: in, prompt: prompt}
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
	if len(args) == 1 && args[0] == infoSubcommand {
		return printPluginInfo(os.Stdout)
	}
	cfg := &globalConfig{Printer: &output.Printer{Out: os.Stdout}}
	root := newRootCmd(cfg)
	root.SetArgs(rootArgs(args))
	if err := root.Execute(); err != nil {
		return cfg.renderError(err)
	}
	return 0
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

func (g *globalConfig) renderError(err error) int {
	ce := clierr.Classify(err)
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

// rootArgs returns the arguments of the `a2a` root command for args, the
// command line of `a2a serve`. Shell completion asks `a2a-serve __complete
// <args>`, which completes `a2a serve <args>`.
func rootArgs(args []string) []string {
	if len(args) > 0 && (args[0] == cobra.ShellCompRequestCmd || args[0] == cobra.ShellCompNoDescRequestCmd) {
		return append([]string{args[0], pluginName}, args[1:]...)
	}
	return append([]string{pluginName}, args...)
}

// newRootCmd returns the `a2a` command with `serve` as its only subcommand,
// so that help and errors name commands the way users type them through
// the a2a CLI.
func newRootCmd(cfg *globalConfig) *cobra.Command {
	root := &cobra.Command{
		Use:               "a2a",
		SilenceUsage:      true,
		SilenceErrors:     true,
		CompletionOptions: cobra.CompletionOptions{DisableDefaultCmd: true},
	}
	serve := newServeCmd(cfg)
	serve.SetVersionTemplate("a2a-serve {{.Version}}\n")
	pf := serve.PersistentFlags()
	pf.StringVarP(&cfg.output, "output", "o", "text", "Output format: text, json (indented), or jsonl (one compact JSON object per line)")
	pf.BoolVarP(&cfg.verbose, "verbose", "v", false, "Verbose output to stderr")
	serve.PersistentPreRunE = func(cmd *cobra.Command, args []string) error {
		switch output.Mode(cfg.output) {
		case output.ModeText, output.ModeJson, output.ModeJSONL:
			cfg.Mode = output.Mode(cfg.output)
		default:
			return clierr.Usage(fmt.Sprintf("invalid --output %q (want text, json or jsonl)", cfg.output))
		}
		return nil
	}
	must(serve.RegisterFlagCompletionFunc("output", fixedCompletions(string(output.ModeText), string(output.ModeJson), string(output.ModeJSONL))))
	serve.AddCommand(newHelpCmd(serve))
	addGroups(serve)
	root.AddCommand(serve)
	markUsageErrors(root)
	return root
}

// newHelpCmd returns `a2a serve help [command]`. Cobra only adds a help
// command to the root, which here is the `a2a` wrapper.
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
