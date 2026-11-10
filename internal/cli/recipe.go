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
	"path/filepath"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/a2aproject/a2a-serve/internal/agentctl"
	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/clierr"
	"github.com/a2aproject/a2a-serve/internal/flagparse"
	"github.com/a2aproject/a2a-serve/internal/recipe"
	"github.com/a2aproject/a2a-serve/internal/wizard"
)

func newRecipeCmd(cfg *globalConfig, sf *serverFlags) *cobra.Command {
	var (
		flags  settingsFlags
		yes    bool
		dryRun bool
	)
	cmd := &cobra.Command{
		Use:   "recipe [<source>]",
		Short: "Create an agent from a recipe, a directory with a ready-made agent, or update it",
		Long: `Create an agent from a recipe: a directory with a committed
.a2a/config.yaml, and optionally AGENTS.md, skills, .mcp.json and
.a2a/recipe.yaml, which describes its MCP servers. The source is a tree URL
such as https://github.com/<org>/<repo>/tree/<ref>/<path>, a git URL with
an optional #<ref>:<path>, or a local directory. Git sources are fetched
with your git, so your SSH keys and credential helpers apply.

recipe shows what the agent will run and asks before it creates the agent
in ./<name>, or in --dir. It asks for the secrets the agent lacks and
whether to use each optional MCP server. The recipe sets what the agent
is; where it is reachable and who may call it stay yours. The flags of
` + "`a2a-serve config set`" + ` apply on top of the recipe. Start the agent with
` + "`a2a-serve start`" + `.

Run recipe again in the agent's directory, without a source, to update the
agent to a newer version of its recipe: a branch moves to its newest
commit, a tag or a commit stays. The update keeps your changes. A file
changed both here and in the recipe stays as it is, and the recipe's
version is saved next to it as <file>.recipe.

` + flagparse.SecretHelp,
		Example: `  a2a-serve recipe https://github.com/acme/agents/tree/v1.2/experts/elk --name elk
  a2a-serve recipe git@github.com:acme/agents.git#v1.2:experts/elk --dry-run
  a2a-serve recipe ./experts/elk --yes --disable-mcp kubernetes --secret GRAFANA_TOKEN
  a2a-serve recipe --dry-run`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			interactive := cfg.isTerminal() && !yes
			run := &recipeRun{cfg: cfg, sf: sf, dir: sf.dir, interactive: interactive, dryRun: dryRun, unconfirmed: !interactive && !yes && !dryRun, getenv: cfg.env}
			if len(args) == 1 {
				src, err := recipe.ParseSource(args[0])
				if err != nil {
					return clierr.Usage(err.Error())
				}
				run.src, run.hasSource = src, true
			}
			s, err := flags.settings(cfg.secretReader(cfg.stderr()))
			if err != nil {
				return err
			}
			run.s = s
			return run.run(cmd.Context())
		},
	}
	flags.register(cmd, agentdir.AgentFlag|agentdir.CoreFlags|agentdir.OperationalFlags)
	cmd.SetUsageFunc(sectionedUsage)
	f := cmd.Flags()
	f.StringVar(&sf.dir, "dir", "", "Agent directory: a new agent's, or the agent to update (default: the agent here, or ./<name> for a new one)")
	f.BoolVarP(&yes, "yes", "y", false, "Create or update the agent without asking; the review still prints")
	f.BoolVar(&dryRun, "dry-run", false, "Fetch the recipe and print the review without saving")
	setSection(f, agentdir.SectionGeneral, "dir", "yes", "dry-run")
	markTextOnly(cmd)
	return cmd
}

func unconfirmedError() error {
	return clierr.Usage("recipe asks before it changes anything; without a terminal, pass --yes or --dry-run")
}

// recipeRun creates an agent from a recipe, or updates it.
type recipeRun struct {
	cfg         *globalConfig
	sf          *serverFlags
	src         recipe.Source
	hasSource   bool
	s           agentdir.Settings
	dir         string
	interactive bool
	dryRun      bool
	// unconfirmed is a run that can neither ask nor change anything, which
	// fails once a local source is read, and before a git source is fetched.
	unconfirmed bool
	getenv      func(string) string
	prompter    *wizard.Prompter
	// stored are the secrets of an agent being updated.
	stored agentdir.Secrets
	// known are the MCP servers of the recipe at the recorded commit, which
	// an update doesn't ask about again.
	known []string
}

func (r *recipeRun) printf(format string, args ...any) error {
	_, err := fmt.Fprintf(r.cfg.Out, format, args...)
	return err
}

// run fetches the recipe, and updates the agent it belongs to or creates
// a new one.
func (r *recipeRun) run(ctx context.Context) error {
	var (
		d      agentdir.Dir
		record *recipe.Record
		err    error
	)
	if !r.hasSource {
		if d, err = r.sf.agentDir(); err != nil {
			return err
		}
		if record, err = readRecord(d); err != nil {
			return err
		}
		r.src = record.Source()
	}
	if r.unconfirmed && !r.src.Local() {
		return unconfirmedError()
	}
	work, err := os.MkdirTemp("", "a2a-recipe-*")
	if err != nil {
		return err
	}
	defer func() {
		if rmErr := os.RemoveAll(work); rmErr != nil {
			slog.Warn("removing the fetched recipe", "dir", work, "error", rmErr)
		}
	}()
	fetched, rec, err := r.fetch(ctx, work)
	if err != nil {
		return err
	}
	if r.unconfirmed {
		return unconfirmedError()
	}
	if record != nil {
		return r.update(ctx, d, record, fetched, rec)
	}
	if r.dir != "" {
		if d, err = agentdir.Open(r.dir); err != nil {
			return err
		}
		if !d.Exists() {
			return r.create(ctx, fetched, rec)
		}
		if record, err = readRecord(d); err != nil {
			return err
		}
		return r.update(ctx, d, record, fetched, rec)
	}
	wd, err := os.Getwd()
	if err != nil {
		return err
	}
	if d, err := agentdir.Find(wd); err == nil {
		if record, err := recipe.ReadRecord(d); err == nil && sameSource(record.Source(), fetched.Source) {
			return r.update(ctx, d, record, fetched, rec)
		}
	}
	return r.create(ctx, fetched, rec)
}

// readRecord reads the record of the agent d, which must have come from a
// recipe.
func readRecord(d agentdir.Dir) (*recipe.Record, error) {
	record, err := recipe.ReadRecord(d)
	if errors.Is(err, os.ErrNotExist) {
		return nil, clierr.Usage(fmt.Sprintf("the agent in %s wasn't created from a recipe; create a new agent from one with `a2a-serve recipe <source> --dir <new directory>`", d.Root))
	}
	return record, err
}

// sameSource reports whether a and b are the same directory of the same
// repository, at any ref.
func sameSource(a, b recipe.Source) bool {
	return a.Repo == b.Repo && a.Path == b.Path && a.Dir == b.Dir
}

// create creates a new agent from the fetched recipe rec.
func (r *recipeRun) create(ctx context.Context, fetched *recipe.Fetched, rec *recipe.Recipe) (err error) {
	parent, err := r.parent()
	if err != nil {
		return err
	}
	if outer, err := agentdir.FindEnclosing(parent); err == nil {
		return clierr.Usage(fmt.Sprintf("%s is inside the agent in %s, which could read the new agent's secrets; create it elsewhere or pass --dir", parent, outer.Root))
	}
	name := agentdir.SanitizeName(firstNonEmpty(r.s.Name, rec.Config.Name, r.src.Name()))
	target := r.dir
	if target == "" {
		target = filepath.Join(parent, name)
	}
	if target, err = filepath.Abs(target); err != nil {
		return err
	}
	if err := checkTarget(target); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(parent, "."+name+".recipe-*")
	if err != nil {
		return err
	}
	defer func() {
		if rmErr := os.RemoveAll(stage); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
			err = errors.Join(err, rmErr)
		}
	}()
	if err := recipe.Copy(fetched.Dir, stage, rec.Files, rec.Files.Paths()); err != nil {
		return err
	}
	d, err := agentdir.Open(stage)
	if err != nil {
		return err
	}
	base := rec.NewConfig(name)
	preview, err := r.preview(d, rec, base)
	if err != nil {
		return err
	}
	if err := r.printReview(name, fetched, rec, agentdir.Dir{Root: target}, preview); err != nil {
		return err
	}
	if r.dryRun {
		return r.printf("  (dry run: nothing saved)\n")
	}
	if r.interactive {
		r.prompter = wizard.NewPrompter(r.cfg.input(), r.cfg.Out, r.cfg.readSecret())
		ok, err := r.prompter.Confirm(fmt.Sprintf("Create %s in %s?", name, displayPath(target)), true)
		if err != nil {
			return err
		}
		if !ok {
			return wizard.ErrCanceled
		}
	}
	off, err := r.ask(d, target, rec, preview)
	if err != nil {
		return err
	}
	if err := r.save(ctx, d, fetched, rec, base, off); err != nil {
		return err
	}
	if err := moveDir(stage, target); err != nil {
		return err
	}
	done := fmt.Sprintf("✓ created %s in %s", name, displayPath(target))
	if len(off) > 0 {
		done += ", with " + strings.Join(off, ", ") + " off"
	}
	return r.printf("%s\n  start it with `cd %s && a2a-serve start`\n", done, strings.TrimPrefix(displayPath(target), "."+string(filepath.Separator)))
}

// parent is the directory that holds the new agent.
func (r *recipeRun) parent() (string, error) {
	if r.dir == "" {
		return os.Getwd()
	}
	abs, err := filepath.Abs(r.dir)
	if err != nil {
		return "", err
	}
	parent := filepath.Dir(abs)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return "", err
	}
	return parent, nil
}

// fetch fetches the recipe, a git source into the empty directory work.
func (r *recipeRun) fetch(ctx context.Context, work string) (*recipe.Fetched, *recipe.Recipe, error) {
	if !r.src.Local() {
		if err := r.printf("fetching %s\n", r.src.Describe()); err != nil {
			return nil, nil, err
		}
	}
	fetched, err := recipe.Fetcher{Git: recipe.ExecGit(r.interactive)}.Fetch(ctx, r.src, work)
	if err != nil {
		return nil, nil, err
	}
	rec, err := recipe.Load(fetched.Dir)
	if err != nil {
		return nil, nil, err
	}
	if fetched.Commit != "" {
		err = r.printf("✓ fetched %s, %d files\n\n", shortCommit(fetched.Commit), len(rec.Files))
	}
	return fetched, rec, err
}

// checkTarget refuses a directory that holds files or an agent.
func checkTarget(target string) error {
	if (agentdir.Dir{Root: target}).Exists() {
		return clierr.Usage(target + " already has an agent; create the new one in another directory with --dir")
	}
	entries, err := os.ReadDir(target)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if len(entries) > 0 {
		return clierr.Usage(target + " isn't empty; create the agent in a new directory with --dir")
	}
	return nil
}

// preview is the config of the agent with the flags applied, checked
// except for secret values and the mounts of optional MCP servers, so that
// a missing mount source fails before any question.
func (r *recipeRun) preview(d agentdir.Dir, rec *recipe.Recipe, base *agentdir.Config) (*agentdir.Config, error) {
	cfg := base.Clone()
	if _, err := r.s.Apply(cfg, d); err != nil {
		return nil, clierr.Usage(err.Error())
	}
	check := cfg.Clone()
	var optional []string
	for _, srv := range rec.MCP {
		if rec.Manifest.Optional(srv.Name) || check.MCPDisabled(srv.Name) {
			optional = append(optional, srv.Name)
		}
	}
	rec.DropMounts(check, optional)
	secrets := agentdir.Secrets{}
	for _, name := range check.Secrets {
		secrets[name] = "-"
	}
	if err := d.CheckConfig(check, secrets); err != nil {
		return nil, clierr.Usage(err.Error())
	}
	return cfg, nil
}

// printReview prints what the agent of rec will run in the agent directory
// target.
func (r *recipeRun) printReview(name string, f *recipe.Fetched, rec *recipe.Recipe, target agentdir.Dir, cfg *agentdir.Config) error {
	var sb strings.Builder
	from := r.src.Describe()
	if f.Commit != "" {
		from = f.Source.Describe() + " (" + shortCommit(f.Commit) + ")"
	}
	fmt.Fprintf(&sb, "%s from %s will run:\n", name, from)
	for _, line := range recipe.Review(rec, target, cfg, displayPath(target.Root)) {
		label := line.Label
		for _, item := range line.Items {
			text := item.Text
			if item.Warn {
				text = "! " + text
			}
			fmt.Fprintf(&sb, "  %-13s %s\n", label, text)
			if item.Detail != "" {
				fmt.Fprintf(&sb, "  %-13s   %s\n", "", item.Detail)
			}
			label = ""
		}
	}
	if _, ok := rec.Files["README.md"]; ok {
		sb.WriteString("Read the recipe's README.md for the access the agent's credentials need.\n")
	}
	sb.WriteString("\n")
	_, err := io.WriteString(r.cfg.Out, sb.String())
	return err
}

// ask gets the values the agent needs: the secrets of the recipe's config,
// a model API key, and the variables of the MCP servers it uses. It
// returns the MCP servers left off. The agent is in d, which moves to
// target.
func (r *recipeRun) ask(d agentdir.Dir, target string, rec *recipe.Recipe, cfg *agentdir.Config) (off []string, err error) {
	var missing []string
	serverMissing := false
	need := func(v recipe.Var, required bool) (bool, error) {
		wv := wizard.Var{Name: v.Name, Description: v.Description, Secret: v.Secret}
		if r.provided(cfg, v.Name) {
			return true, nil
		}
		if r.prompter != nil {
			return true, r.prompter.VarValue(&r.s, r.getenv, wv)
		}
		if val := r.getenv(v.Name); val != "" {
			wizard.AddVar(&r.s, wv, val)
			return true, nil
		}
		if !required {
			return false, nil
		}
		if v.Secret {
			missing = append(missing, "--secret "+v.Name)
		} else {
			missing = append(missing, "--env "+v.Name+"=<value>")
		}
		return false, nil
	}
	for _, v := range rec.SecretVars() {
		if _, err := need(v, true); err != nil {
			return nil, err
		}
	}
	if r.stored == nil && r.prompter != nil && wizard.NeedsAPIKey(cfg.Harness, slices.Concat(cfg.Secrets, r.s.Secrets)) {
		keys, err := r.prompter.APIKeys(cfg.Harness, r.getenv)
		if err != nil {
			return nil, err
		}
		r.s.Secrets = append(r.s.Secrets, keys...)
	}
	for _, srv := range rec.MCP {
		if cfg.MCPDisabled(srv.Name) {
			off = append(off, srv.Name)
			continue
		}
		optional := rec.Manifest.Optional(srv.Name)
		vars := rec.ServerVars(srv, func(name string) bool { return r.provided(cfg, name) })
		use, err := r.useServer(d, target, rec, cfg, srv.Name, optional, vars)
		if err != nil {
			return nil, err
		}
		for _, v := range vars {
			if !use {
				break
			}
			if use, err = need(v, !optional); err != nil {
				return nil, err
			}
			serverMissing = serverMissing || !use
		}
		if !use {
			off = append(off, srv.Name)
			r.s.DisableMCP = append(r.s.DisableMCP, srv.Name)
		}
	}
	if len(missing) > 0 {
		msg := "the agent needs values the environment doesn't set; pass " + strings.Join(missing, " ")
		if serverMissing {
			msg += ", or turn off the MCP servers that need them with --disable-mcp"
		}
		return nil, clierr.Usage(msg)
	}
	return off, nil
}

// useServer asks whether to use an optional MCP server new to the agent.
// Without asking, one is used when its variables have values. One whose
// tied mounts have no source stays off.
func (r *recipeRun) useServer(d agentdir.Dir, target string, rec *recipe.Recipe, cfg *agentdir.Config, name string, optional bool, vars []recipe.Var) (bool, error) {
	if !optional || slices.Contains(r.known, name) {
		return true, nil
	}
	if missing := rec.MissingMounts(d, cfg, name); len(missing) > 0 {
		for i, p := range missing {
			missing[i] = movedPath(p, d.Root, target)
		}
		return false, r.printf("  %s stays off: it needs %s, which doesn't exist\n", name, strings.Join(missing, ", "))
	}
	if r.prompter == nil {
		return !slices.ContainsFunc(vars, func(v recipe.Var) bool { return r.getenv(v.Name) == "" }), nil
	}
	q := "Use " + name
	if desc := rec.Manifest.MCP[name].Description; desc != "" {
		q += " (" + desc + ")"
	}
	return r.prompter.Confirm(q+"?", true)
}

// provided reports whether the agent gets a value for the variable name:
// from the flags, the env of the config, or a stored secret it passes.
func (r *recipeRun) provided(cfg *agentdir.Config, name string) bool {
	if slices.Contains(r.s.Secrets, name) || slices.Contains(cfg.Secrets, name) && r.stored[name] != "" {
		return true
	}
	return slices.ContainsFunc(slices.Concat(cfg.Env, r.s.Env), func(kv string) bool {
		k, _, _ := strings.Cut(kv, "=")
		return k == name
	})
}

// save saves the agent in the staging directory d: its config with the
// flags and answers, the secrets and, last, the record of the recipe.
func (r *recipeRun) save(ctx context.Context, d agentdir.Dir, f *recipe.Fetched, rec *recipe.Recipe, base *agentdir.Config, off []string) error {
	_, err := agentctl.ApplyThen(ctx, d, nil, func(cfg *agentdir.Config) (agentdir.Secrets, []string, error) {
		*cfg = *base.Clone()
		rec.DropMounts(cfg, off)
		store, err := r.s.Apply(cfg, d)
		return store, nil, err
	}, func(*agentdir.Config) error {
		return recipe.WriteRecord(d, recipe.NewRecord(f, rec))
	}, false)
	var se *agentdir.SettingsError
	if errors.As(err, &se) {
		return clierr.Usage(se.Error())
	}
	return err
}

// moveDir renames the staging directory to target, which may be an empty
// directory.
func moveDir(stage, target string) error {
	if err := os.Remove(target); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.Rename(stage, target)
}

// movedPath returns where p is once the directory from moves to to.
func movedPath(p, from, to string) string {
	rel, err := filepath.Rel(from, p)
	if err != nil || !filepath.IsLocal(rel) {
		return p
	}
	return filepath.Join(to, rel)
}

// displayPath writes p relative to the current directory when it is inside
// it, such as ./o11y.
func displayPath(p string) string {
	wd, err := os.Getwd()
	if err != nil {
		return p
	}
	rel, err := filepath.Rel(wd, p)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return p
	}
	return "." + string(filepath.Separator) + rel
}

func shortCommit(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
