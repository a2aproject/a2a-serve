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
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	_ "modernc.org/sqlite" // registers the sqlite driver

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/fsutil"
)

// Paths in a snapshot, relative to the .a2a directory.
const (
	pathConfig  = "config.yaml"
	pathDB      = "state/server.db"
	pathSecrets = "state/secrets"
	pathAds     = "state/ads.json"
	pathHome    = "state/home"
	// pathUserHomes holds a home per authenticated user.
	pathUserHomes = "state/homes"
)

// identityPaths hold the agent's network identity on a tailnet or an Agent
// Mesh relay, restored only when the new host takes over from the old one.
var identityPaths = []string{"state/tsnet", "state/agentmesh"}

// skippedHomeDirs are caches in the harness home, which the harness fills
// again.
var skippedHomeDirs = []string{".cache", ".npm"}

// maxCopyAttempts is how many times a file that changes while it is copied
// is copied again before the last copy is kept.
const maxCopyAttempts = 3

// snapshotter adds files to a tar archive. It copies each file to a staging
// file first, so that a copy that raced with a write of the agent can be
// discarded before it reaches the archive.
type snapshotter struct {
	tw    *tar.Writer
	stage *os.File
	log   *slog.Logger
	// afterCopy, when set, runs after each attempt to copy a file and before
	// the file is checked for changes.
	afterCopy func(name string, attempt int)
}

func newSnapshotter(tw *tar.Writer, stage *os.File, log *slog.Logger, afterCopy func(name string, attempt int)) *snapshotter {
	return &snapshotter{tw: tw, stage: stage, log: log, afterCopy: afterCopy}
}

// writeArchive writes a gzipped tar of the agent's state to w: a consistent
// copy of server.db, config.yaml, the secrets, the advertised pairings, the
// harness homes and the network identity.
func writeArchive(ctx context.Context, d agentdir.Dir, w io.Writer) error {
	if err := os.MkdirAll(d.StateDir(), 0o700); err != nil {
		return err
	}
	tmp, release, err := stageDir(d.StateDir())
	if err != nil {
		return err
	}
	defer func() {
		if rerr := errors.Join(release(), os.RemoveAll(tmp)); rerr != nil {
			slog.Warn("removing the staged backup files", "error", rerr)
		}
	}()
	stage, err := os.OpenFile(filepath.Join(tmp, "stage"), os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer closeLogged(stage)

	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	s := newSnapshotter(tw, stage, slog.Default(), nil)
	if err := s.addDB(ctx, d, tmp); err != nil {
		return err
	}
	for _, p := range []string{pathConfig, pathSecrets, pathAds} {
		if err := s.addFile(filepath.Join(d.Path(), p), p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	for _, p := range append([]string{pathHome, pathUserHomes}, identityPaths...) {
		if err := s.addTree(ctx, filepath.Join(d.Path(), p), p); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}

// stageLock is the file in a staging directory that its backup holds
// locked, so that a directory a crash left behind can be told from one in
// use.
const stageLock = "lock"

// stageDir creates a directory in stateDir to stage the files of a backup
// in, and returns it locked. It first removes the directories of
// interrupted backups, which can hold a plaintext copy of the database.
// The CLI and the server take backups at the same time, so a directory
// that another backup holds locked is kept.
func stageDir(stateDir string) (dir string, release func() error, err error) {
	err = fsutil.WithLock(filepath.Join(stateDir, "backup"), func() error {
		removeLeftoverStages(stateDir)
		if dir, err = os.MkdirTemp(stateDir, "backup-"); err != nil {
			return err
		}
		if release, err = fsutil.TryLock(filepath.Join(dir, stageLock)); err != nil {
			return errors.Join(err, os.RemoveAll(dir))
		}
		return nil
	})
	return dir, release, err
}

// removeLeftoverStages removes the staging directories in stateDir that no
// backup holds locked.
func removeLeftoverStages(stateDir string) {
	removeLeftovers(filepath.Join(stateDir, "backup-*"), func(dir string) (func() error, error) {
		return fsutil.TryLock(filepath.Join(dir, stageLock))
	})
}

// removeLeftovers removes the directories matching pattern whose lock it
// can take, keeping the ones in use. A failure is logged: it doesn't stop
// the operation that cleans up.
func removeLeftovers(pattern string, lock func(dir string) (release func() error, err error)) {
	dirs, err := filepath.Glob(pattern)
	if err != nil {
		slog.Warn("listing leftover directories", "pattern", pattern, "error", err)
		return
	}
	for _, dir := range dirs {
		release, err := lock(dir)
		if errors.Is(err, fsutil.ErrLocked) {
			continue
		}
		if err != nil {
			slog.Warn("checking a leftover directory", "dir", dir, "error", err)
			continue
		}
		if err := errors.Join(release(), os.RemoveAll(dir)); err != nil {
			slog.Warn("removing a leftover directory", "dir", dir, "error", err)
		}
	}
}

// addDB adds a copy of the database made with VACUUM INTO, which is
// consistent while the server writes to it. The copy is made in tmp, a
// directory under state/ that only the user can read.
func (s *snapshotter) addDB(ctx context.Context, d agentdir.Dir, tmp string) error {
	dbPath := d.DBPath()
	if _, err := os.Stat(dbPath); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	u := url.URL{Scheme: "file", OmitHost: true, Path: dbPath, RawQuery: "_pragma=busy_timeout(5000)"}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return err
	}
	copyPath := filepath.Join(tmp, "server.db")
	_, err = db.ExecContext(ctx, `VACUUM INTO ?`, copyPath)
	if err := errors.Join(err, db.Close()); err != nil {
		return fmt.Errorf("copying the database: %w", err)
	}
	return s.addStableFile(copyPath, pathDB)
}

// addStableFile adds a file that nothing writes to.
func (s *snapshotter) addStableFile(src, name string) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer closeLogged(f)
	info, err := f.Stat()
	if err != nil {
		return err
	}
	return s.writeEntry(info, name, info.Size(), f)
}

// addFile adds the file src that the agent may write to, opened through the
// directory it is in.
func (s *snapshotter) addFile(src, name string) (err error) {
	r, err := os.OpenRoot(filepath.Dir(src))
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, r.Close()) }()
	return s.addRootFile(r, filepath.Base(src), name)
}

// addRootFile adds the file rel in r. A file that changes while it is
// copied is copied again, up to maxCopyAttempts times. After that the last
// copy is kept and a warning names the file.
func (s *snapshotter) addRootFile(r *os.Root, rel, name string) error {
	for attempt := 1; ; attempt++ {
		info, n, stable, err := s.copyToStage(r, rel, name, attempt)
		if errors.Is(err, fsutil.ErrNotRegular) {
			if attempt < maxCopyAttempts {
				continue
			}
			return fmt.Errorf("%s is %w", name, err)
		}
		if err != nil {
			return err
		}
		if !stable && attempt < maxCopyAttempts {
			continue
		}
		if !stable {
			s.log.Warn("a file kept changing while it was backed up, the archive holds a copy that may be inconsistent",
				"file", name, "attempts", attempt)
		}
		if _, err := s.stage.Seek(0, io.SeekStart); err != nil {
			return err
		}
		return s.writeEntry(info, name, n, s.stage)
	}
}

// copyToStage copies rel in r to the staging file and reports how many
// bytes it copied and whether rel stayed the same during the copy. Both
// checks of rel go through r and don't follow a symlink at rel, so the copy
// is always of a regular file in r.
func (s *snapshotter) copyToStage(r *os.Root, rel, name string, attempt int) (info fs.FileInfo, n int64, stable bool, err error) {
	listed, err := r.Lstat(rel)
	if err != nil {
		return nil, 0, false, err
	}
	if !listed.Mode().IsRegular() {
		return nil, 0, false, fsutil.ErrNotRegular
	}
	f, err := fsutil.OpenRegular(r, rel)
	if errors.Is(err, fsutil.ErrNotRegular) {
		// rel was replaced, such as with a FIFO, between the Lstat and the
		// open.
		return nil, 0, false, fsutil.ErrNotRegular
	}
	if err != nil {
		return nil, 0, false, err
	}
	defer closeLogged(f)
	before, err := f.Stat()
	if err != nil {
		return nil, 0, false, err
	}
	if !os.SameFile(listed, before) {
		// rel was replaced between the Lstat and the open.
		return nil, 0, false, fsutil.ErrNotRegular
	}
	if err := s.stage.Truncate(0); err != nil {
		return nil, 0, false, err
	}
	if _, err := s.stage.Seek(0, io.SeekStart); err != nil {
		return nil, 0, false, err
	}
	// Reading no more than the initial size keeps a file that grows without
	// end from stalling the backup; the size check then finds the growth.
	n, err = io.Copy(s.stage, io.LimitReader(f, before.Size()))
	if err != nil {
		return nil, 0, false, err
	}
	if s.afterCopy != nil {
		s.afterCopy(name, attempt)
	}
	after, err := f.Stat()
	if err != nil {
		return nil, 0, false, err
	}
	current, err := r.Lstat(rel)
	if errors.Is(err, fs.ErrNotExist) {
		return before, n, false, nil
	}
	if err != nil {
		return nil, 0, false, err
	}
	stable = n == before.Size() &&
		after.Size() == before.Size() &&
		after.ModTime().Equal(before.ModTime()) &&
		os.SameFile(before, current)
	return before, n, stable, nil
}

// writeEntry writes a regular file entry of size bytes read from r.
func (s *snapshotter) writeEntry(info fs.FileInfo, name string, size int64, r io.Reader) error {
	hdr, err := tar.FileInfoHeader(info, "")
	if err != nil {
		return err
	}
	hdr.Name = name
	hdr.Size = size
	if err := s.tw.WriteHeader(hdr); err != nil {
		return err
	}
	_, err = io.CopyN(s.tw, r, size)
	return err
}

// addTree adds the tree at root as name. The agent can change its homes
// while the walk runs, so files are opened through root, which refuses
// symlinks that lead out of it.
func (s *snapshotter) addTree(ctx context.Context, root, name string) (err error) {
	info, err := os.Lstat(root)
	if errors.Is(err, fs.ErrNotExist) {
		// A missing root is an absent optional tree.
		return nil
	}
	if err != nil {
		return err
	}
	switch {
	case info.Mode().IsRegular():
		return s.addFile(root, name)
	case !info.IsDir():
		return s.addEntry(info, func() (string, error) { return os.Readlink(root) }, name)
	}
	r, err := os.OpenRoot(root)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, r.Close()) }()
	return fs.WalkDir(r.FS(), ".", func(rel string, e fs.DirEntry, err error) error {
		if errors.Is(err, fs.ErrNotExist) {
			// A vanished entry was deleted during the backup.
			return nil
		}
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if e.IsDir() && isHomeCache(name, rel) {
			return filepath.SkipDir
		}
		entry := path.Join(name, rel)
		info, err := e.Info()
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return s.addEntry(info, func() (string, error) { return r.Readlink(filepath.FromSlash(rel)) }, entry)
		}
		err = s.addRootFile(r, filepath.FromSlash(rel), entry)
		if errors.Is(err, fs.ErrNotExist) {
			// The file was deleted after the walk listed it.
			return nil
		}
		if errors.Is(err, fsutil.ErrNotRegular) {
			s.log.Warn("skipped a file that was replaced while it was backed up", "file", entry)
			return nil
		}
		return err
	})
}

// addEntry adds a directory or a symlink, whose target readlink returns.
func (s *snapshotter) addEntry(info fs.FileInfo, readlink func() (string, error), entry string) error {
	link := ""
	switch {
	case info.Mode()&fs.ModeSymlink != 0:
		var err error
		if link, err = readlink(); err != nil {
			return err
		}
	case !info.IsDir():
		// Sockets and other special files don't survive a move.
		return nil
	}
	hdr, err := tar.FileInfoHeader(info, link)
	if err != nil {
		return err
	}
	hdr.Name = entry
	if info.IsDir() {
		hdr.Name += "/"
	}
	return s.tw.WriteHeader(hdr)
}

// extractArchive extracts a snapshot into dst, the .a2a directory of the
// agent to restore. The network identity is extracted only with takeOver.
func extractArchive(ctx context.Context, r io.Reader, dst string, takeOver bool) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("reading the snapshot: %w", err)
	}
	tr := tar.NewReader(gz)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
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

// isHomeCache reports whether rel in the tree name is a cache directory of
// a harness home.
func isHomeCache(name, rel string) bool {
	switch name {
	case pathHome:
		return slices.Contains(skippedHomeDirs, rel)
	case pathUserHomes:
		_, dir, ok := strings.Cut(rel, "/")
		return ok && slices.Contains(skippedHomeDirs, dir)
	}
	return false
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
