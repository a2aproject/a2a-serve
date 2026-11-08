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
	"maps"
	"slices"
	"strings"

	"github.com/a2aproject/a2a-serve/internal/agentctl"
	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/clierr"
	"github.com/a2aproject/a2a-serve/internal/project"
	"github.com/a2aproject/a2a-serve/internal/recipe"
	"github.com/a2aproject/a2a-serve/internal/wizard"
)

// recipeUpdate is an update of the agent d to the fetched recipe up, from
// the recipe at the recorded commit.
type recipeUpdate struct {
	d         agentdir.Dir
	record    *recipe.Record
	fetched   *recipe.Fetched
	up        *recipe.Recipe
	local     *agentdir.Config
	merged    *agentdir.Config
	conflicts []agentdir.Conflict
	changes   []recipe.FileChange
	edits     []string
	pending   []string
	servers   []project.MCPServer
}

// update updates the agent d to the fetched recipe up, keeping the changes
// made in the agent's directory.
func (r *recipeRun) update(ctx context.Context, d agentdir.Dir, record *recipe.Record, fetched *recipe.Fetched, up *recipe.Recipe) error {
	u, err := planUpdate(d, record, fetched, up)
	if err != nil {
		return err
	}
	if r.stored, err = d.LoadSecrets(); err != nil {
		return err
	}
	r.known = record.MCP
	preview := u.merged.Clone()
	if _, err := r.s.Apply(preview, d); err != nil {
		return clierr.Usage(err.Error())
	}
	up.DropMounts(preview, preview.MCP.Disabled)
	changes := agentdir.Diff(u.local, preview)
	if fetched.Commit == record.Commit && sameSource(record.Source(), fetched.Source) && len(u.changes) == 0 && len(changes) == 0 {
		if !r.dryRun {
			if err := recipe.RemoveResolved(d.Root, up.Files); err != nil {
				return err
			}
		}
		return r.printUpToDate(u)
	}
	if err := r.printUpdate(u, changes); err != nil {
		return err
	}
	if r.dryRun {
		return r.printf("  (dry run: nothing saved)\n")
	}
	if r.interactive {
		r.prompter = wizard.NewPrompter(r.cfg.input(), r.cfg.Out, r.cfg.readSecret())
		ok, err := r.prompter.Confirm("Apply these changes?", true)
		if err != nil {
			return err
		}
		if !ok {
			return wizard.ErrCanceled
		}
	}
	off, err := r.ask(up, preview)
	if err != nil {
		return err
	}
	res, err := agentctl.ApplyThen(ctx, d, r.sf.controlClient(d), func(cfg *agentdir.Config) (agentdir.Secrets, []string, error) {
		old := cfg.Clone()
		*cfg = *u.merged.Clone()
		up.DropMounts(cfg, off)
		store, err := r.s.Apply(cfg, d)
		return store, agentdir.UnusedSecrets(old, cfg), err
	}, func(*agentdir.Config) error {
		if err := recipe.ApplyFiles(fetched.Dir, d.Root, up.Files, u.changes); err != nil {
			return err
		}
		return recipe.WriteRecord(d, recipe.NewRecord(fetched, up))
	}, false)
	var se *agentdir.SettingsError
	if errors.As(err, &se) {
		return clierr.Usage(se.Error())
	}
	if err != nil {
		return err
	}
	return r.printUpdated(u, res)
}

// planUpdate merges the agent's settings and files with the recipe's.
func planUpdate(d agentdir.Dir, record *recipe.Record, fetched *recipe.Fetched, up *recipe.Recipe) (*recipeUpdate, error) {
	local, err := d.LoadConfig()
	if err != nil {
		return nil, err
	}
	base, err := agentdir.ParseConfig([]byte(record.Config))
	if err != nil {
		return nil, fmt.Errorf("parsing the recipe's config.yaml in %s: %w", d.RecipeLockPath(), err)
	}
	baseFiles, err := record.RecipeFiles()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", d.RecipeLockPath(), err)
	}
	withTied := local.Clone()
	up.RestoreMounts(withTied, base, local.MCP.Disabled)
	merged, conflicts := agentdir.Merge3(base, withTied, up.Config)
	localFiles, err := recipe.LocalFiles(d.Root, slices.Concat(baseFiles.Paths(), up.Files.Paths()))
	if err != nil {
		return nil, err
	}
	pending, err := recipe.Outstanding(d.Root, up.Files)
	if err != nil {
		return nil, err
	}
	servers, err := project.LoadMCP(d.Root)
	if err != nil {
		slog.Warn("reading the agent's MCP servers to compare them with the recipe's", "error", err)
	}
	return &recipeUpdate{
		d: d, record: record, fetched: fetched, up: up, local: local,
		merged: merged, conflicts: conflicts,
		changes: recipe.PlanFiles(baseFiles, localFiles, up.Files),
		edits:   recipe.LocalEdits(baseFiles, localFiles),
		pending: pending, servers: servers,
	}, nil
}

// at names the ref and commit of a record or a fetch, such as main
// (3f2a1c9).
func at(ref, commit string) string {
	if ref == "" {
		ref = "the default branch"
	}
	if commit == "" {
		return ref
	}
	return ref + " (" + shortCommit(commit) + ")"
}

func (r *recipeRun) printUpToDate(u *recipeUpdate) error {
	var sb strings.Builder
	name := u.local.Name
	if u.fetched.Source.Local() {
		fmt.Fprintf(&sb, "✓ %s is up to date with %s\n", name, u.fetched.Source.Dir)
	} else {
		fmt.Fprintf(&sb, "✓ %s is up to date with %s\n", name, at(u.fetched.Source.Ref, u.fetched.Commit))
	}
	writeEdits(&sb, u)
	_, err := io.WriteString(r.cfg.Out, sb.String())
	return err
}

func (r *recipeRun) printUpdate(u *recipeUpdate, changes []agentdir.Change) error {
	var sb strings.Builder
	old, next := u.record.Source(), u.fetched.Source
	switch {
	case !sameSource(old, next):
		fmt.Fprintf(&sb, "%s: ! the source changes from %s (%s) to %s (%s)\n", u.local.Name, old.Describe(), shortCommit(u.record.Commit), next.Describe(), shortCommit(u.fetched.Commit))
	case next.Local():
		fmt.Fprintf(&sb, "%s: %s\n", u.local.Name, next.Dir)
	case old.Ref != next.Ref:
		fmt.Fprintf(&sb, "%s: %s → %s\n", u.local.Name, at(old.Ref, u.record.Commit), at(next.Ref, u.fetched.Commit))
	default:
		fmt.Fprintf(&sb, "%s: %s %s → %s\n", u.local.Name, at(next.Ref, ""), shortCommit(u.record.Commit), shortCommit(u.fetched.Commit))
	}
	line := func(label, text string) { fmt.Fprintf(&sb, "  %-13s %s\n", label, text) }
	var files []string
	for _, c := range u.changes {
		switch c.Action {
		case recipe.FileAdd:
			files = append(files, "+ "+c.Path)
		case recipe.FileUpdate:
			files = append(files, "~ "+c.Path)
		case recipe.FileRemove:
			files = append(files, "- "+c.Path)
		}
	}
	if len(files) > 0 {
		line("files", strings.Join(files, ", "))
	}
	for _, c := range u.changes {
		warn := ""
		if recipe.IsRisky(c.Path) {
			warn = "! "
		}
		switch c.Action {
		case recipe.FileConflict:
			line("kept", fmt.Sprintf("%s%s, changed here and in the recipe; the recipe's version is in %s%s", warn, c.Path, c.Path, recipe.RecipeSuffix))
		case recipe.FileKeepRemoved:
			line("kept", fmt.Sprintf("%s%s removed, which the recipe changed", warn, c.Path))
		case recipe.FileKeepChanged:
			line("kept", fmt.Sprintf("%s%s, changed here and removed from the recipe", warn, c.Path))
		}
	}
	if applied := slices.ContainsFunc(u.changes, func(c recipe.FileChange) bool {
		return c.Path == project.MCPFile && c.Action != recipe.FileConflict && c.Action != recipe.FileKeepRemoved && c.Action != recipe.FileKeepChanged
	}); applied {
		for _, item := range mcpChanges(u.servers, u.up.MCP) {
			line("mcp", item)
		}
	}
	for _, c := range changes {
		line(c.Setting, formatChange(c, nil))
	}
	for _, c := range u.conflicts {
		what := c.Setting
		if c.Key != "" {
			what += " " + c.Key
		}
		line("kept", what+", changed here and in the recipe")
	}
	writeEdits(&sb, u)
	sb.WriteString("\n")
	_, err := io.WriteString(r.cfg.Out, sb.String())
	return err
}

// writeEdits reports the changes made in the agent's directory to the files
// that decide what the agent runs, and the recipe copies not resolved yet.
func writeEdits(sb *strings.Builder, u *recipeUpdate) {
	if len(u.edits) > 0 {
		fmt.Fprintf(sb, "  %-13s ! %s: they decide what the agent runs\n", "changed here", strings.Join(u.edits, ", "))
	}
	pending := slices.DeleteFunc(slices.Clone(u.pending), func(p string) bool {
		return slices.ContainsFunc(u.changes, func(c recipe.FileChange) bool { return c.Path == p && c.Action == recipe.FileConflict })
	})
	if len(pending) > 0 {
		fmt.Fprintf(sb, "  %-13s %s: compare with the %s copies, then delete them\n", "unresolved", strings.Join(pending, ", "), recipe.RecipeSuffix)
	}
}

// mcpChanges lists the MCP servers added, changed and removed from the
// agent's .mcp.json to the recipe's.
func mcpChanges(old, next []project.MCPServer) []string {
	before := map[string]string{}
	for _, s := range old {
		before[s.Name] = s.Summary()
	}
	var out []string
	for _, s := range next {
		prev, ok := before[s.Name]
		switch {
		case !ok:
			out = append(out, "+ "+s.Name+": "+s.Summary())
		case prev != s.Summary():
			out = append(out, "~ "+s.Name+": "+s.Summary())
		}
		delete(before, s.Name)
	}
	for _, name := range slices.Sorted(maps.Keys(before)) {
		out = append(out, "- "+name)
	}
	return out
}

func (r *recipeRun) printUpdated(u *recipeUpdate, res *agentctl.Result) error {
	written := 0
	for _, c := range u.changes {
		if c.Action == recipe.FileAdd || c.Action == recipe.FileUpdate || c.Action == recipe.FileRemove || c.Action == recipe.FileConflict {
			written++
		}
	}
	var sb strings.Builder
	where := u.fetched.Source.Dir
	if !u.fetched.Source.Local() {
		where = at(u.fetched.Source.Ref, u.fetched.Commit)
	}
	fmt.Fprintf(&sb, "✓ saved %s and %d files; the recipe is at %s\n", displayPath(u.d.ConfigPath()), written, where)
	if len(res.Applied) > 0 {
		fmt.Fprintf(&sb, "✓ applied to the running agent: %s\n", strings.Join(res.Applied, ", "))
	}
	restart := res.Restart
	if res.Running && recipe.TouchesSkills(u.changes) {
		restart = append(restart, "skills")
	}
	if len(restart) > 0 {
		fmt.Fprintf(&sb, "! %s is running; restart to apply %s: a2a-serve restart\n", res.Name, strings.Join(restart, ", "))
	}
	writeAccess(&sb, res.Access, restart)
	_, err := io.WriteString(r.cfg.Out, sb.String())
	return err
}
