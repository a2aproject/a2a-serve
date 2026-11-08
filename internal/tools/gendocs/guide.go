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

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
)

// generatedPart is text of a page between the markers
// <!-- generated:NAME --> and <!-- /generated:NAME -->.
type generatedPart struct {
	// path is the page, relative to the reference directory.
	path string
	name string
	text func() string
}

var generatedParts = []generatedPart{
	{path: filepath.Join("..", "..", "guide", "config.md"), name: "live-settings", text: liveSettingsText},
	{path: filepath.Join("..", "configuration.md"), name: "settings", text: settingsTable},
}

// liveSettingsText says which settings a running agent applies at once.
func liveSettingsText() string {
	fileKeys := map[string]string{}
	for _, s := range agentdir.SettingDocs() {
		if s.FileOnly {
			fileKeys[s.Flag] = s.Key
		}
	}
	names := agentdir.LiveSettings()
	flags := make([]string, len(names))
	for i, name := range names {
		flags[i] = "`--" + name + "`"
		if key, ok := fileKeys[name]; ok {
			flags[i] = "`" + key + "`"
		}
	}
	list := strings.Join(flags[:len(flags)-1], ", ") + " and " + flags[len(flags)-1]
	return "A running agent applies " + list + " at once, and messengers too. Other settings apply after `a2a-serve restart`, or pass `--restart` to `config set` or `config unset`."
}

// settingsTable lists the key of config.yaml each flag sets.
func settingsTable() string {
	var sb strings.Builder
	sb.WriteString("| Flag | Key | Commands |\n| --- | --- | --- |\n")
	for _, s := range agentdir.SettingDocs() {
		key := "`" + s.Key + "`"
		if s.Key == "" {
			key = "the secret `" + s.Secret + "` in `state/secrets`"
		}
		commands := make([]string, len(s.Commands))
		for i, c := range s.Commands {
			commands[i] = "`" + c + "`"
		}
		flag := "`--" + s.Flag + "`"
		if s.FileOnly {
			flag = "no flag (`" + s.Flag + "` in `config unset`)"
		}
		fmt.Fprintf(&sb, "| %s | %s | %s |\n", flag, key, strings.Join(commands, ", "))
	}
	return strings.TrimSuffix(sb.String(), "\n")
}

// updateGenerated fills the generated parts of the pages, relative to the
// reference directory dir. A missing page is skipped, so the reference can
// be written anywhere.
func updateGenerated(dir string) error {
	for _, p := range generatedParts {
		if err := p.update(filepath.Join(dir, p.path)); err != nil {
			return err
		}
	}
	return nil
}

func (p generatedPart) update(path string) error {
	doc, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	updated, err := p.fill(string(doc))
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if updated == string(doc) {
		return nil
	}
	return os.WriteFile(path, []byte(updated), 0o644)
}

// fill replaces the text between the markers of p in doc.
func (p generatedPart) fill(doc string) (string, error) {
	start, end := "<!-- generated:"+p.name+" -->", "<!-- /generated:"+p.name+" -->"
	head, rest, ok := strings.Cut(doc, start)
	if !ok {
		return "", fmt.Errorf("no %s marker", start)
	}
	_, tail, ok := strings.Cut(rest, end)
	if !ok {
		return "", fmt.Errorf("no %s marker", end)
	}
	return head + start + "\n" + p.text() + "\n" + end + tail, nil
}
