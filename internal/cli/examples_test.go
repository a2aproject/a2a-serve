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
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestCommandExamplesParse(t *testing.T) {
	t.Parallel()
	var walk func(cmd *cobra.Command)
	walk = func(cmd *cobra.Command) {
		for _, line := range exampleLines(cmd.Example) {
			t.Run(line, func(t *testing.T) {
				t.Parallel()
				root := newRootCmd(&globalConfig{})
				args := splitExample(line)
				if len(args) == 0 || args[0] != "a2a-serve" {
					return
				}
				target, rest, err := root.Find(args[1:])
				if err != nil {
					t.Fatalf("root.Find(%q) error = %v", line, err)
				}
				if err := target.ParseFlags(rest); err != nil {
					t.Fatalf("%s.ParseFlags(%q) error = %v", target.CommandPath(), line, err)
				}
				if err := target.ValidateArgs(target.Flags().Args()); err != nil {
					t.Fatalf("%s.ValidateArgs(%q) error = %v", target.CommandPath(), line, err)
				}
			})
		}
		for _, sub := range cmd.Commands() {
			walk(sub)
		}
	}
	walk(newRootCmd(&globalConfig{}))
}

// exampleLines joins the continuation lines of an Example block.
func exampleLines(example string) []string {
	var lines []string
	var cur string
	for l := range strings.SplitSeq(example, "\n") {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		if head, ok := strings.CutSuffix(l, `\`); ok {
			cur += head + " "
			continue
		}
		lines = append(lines, cur+l)
		cur = ""
	}
	return lines
}

func splitExample(line string) []string {
	var args []string
	var cur strings.Builder
	var quote rune
	inArg := false
	for _, r := range line {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
				continue
			}
			cur.WriteRune(r)
		case r == '\'' || r == '"':
			quote, inArg = r, true
		case r == ' ' || r == '\t':
			if inArg {
				args = append(args, cur.String())
				cur.Reset()
				inArg = false
			}
		default:
			cur.WriteRune(r)
			inArg = true
		}
	}
	if inArg {
		args = append(args, cur.String())
	}
	return args
}
