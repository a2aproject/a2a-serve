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
	"io"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/a2aproject/a2a-serve/internal/clidoc"
	"github.com/a2aproject/a2a-serve/internal/output"
)

// WriteReference writes the markdown command reference to dir.
func WriteReference(dir string) error {
	return clidoc.Write(dir, referenceRoot(), referenceOptions)
}

// ReferencePages renders the markdown command reference, keyed by file name.
func ReferencePages() map[string]string {
	return clidoc.Pages(referenceRoot(), referenceOptions)
}

func referenceRoot() *cobra.Command {
	return newRootCmd(&globalConfig{Printer: output.NewPrinter(io.Discard, output.ModeText)})
}

var referenceOptions = clidoc.Options{
	Section: func(f *pflag.Flag) string {
		if s := f.Annotations[sectionAnnotation]; len(s) > 0 {
			return s[0]
		}
		return ""
	},
	Sections: sections,
}
