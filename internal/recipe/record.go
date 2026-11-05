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

package recipe

import (
	"fmt"
	"os"
	"slices"

	"gopkg.in/yaml.v3"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/fsutil"
)

// Record is the content of .a2a/recipe.lock: the recipe an agent came from,
// at the commit it was created or last updated from. It holds no secrets.
type Record struct {
	Repo   string `yaml:"repo,omitempty"`
	Ref    string `yaml:"ref,omitempty"`
	Path   string `yaml:"path,omitempty"`
	Commit string `yaml:"commit,omitempty"`
	// Dir is the directory of a local source.
	Dir string `yaml:"dir,omitempty"`
	// MCP are the names of the recipe's MCP servers.
	MCP []string `yaml:"mcp,omitempty"`
	// Files are the recipe's files, in the form of [File.String].
	Files map[string]string `yaml:"files,omitempty"`
	// Config is the recipe's config.yaml.
	Config string `yaml:"config,omitempty"`
}

const recordHeader = "# Written by `a2a-serve recipe`. It records the recipe this agent came from,\n" +
	"# so that running the command again can tell your changes from the recipe's.\n"

// NewRecord returns the record of the recipe r, fetched as f.
func NewRecord(f *Fetched, r *Recipe) *Record {
	rec := &Record{
		Repo:   f.Source.Repo,
		Ref:    f.Source.Ref,
		Path:   f.Source.Path,
		Commit: f.Commit,
		Dir:    f.Source.Dir,
		Files:  make(map[string]string, len(r.Files)),
		Config: string(r.ConfigData),
	}
	for _, s := range r.MCP {
		rec.MCP = append(rec.MCP, s.Name)
	}
	for name, file := range r.Files {
		rec.Files[name] = file.String()
	}
	return rec
}

// Source returns the source the record was fetched from.
func (r *Record) Source() Source {
	return Source{Repo: r.Repo, Ref: r.Ref, Path: r.Path, Dir: r.Dir}
}

// RecipeFiles returns the recipe's files as the record lists them.
func (r *Record) RecipeFiles() (Files, error) {
	out := make(Files, len(r.Files))
	for name, s := range r.Files {
		f, err := ParseFile(s)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		out[name] = f
	}
	return out, nil
}

// ReadRecord reads the record of the agent d. Without one, the error
// matches [os.ErrNotExist].
func ReadRecord(d agentdir.Dir) (*Record, error) {
	data, err := os.ReadFile(d.RecipeLockPath())
	if err != nil {
		return nil, err
	}
	var r Record
	if err := yaml.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", d.RecipeLockPath(), err)
	}
	if (r.Repo == "") == (r.Dir == "") {
		return nil, fmt.Errorf("%s names neither a repository nor a directory", d.RecipeLockPath())
	}
	return &r, nil
}

// WriteRecord writes the record of the agent d.
func WriteRecord(d agentdir.Dir, r *Record) error {
	out := *r
	out.MCP = slices.Sorted(slices.Values(r.MCP))
	data, err := yaml.Marshal(&out)
	if err != nil {
		return err
	}
	return fsutil.WriteAtomic(d.RecipeLockPath(), append([]byte(recordHeader), data...), 0o644)
}
