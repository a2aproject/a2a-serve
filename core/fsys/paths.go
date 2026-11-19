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

package fsys

import (
	"fmt"
	"io/fs"
	"path"
	"strings"
)

var errInvalidPath = fmt.Errorf("%w: want a cleaned path relative to the root, without NUL", fs.ErrInvalid)

// checkPath refuses a path outside the rules of this package.
func checkPath(op, p string) error {
	if p == "" || strings.ContainsRune(p, 0) || path.IsAbs(p) || p != path.Clean(p) || p == ".." || strings.HasPrefix(p, "../") {
		return &fs.PathError{Op: op, Path: p, Err: errInvalidPath}
	}
	return nil
}

// checkEntry refuses a path that isn't an entry below the root.
func checkEntry(op, p string) error {
	if err := checkPath(op, p); err != nil {
		return err
	}
	if p == "." {
		return &fs.PathError{Op: op, Path: p, Err: fmt.Errorf("%w: the root", fs.ErrInvalid)}
	}
	return nil
}
