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

// guidePath is the config guide, relative to the reference directory.
var guidePath = filepath.Join("..", "..", "guide", "config.md")

const (
	liveStart = "<!-- generated:live-settings -->"
	liveEnd   = "<!-- /generated:live-settings -->"
)

// liveSettingsText says which settings a running agent applies at once.
func liveSettingsText() string {
	names := agentdir.LiveSettings()
	flags := make([]string, len(names))
	for i, name := range names {
		flags[i] = "`--" + name + "`"
	}
	list := strings.Join(flags[:len(flags)-1], ", ") + " and " + flags[len(flags)-1]
	return "A running agent applies " + list + " at once, and messengers too. Other settings apply after `a2a-serve restart`, or pass `--restart` to `config set`."
}

// updateGuide fills the live settings of the config guide at path. A missing
// guide is skipped, so the reference can be written anywhere.
func updateGuide(path string) error {
	doc, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	updated, err := fillLiveSettings(string(doc))
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if updated == string(doc) {
		return nil
	}
	return os.WriteFile(path, []byte(updated), 0o644)
}

// fillLiveSettings replaces the text between the live settings markers of doc.
func fillLiveSettings(doc string) (string, error) {
	head, rest, ok := strings.Cut(doc, liveStart)
	if !ok {
		return "", fmt.Errorf("no %s marker", liveStart)
	}
	_, tail, ok := strings.Cut(rest, liveEnd)
	if !ok {
		return "", fmt.Errorf("no %s marker", liveEnd)
	}
	return head + liveStart + "\n" + liveSettingsText() + "\n" + liveEnd + tail, nil
}
