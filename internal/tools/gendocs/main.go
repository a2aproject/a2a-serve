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

// Command gendocs writes the a2a-serve command reference:
//
//	go run ./internal/tools/gendocs [dir]
//
// dir defaults to docs/reference/commands. It also fills the generated parts
// of the guides.
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/a2aproject/a2a-serve/internal/cli"
)

func run(dir string) error {
	if err := cli.WriteReference(dir); err != nil {
		return err
	}
	return updateGuide(filepath.Join(dir, guidePath))
}

// DefaultDir is where the reference lives, relative to the repository root.
const DefaultDir = "docs/reference/commands"

func main() {
	dir := DefaultDir
	if len(os.Args) > 1 {
		dir = os.Args[1]
	}
	if err := run(dir); err != nil {
		fmt.Fprintf(os.Stderr, "gendocs: %v\n", err)
		os.Exit(1)
	}
}
