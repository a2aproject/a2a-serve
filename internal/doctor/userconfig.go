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

package doctor

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"

	"github.com/a2aproject/a2a-serve/internal/fsutil"
)

// UserConfigPath returns the path of the a2a CLI config under home.
func UserConfigPath(home string) string {
	return filepath.Join(home, ".config", "a2a-cli", "config.yaml")
}

// readUserConfig returns the top-level values of the a2a CLI config at
// path, or nil if it doesn't exist.
func readUserConfig(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var values map[string]any
	if err := yaml.Unmarshal(data, &values); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	return values, nil
}

// SetUserConfigKey sets key to value in the YAML mapping at path, creating
// the file if needed. Other keys, their order and comments are kept, and an
// existing key is replaced rather than added again. It reports whether the
// file changed.
func SetUserConfigKey(path, key string, value *yaml.Node) (bool, error) {
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return false, fmt.Errorf("parsing %s: %w", path, err)
	}
	if doc.Kind == 0 {
		doc = yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}}
	}
	m := doc.Content[0]
	if m.Kind != yaml.MappingNode {
		return false, fmt.Errorf("%s is not a YAML mapping", path)
	}
	if !setMappingKey(m, key, value) {
		return false, nil
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return false, err
	}
	if err := enc.Close(); err != nil {
		return false, err
	}
	perm := os.FileMode(0o600)
	if info, err := os.Stat(path); err == nil {
		perm = info.Mode().Perm()
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, err
	}
	if err := fsutil.WriteAtomic(path, buf.Bytes(), perm); err != nil {
		return false, err
	}
	return true, nil
}

// setMappingKey sets the first key of m to value, or appends it, and
// reports whether m changed. A flow-style mapping, such as the empty {},
// turns into a block mapping when it gains a key.
func setMappingKey(m *yaml.Node, key string, value *yaml.Node) bool {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value != key {
			continue
		}
		cur := m.Content[i+1]
		if cur.Kind == value.Kind && cur.Value == value.Value && len(cur.Content) == 0 && len(value.Content) == 0 {
			return false
		}
		m.Content[i+1] = value
		return true
	}
	m.Style = 0
	m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, value)
	return true
}

func scalar(tag, value string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: tag, Value: value}
}
