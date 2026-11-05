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
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/a2aproject/a2a-serve/internal/clierr"
)

// namedArgs checks the positional arguments against the placeholders of the
// Use line of cmd: <x> is required, [<x>] is optional, and a placeholder
// that ends in ... takes any number of arguments. Its errors name the
// arguments and print the usage line as the hint.
func namedArgs(cmd *cobra.Command, args []string) error {
	var required []string
	optional, variadic := 0, false
	for _, word := range strings.Fields(cmd.Use)[1:] {
		if strings.HasSuffix(strings.TrimRight(word, "]"), "...") {
			variadic = true
		}
		switch {
		case strings.HasPrefix(word, "["):
			optional++
		case strings.HasPrefix(word, "<"):
			required = append(required, strings.TrimSuffix(word, "..."))
		}
	}
	switch {
	case len(args) < len(required):
		return usageWithSynopsis(cmd, fmt.Sprintf("%s: missing %s", cmd.CommandPath(), joinAnd(required[len(args):])))
	case !variadic && len(args) > len(required)+optional:
		return usageWithSynopsis(cmd, fmt.Sprintf("%s: unexpected argument %q", cmd.CommandPath(), args[len(required)+optional]))
	}
	return nil
}

func usageWithSynopsis(cmd *cobra.Command, msg string) *clierr.Error {
	err := clierr.Usage(msg)
	err.Hint = fmt.Sprintf("usage: %s", cmd.UseLine())
	return err
}

func joinAnd(names []string) string {
	if len(names) < 2 {
		return strings.Join(names, "")
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}
