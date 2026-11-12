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

package meshbackend

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/a2aproject/a2a-serve/internal/agentmesh"
	"github.com/a2aproject/a2a-serve/internal/download"
	"github.com/a2aproject/a2a-serve/internal/fsutil"
)

// BinaryEnv names an agentmesh-one to run in place of the pinned release,
// such as the one in the relay image.
const BinaryEnv = "A2A_SERVE_AGENTMESH_ONE"

// Fetcher gets the pinned agentmesh-one.
type Fetcher struct {
	// cacheDir keeps downloaded binaries, one directory per version.
	cacheDir string
	// override is the binary of [BinaryEnv], used instead of a download.
	override string
	hc       *http.Client
	// goos and goarch select the release archive.
	goos, goarch string
	// archives maps archive names to their SHA-256.
	archives map[string]string
	// url returns where an archive is downloaded from.
	url  func(name string) string
	logf func(string, ...any)
}

// NewFetcher returns a fetcher of the pinned release for this platform.
func NewFetcher(cacheDir, override string, hc *http.Client, goos, goarch string, logf func(string, ...any)) *Fetcher {
	return &Fetcher{
		cacheDir: cacheDir, override: override, hc: hc, goos: goos, goarch: goarch,
		archives: agentmesh.Archives, url: agentmesh.ArchiveURL, logf: logf,
	}
}

func (f *Fetcher) binaryName() string {
	if f.goos == "windows" {
		return "agentmesh-one.exe"
	}
	return "agentmesh-one"
}

// Path returns where the pinned binary is cached.
func (f *Fetcher) Path() string {
	return filepath.Join(f.cacheDir, "agentmesh-"+agentmesh.Version, f.binaryName())
}

// Location returns where the pinned agentmesh-one is: the override, or the
// cache it is downloaded to.
func (f *Fetcher) Location() string {
	if f.override != "" {
		return f.override
	}
	return f.Path()
}

// Get returns the path of the pinned agentmesh-one: the override, the
// cached binary, or one downloaded from the release and checked against its
// SHA-256.
func (f *Fetcher) Get(ctx context.Context) (string, error) {
	if f.override != "" {
		return f.override, nil
	}
	path := f.Path()
	if _, err := os.Stat(path); err == nil {
		return path, nil
	}
	name, err := agentmesh.ArchiveName(f.goos, f.goarch)
	if err != nil {
		return "", err
	}
	sum, ok := f.archives[name]
	if !ok {
		return "", fmt.Errorf("no checksum of %s in this a2a-serve", name)
	}
	f.progress("… downloading agentmesh-one %s", agentmesh.Version)
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
	if err := f.download(ctx, f.url(name), archive, sum); err != nil {
		return "", err
	}
	data, err := extract(archive, name, f.binaryName())
	if err != nil {
		return "", fmt.Errorf("extracting agentmesh-one from %s: %w", name, err)
	}
	if err := fsutil.WriteAtomic(path, data, 0o755); err != nil {
		return "", err
	}
	return path, nil
}

func (f *Fetcher) download(ctx context.Context, url string, dst *os.File, want string) error {
	if _, err := download.ToFile(ctx, f.hc, url, dst, want); err != nil {
		return fmt.Errorf("downloading agentmesh-one: %w", err)
	}
	return nil
}

func extract(archive *os.File, archiveName, binary string) ([]byte, error) {
	return download.Extract(archive, archiveName, binary)
}

func (f *Fetcher) progress(format string, args ...any) {
	if f.logf != nil {
		f.logf(format, args...)
	}
}

// BinaryVersion returns the version agentmesh-one at path reports.
func BinaryVersion(ctx context.Context, path string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "--version").Output()
	if err != nil {
		return "", fmt.Errorf("running %s --version: %w", path, err)
	}
	// It prints "agentmesh-one version v0.2.0".
	fields := strings.Fields(string(out))
	if len(fields) == 0 {
		return "", fmt.Errorf("%s --version printed nothing", path)
	}
	return fields[len(fields)-1], nil
}

// CheckBinary checks that agentmesh-one at path is the pinned version.
func CheckBinary(ctx context.Context, path string) error {
	v, err := BinaryVersion(ctx, path)
	if err != nil {
		return err
	}
	if v != agentmesh.Version {
		return fmt.Errorf("%s is Agent Mesh %s; this a2a-serve runs %s", path, v, agentmesh.Version)
	}
	return nil
}
