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

package fsutil

import (
	"os"
	"path/filepath"
)

// ResolvePath returns p with symlinks resolved. Of a path that doesn't
// exist, it resolves the part that does, dangling symlinks included.
func ResolvePath(p string) string {
	return resolveFrom(filepath.Clean(p), 0)
}

// MaxSymlinks bounds the symlinks that a path walk follows by hand.
const MaxSymlinks = 40

func resolveFrom(p string, hops int) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	if target, err := os.Readlink(p); err == nil && hops < MaxSymlinks {
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(p), target)
		}
		return resolveFrom(filepath.Clean(target), hops+1)
	}
	parent := filepath.Dir(p)
	if parent == p {
		return p
	}
	return filepath.Join(resolveFrom(parent, hops), filepath.Base(p))
}
