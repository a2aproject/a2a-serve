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
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"strings"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
)

// commandRows are the rows that commands such as `pair list` and a start
// write before anyone uses the agent, by table: those matching the
// condition, or every row for an empty one. A row of any other table is
// agent data, so tables added later count as agent data.
var commandRows = map[string]string{
	"pairings":        "name IS 'local'",
	"tokens":          "pairing IS 'local'",
	"codes":           "pairing IS 'local'",
	"server_keys":     "",
	"messenger_state": "",
	"seen":            "",
}

// holdsAgentData reports whether the state directory of d holds data of an
// agent someone used: a database row besides those of commandRows, or a
// secret besides the backup key.
func holdsAgentData(ctx context.Context, d agentdir.Dir) (bool, error) {
	secrets, err := d.LoadSecrets()
	if err != nil {
		return false, err
	}
	for name := range secrets {
		if name != agentdir.SecretBackupKey {
			return true, nil
		}
	}
	if _, err := os.Stat(d.DBPath()); errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	u := url.URL{Scheme: "file", OmitHost: true, Path: d.DBPath(), RawQuery: "_pragma=query_only(1)"}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return false, err
	}
	defer closeLogged(db)
	rows, err := db.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		return false, fmt.Errorf("reading %s: %w", d.DBPath(), err)
	}
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return false, errors.Join(err, rows.Close())
		}
		tables = append(tables, name)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return false, err
	}
	for _, table := range tables {
		query := `SELECT EXISTS (SELECT 1 FROM "` + strings.ReplaceAll(table, `"`, `""`) + `"`
		cond, ok := commandRows[table]
		switch {
		case ok && cond == "":
			continue
		case ok:
			query += ` WHERE NOT (` + cond + `)`
		}
		var exists bool
		if err := db.QueryRowContext(ctx, query+`)`).Scan(&exists); err != nil {
			return false, fmt.Errorf("reading %s: %w", d.DBPath(), err)
		}
		if exists {
			return true, nil
		}
	}
	return false, nil
}
