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
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/google/shlex"
	"gopkg.in/yaml.v3"

	"github.com/a2aproject/a2a-serve/internal/credstore"
)

// helperEntry is a credential helper of the a2a CLI config.
type helperEntry struct {
	Scope   string   `json:"scope"`
	Command []string `json:"command"`
}

func serveHelper() helperEntry {
	return helperEntry{Command: strings.Fields(credstore.HelperCommand)}
}

// isServe reports whether e runs `a2a-serve credential`.
func (e helperEntry) isServe() bool {
	return len(e.Command) >= 2 && filepath.Base(e.Command[0]) == "a2a-serve" && e.Command[1] == "credential"
}

func (e helperEntry) String() string {
	s := strings.Join(e.Command, " ")
	if e.Scope != "" {
		s = e.Scope + "=" + s
	}
	return s
}

func joinHelpers(helpers []helperEntry) string {
	specs := make([]string, len(helpers))
	for i, h := range helpers {
		specs[i] = h.String()
	}
	return strings.Join(specs, ", ")
}

// parseHelpers reads the credential-helper value of the a2a CLI config as
// the a2a CLI does: one "[scope=]path", or a list of {scope, command}. It
// also reports whether v used the "[scope=]command args" form of earlier
// a2a builds, which it converts. A single value in that form is converted
// only when it runs `a2a-serve credential`, since a path may have spaces.
func parseHelpers(v any) ([]helperEntry, bool, error) {
	switch v := v.(type) {
	case string:
		if e, err := parseOldHelper(v); err == nil && e.isServe() {
			return []helperEntry{e}, true, nil
		}
		return []helperEntry{{Command: []string{strings.TrimSpace(v)}}}, false, nil
	case []any:
		helpers := make([]helperEntry, 0, len(v))
		old := false
		for _, item := range v {
			var e helperEntry
			var err error
			switch item := item.(type) {
			case string:
				e, err = parseOldHelper(item)
				old = true
			case map[string]any:
				e, err = parseHelperEntry(item)
			default:
				err = fmt.Errorf("helper %v: want {scope, command}", item)
			}
			if err != nil {
				return nil, false, err
			}
			helpers = append(helpers, e)
		}
		return helpers, old, nil
	}
	return nil, false, fmt.Errorf("want a list of {scope, command}, got %v", v)
}

// parseHelperEntry decodes item like the a2a CLI, which rejects unknown
// fields.
func parseHelperEntry(item map[string]any) (helperEntry, error) {
	data, err := json.Marshal(item)
	if err != nil {
		return helperEntry{}, fmt.Errorf("helper %v: %w", item, err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var e helperEntry
	if err := dec.Decode(&e); err != nil {
		return helperEntry{}, fmt.Errorf("helper %s: want {scope, command}: %w", data, err)
	}
	if len(e.Command) == 0 || e.Command[0] == "" {
		return helperEntry{}, fmt.Errorf("helper %s: no command", data)
	}
	return e, nil
}

// parseOldHelper parses "[scope=]command args", split like a shell would.
func parseOldHelper(spec string) (helperEntry, error) {
	var e helperEntry
	rest := strings.TrimSpace(spec)
	if scope, cmd, ok := strings.Cut(rest, "="); ok && scope != "" && !strings.ContainsAny(scope, " \t/\\\"'") {
		e.Scope, rest = scope, cmd
	}
	args, err := shlex.Split(rest)
	if err != nil {
		return helperEntry{}, fmt.Errorf("helper %q: %w", spec, err)
	}
	if len(args) == 0 {
		return helperEntry{}, fmt.Errorf("helper %q: no command", spec)
	}
	e.Command = args
	return e, nil
}

// helpersNode renders helpers as a2a-cli documents them:
//
//   - scope: "*.example.com"
//     command: [helper, arg]
func helpersNode(helpers []helperEntry) *yaml.Node {
	seq := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
	for _, h := range helpers {
		m := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		if h.Scope != "" {
			m.Content = append(m.Content, scalar("!!str", "scope"), scalar("!!str", h.Scope))
		}
		cmd := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq", Style: yaml.FlowStyle}
		for _, arg := range h.Command {
			cmd.Content = append(cmd.Content, scalar("!!str", arg))
		}
		m.Content = append(m.Content, scalar("!!str", "command"), cmd)
		seq.Content = append(seq.Content, m)
	}
	return seq
}
