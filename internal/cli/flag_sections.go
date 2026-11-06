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
	"io"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
)

// sectionAnnotation is the flag annotation naming the help section of a flag.
const sectionAnnotation = "a2a-serve-section"

// bareValueAnnotation marks a flag whose bare form's value, NoOptDefVal, is
// left out of the usage line.
const bareValueAnnotation = "a2a-serve-hide-bare-value"

const sectionRun = "Run (not saved)"

var sections = []string{
	agentdir.SectionGeneral, agentdir.SectionWorkspace, agentdir.SectionSandbox, agentdir.SectionNetwork,
	agentdir.SectionAccess, agentdir.SectionApprovals, sectionRun,
}

// setSection puts the named flags in a help section.
func setSection(f *pflag.FlagSet, section string, names ...string) {
	for _, name := range names {
		must(f.SetAnnotation(name, sectionAnnotation, []string{section}))
	}
}

// hideBareValue leaves the value of the bare form of the named flag, which
// pflag prints as [="value"], out of the usage line. The usage text says what
// the bare form does.
func hideBareValue(f *pflag.FlagSet, name string) {
	must(f.SetAnnotation(name, bareValueAnnotation, []string{"true"}))
}

// sectionedUsage prints the usage of a command whose flags are in help
// sections, followed by the flags in none.
func sectionedUsage(c *cobra.Command) error {
	var sb strings.Builder
	sb.WriteString("Usage:\n  " + c.UseLine())
	if len(c.Aliases) > 0 {
		sb.WriteString("\n\nAliases:\n  " + c.NameAndAliases())
	}
	if c.HasExample() {
		sb.WriteString("\n\nExamples:\n" + c.Example)
	}
	bySection := map[string]*pflag.FlagSet{}
	other := pflag.NewFlagSet("other", pflag.ContinueOnError)
	c.LocalFlags().VisitAll(func(fl *pflag.Flag) {
		if len(fl.Annotations[bareValueAnnotation]) > 0 {
			shown := *fl
			shown.NoOptDefVal = ""
			fl = &shown
		}
		section := other
		if s := fl.Annotations[sectionAnnotation]; len(s) > 0 {
			if bySection[s[0]] == nil {
				bySection[s[0]] = pflag.NewFlagSet(s[0], pflag.ContinueOnError)
			}
			section = bySection[s[0]]
		}
		section.AddFlag(fl)
	})
	writeFlags := func(title string, fs *pflag.FlagSet) {
		if usages := fs.FlagUsages(); strings.TrimSpace(usages) != "" {
			fmt.Fprintf(&sb, "\n\n%s:\n%s", title, strings.TrimRight(usages, " \n"))
		}
	}
	for _, s := range sections {
		if fs := bySection[s]; fs != nil {
			writeFlags(s, fs)
		}
	}
	writeFlags("Flags", other)
	writeFlags("Global Flags", c.InheritedFlags())
	sb.WriteString("\n")
	_, err := io.WriteString(c.OutOrStderr(), sb.String())
	return err
}
