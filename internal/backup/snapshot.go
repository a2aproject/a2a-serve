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

package backup

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	_ "modernc.org/sqlite" // registers the sqlite driver

	"github.com/a2aproject/a2a-serve/internal/agentdir"
)

// Paths in a snapshot, relative to the .a2a directory.
const (
	pathConfig  = "config.yaml"
	pathDB      = "state/server.db"
	pathSecrets = "state/secrets"
	pathAds     = "state/ads.json"
	pathHome    = "state/home"
)

// identityPaths hold the agent's network identity on a tailnet or an Agent
// Mesh relay, restored only when the new host takes over from the old one.
var identityPaths = []string{"state/tsnet", "state/agentmesh"}

// skippedHomeDirs are caches in the harness home, which the harness fills
// again.
var skippedHomeDirs = []string{".cache", ".npm"}

// writeArchive writes a gzipped tar of the agent's state to w: a consistent
// copy of server.db, config.yaml, the secrets, the advertised pairings, the
// harness home and the network identity.
func writeArchive(ctx context.Context, d agentdir.Dir, w io.Writer) error {
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	if err := addDB(ctx, tw, d.DBPath()); err != nil {
		return err
	}
	for _, p := range []string{pathConfig, pathSecrets, pathAds} {
		if err := addFile(tw, filepath.Join(d.Path(), p), p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	for _, p := range append([]string{pathHome}, identityPaths...) {
		if err := addTree(ctx, tw, filepath.Join(d.Path(), p), p); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}

// addDB adds a copy of the database made with VACUUM INTO, which is
// consistent while the server writes to it.
func addDB(ctx context.Context, tw *tar.Writer, dbPath string) error {
	if _, err := os.Stat(dbPath); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	tmp, err := os.MkdirTemp("", "a2a-backup")
	if err != nil {
		return err
	}
	defer func() {
		if err := os.RemoveAll(tmp); err != nil {
			slog.Warn("removing the database copy", "error", err)
		}
	}()
	db, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return err
	}
	copyPath := filepath.Join(tmp, "server.db")
	_, err = db.ExecContext(ctx, `VACUUM INTO ?`, copyPath)
	if err := errors.Join(err, db.Close()); err != nil {
		return fmt.Errorf("copying the database: %w", err)
	}
	return addFile(tw, copyPath, pathDB)
}

func addFile(tw *tar.Writer, src, name string) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer closeLogged(f)
	info, err := f.Stat()
	if err != nil {
		return err
	}
	hdr, err := tar.FileInfoHeader(info, "")
	if err != nil {
		return err
	}
	hdr.Name = name
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	_, err = io.Copy(tw, f)
	return err
}

func addTree(ctx context.Context, tw *tar.Writer, root, name string) error {
	return filepath.WalkDir(root, func(p string, e fs.DirEntry, err error) error {
		if errors.Is(err, fs.ErrNotExist) {
			// A missing root is an absent optional tree, a vanished
			// entry was deleted during the backup.
			return nil
		}
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		if e.IsDir() && slices.Contains(skippedHomeDirs, rel) && name == pathHome {
			return filepath.SkipDir
		}
		entry := path.Join(name, filepath.ToSlash(rel))
		info, err := e.Info()
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		switch {
		case info.Mode().IsRegular():
			if err := addFile(tw, p, entry); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
			return nil
		case info.IsDir(), info.Mode()&fs.ModeSymlink != 0:
			link := ""
			if info.Mode()&fs.ModeSymlink != 0 {
				if link, err = os.Readlink(p); err != nil {
					return err
				}
			}
			hdr, err := tar.FileInfoHeader(info, link)
			if err != nil {
				return err
			}
			hdr.Name = entry
			if info.IsDir() {
				hdr.Name += "/"
			}
			return tw.WriteHeader(hdr)
		}
		// Sockets and other special files don't survive a move.
		return nil
	})
}

// extractArchive extracts a snapshot into dst, the .a2a directory of the
// agent to restore. The network identity is extracted only with takeOver.
func extractArchive(r io.Reader, dst string, takeOver bool) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("reading the snapshot: %w", err)
	}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("reading the snapshot: %w", err)
		}
		name := path.Clean(hdr.Name)
		if !fs.ValidPath(name) {
			return fmt.Errorf("the snapshot has an invalid path %q", hdr.Name)
		}
		if !takeOver && isIdentity(name) {
			continue
		}
		target := filepath.Join(dst, filepath.FromSlash(name))
		if err := noSymlinkParents(dst, name); err != nil {
			return err
		}
		if err := extractEntry(tr, hdr, target); err != nil {
			return err
		}
	}
	return gz.Close()
}

func isIdentity(name string) bool {
	return slices.ContainsFunc(identityPaths, func(p string) bool {
		return name == p || strings.HasPrefix(name, p+"/")
	})
}

func extractEntry(tr *tar.Reader, hdr *tar.Header, target string) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return err
	}
	switch hdr.Typeflag {
	case tar.TypeDir:
		return os.MkdirAll(target, 0o700)
	case tar.TypeSymlink:
		return os.Symlink(hdr.Linkname, target)
	case tar.TypeReg:
		f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, hdr.FileInfo().Mode().Perm()&0o700)
		if err != nil {
			return err
		}
		_, err = io.Copy(f, tr)
		return errors.Join(err, f.Close())
	}
	return nil
}

// noSymlinkParents makes sure that no directory on the way to name in root
// is a symlink, so that an entry can't be written outside root.
func noSymlinkParents(root, name string) error {
	dir := root
	parts := strings.Split(name, "/")
	for _, p := range parts[:len(parts)-1] {
		dir = filepath.Join(dir, p)
		info, err := os.Lstat(dir)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return fmt.Errorf("the snapshot writes %q through a symlink", name)
		}
	}
	return nil
}

func closeLogged(c io.Closer) {
	if err := c.Close(); err != nil {
		slog.Warn("closing a file", "error", err)
	}
}
