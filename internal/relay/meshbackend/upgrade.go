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
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	_ "modernc.org/sqlite" // registers the sqlite driver

	"github.com/a2aproject/a2a-serve/internal/agentmesh"
	"github.com/a2aproject/a2a-serve/internal/relaydir"
)

// keptBackups is how many backups taken before upgrades a relay keeps.
const keptBackups = 3

// UpgradeResult says what [PrepareData] did.
type UpgradeResult struct {
	// From is the Agent Mesh version that ran on the data before, or "" on
	// the first run.
	From string `json:"from,omitempty"`
	To   string `json:"to"`
	// Backup is the backup taken before the upgrade.
	Backup string `json:"backup,omitempty"`
}

// Upgraded reports whether the data moves to a newer Agent Mesh version.
func (r UpgradeResult) Upgraded() bool { return r.From != "" && r.From != r.To }

// PrepareData readies the relay's data for the pinned Agent Mesh version,
// with agentmesh-one stopped and the relay's lock held. Agent Mesh migrates
// its database forward and an older version can't use it again, so an
// older pin is refused, and a newer one first backs up a SQLite database.
// The version is recorded before the new version starts.
func PrepareData(ctx context.Context, d relaydir.Dir, pin string, postgres bool, now time.Time) (UpgradeResult, error) {
	res := UpgradeResult{To: pin}
	rec, err := d.RecordedVersion()
	if err != nil {
		return res, err
	}
	res.From = rec
	if rec == pin {
		return res, nil
	}
	if rec == "" && !postgres {
		// Data without a record comes from an unknown version, so it is
		// backed up before the pinned version migrates it.
		if res.Backup, err = backup(ctx, d, "unknown", now); err != nil {
			return res, fmt.Errorf("backing up the relay's database: %w", err)
		}
	}
	if rec != "" {
		cmp, err := compareVersions(pin, rec)
		if err != nil {
			return res, err
		}
		if cmp < 0 {
			return res, downgradeError(d, pin, rec)
		}
		if !postgres {
			if res.Backup, err = backup(ctx, d, rec, now); err != nil {
				return res, fmt.Errorf("backing up the relay before upgrading Agent Mesh: %w", err)
			}
		}
	}
	return res, d.RecordVersion(pin)
}

func compareVersions(a, b string) (int, error) {
	va, err := agentmesh.Parse(a)
	if err != nil {
		return 0, err
	}
	vb, err := agentmesh.Parse(b)
	if err != nil {
		return 0, fmt.Errorf("the recorded Agent Mesh version: %w", err)
	}
	return va.Compare(vb), nil
}

func downgradeError(d relaydir.Dir, pin, rec string) error {
	msg := fmt.Sprintf("the relay's data was last used by Agent Mesh %s, and this a2a-serve runs the older %s, which can't use it; run the relay with a newer a2a-serve", rec, pin)
	backups, err := listBackups(d)
	if err != nil {
		return errors.New(msg)
	}
	for _, b := range slices.Backward(backups) {
		if strings.HasPrefix(filepath.Base(b), "agentmesh-"+pin+"-") {
			return fmt.Errorf("%s, or go back to the backup taken before the upgrade: replace %s with %s and write %s to %s", msg, d.MeshDBPath(), b, pin, d.VersionPath())
		}
	}
	return errors.New(msg)
}

// backup copies the SQLite database with VACUUM INTO, and keeps the newest
// keptBackups backups.
func backup(ctx context.Context, d relaydir.Dir, version string, now time.Time) (string, error) {
	if _, err := os.Stat(d.MeshDBPath()); errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err := os.MkdirAll(d.BackupsDir(), 0o700); err != nil {
		return "", err
	}
	dst := filepath.Join(d.BackupsDir(), fmt.Sprintf("agentmesh-%s-%s.db", version, now.UTC().Format("20060102T150405Z")))
	u := url.URL{Scheme: "file", OmitHost: true, Path: d.MeshDBPath(), RawQuery: "_pragma=busy_timeout(5000)"}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return "", err
	}
	_, err = db.ExecContext(ctx, `VACUUM INTO ?`, dst)
	if err := errors.Join(err, db.Close()); err != nil {
		return "", err
	}
	if err := os.Chmod(dst, 0o600); err != nil {
		return "", err
	}
	backups, err := listBackups(d)
	if err != nil {
		return "", err
	}
	for len(backups) > keptBackups {
		if err := os.Remove(backups[0]); err != nil {
			return "", err
		}
		backups = backups[1:]
	}
	return dst, nil
}

// listBackups returns the backups, oldest first.
func listBackups(d relaydir.Dir) ([]string, error) {
	entries, err := os.ReadDir(d.BackupsDir())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	type entry struct {
		path string
		mod  time.Time
	}
	var es []entry
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "agentmesh-") || !strings.HasSuffix(e.Name(), ".db") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			return nil, err
		}
		es = append(es, entry{filepath.Join(d.BackupsDir(), e.Name()), info.ModTime()})
	}
	slices.SortFunc(es, func(a, b entry) int { return a.mod.Compare(b.mod) })
	out := make([]string, len(es))
	for i, e := range es {
		out[i] = e.path
	}
	return out, nil
}

// LastBackup returns the newest backup taken before an upgrade, or "".
func LastBackup(d relaydir.Dir) (string, time.Time, error) {
	backups, err := listBackups(d)
	if err != nil || len(backups) == 0 {
		return "", time.Time{}, err
	}
	last := backups[len(backups)-1]
	info, err := os.Stat(last)
	if err != nil {
		return "", time.Time{}, err
	}
	return last, info.ModTime(), nil
}
