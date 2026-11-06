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
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/project"
)

// Recipe is a recipe read from its directory.
type Recipe struct {
	// Config is the recipe's config.yaml, with every setting it has.
	Config *agentdir.Config
	// ConfigData is the content of config.yaml, kept in the record.
	ConfigData []byte
	// Manifest is the recipe's recipe.yaml, empty without one.
	Manifest *Manifest
	MCP      []project.MCPServer
	Skills   []project.Skill
	Files    Files
}

// Load reads the recipe in dir. Its config.yaml may only use settings this
// a2a-serve knows, since leaving one out could weaken the agent.
func Load(dir string) (*Recipe, error) {
	data, err := readIn(dir, agentdir.Name+"/config.yaml")
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%s has no %s/config.yaml; a recipe is a directory with one", dir, agentdir.Name)
	}
	if err != nil {
		return nil, fmt.Errorf("reading the recipe's config.yaml: %w", err)
	}
	unknown, err := agentdir.UnknownKeys(data)
	if err != nil {
		return nil, fmt.Errorf("parsing the recipe's config.yaml: %w", err)
	}
	if len(unknown) > 0 {
		paths := make([]string, len(unknown))
		for i, k := range unknown {
			paths[i] = k.Path
		}
		return nil, fmt.Errorf("the recipe uses settings this a2a-serve doesn't know: %s; upgrade a2a-serve", strings.Join(paths, ", "))
	}
	cfg, err := agentdir.ParseConfig(data)
	if err != nil {
		return nil, fmt.Errorf("parsing the recipe's config.yaml: %w", err)
	}
	files, err := Scan(dir)
	if err != nil {
		return nil, err
	}
	servers, err := project.LoadMCP(dir)
	if err != nil {
		return nil, err
	}
	skills, err := project.Skills(dir)
	if err != nil {
		return nil, err
	}
	m := &Manifest{}
	mdata, err := readIn(dir, agentdir.Name+"/"+ManifestFile)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return nil, fmt.Errorf("reading the recipe's %s: %w", ManifestFile, err)
	default:
		if m, err = ParseManifest(mdata); err != nil {
			return nil, fmt.Errorf("parsing the recipe's %s: %w", ManifestFile, err)
		}
	}
	if err := m.check(cfg, servers); err != nil {
		return nil, fmt.Errorf("the recipe's %s: %w", ManifestFile, err)
	}
	return &Recipe{Config: cfg, ConfigData: data, Manifest: m, MCP: servers, Skills: skills, Files: files}, nil
}

// readIn reads the file name in dir without following symlinks out of dir.
func readIn(dir, name string) (_ []byte, err error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	return root.ReadFile(name)
}
