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

package download

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"

	"github.com/a2aproject/a2a-serve/internal/fsutil"
)

// Release is a pinned release of a program, published as archives whose
// SHA-256 a2a-serve knows.
type Release struct {
	// Program names the program in progress lines and errors.
	Program string
	Version string
	// CacheDir names the directory of this version in the cache.
	CacheDir string
	// Binary returns the name of the program in the archive for goos.
	Binary func(goos string) string
	// Archive returns the name of the archive for goos and goarch.
	Archive func(goos, goarch string) (string, error)
	// Archives maps archive names to their SHA-256.
	Archives map[string]string
	// URL returns where an archive is downloaded from.
	URL func(name string) string
}

// Fetcher gets a pinned release for one platform.
type Fetcher struct {
	rel Release
	// cacheDir keeps downloaded binaries, one directory per version.
	cacheDir string
	// override is a binary to use instead of a download.
	override string
	hc       *http.Client
	// goos and goarch select the release archive.
	goos, goarch string
	logf         func(string, ...any)
}

// NewFetcher returns a fetcher of rel for goos and goarch that caches it in
// cacheDir, or uses override when it is set.
func NewFetcher(rel Release, cacheDir, override string, hc *http.Client, goos, goarch string, logf func(string, ...any)) *Fetcher {
	return &Fetcher{rel: rel, cacheDir: cacheDir, override: override, hc: hc, goos: goos, goarch: goarch, logf: logf}
}

// Path returns where the pinned binary is cached.
func (f *Fetcher) Path() string {
	return filepath.Join(f.cacheDir, f.rel.CacheDir, f.rel.Binary(f.goos))
}

// Location returns where the pinned binary is: the override, or the cache
// it is downloaded to.
func (f *Fetcher) Location() string {
	if f.override != "" {
		return f.override
	}
	return f.Path()
}

// Get returns the path of the pinned binary: the override, the cached
// binary, or one downloaded from the release and checked against its
// SHA-256.
func (f *Fetcher) Get(ctx context.Context) (string, error) {
	if f.override != "" {
		return f.override, nil
	}
	path := f.Path()
	if _, err := os.Stat(path); err == nil {
		return path, nil
	}
	name, err := f.rel.Archive(f.goos, f.goarch)
	if err != nil {
		return "", err
	}
	sum, ok := f.rel.Archives[name]
	if !ok {
		return "", fmt.Errorf("no checksum of %s in this a2a-serve", name)
	}
	f.progress("… downloading %s %s", f.rel.Program, f.rel.Version)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	archive, err := os.CreateTemp(filepath.Dir(path), name+".*")
	if err != nil {
		return "", err
	}
	defer func() {
		if err := errors.Join(archive.Close(), os.Remove(archive.Name())); err != nil && !errors.Is(err, os.ErrClosed) {
			slog.Debug("removing the downloaded archive", "error", err)
		}
	}()
	if _, err := ToFile(ctx, f.hc, f.rel.URL(name), archive, sum); err != nil {
		return "", fmt.Errorf("downloading %s: %w", f.rel.Program, err)
	}
	data, err := Extract(archive, name, f.rel.Binary(f.goos))
	if err != nil {
		return "", fmt.Errorf("extracting %s from %s: %w", f.rel.Program, name, err)
	}
	if err := fsutil.WriteAtomic(path, data, 0o755); err != nil {
		return "", err
	}
	return path, nil
}

func (f *Fetcher) progress(format string, args ...any) {
	if f.logf != nil {
		f.logf(format, args...)
	}
}
