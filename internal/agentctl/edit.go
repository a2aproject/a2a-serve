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

package agentctl

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/controlapi"
	"github.com/a2aproject/a2a-serve/internal/fsutil"
)

// errorPrefix marks the lines Edit puts on top of the file to show why the
// last edit was rejected, and notePrefix the line that names a resumed draft.
const (
	errorPrefix = "# ERROR: "
	notePrefix  = "# NOTE: "
)

// Edit lets edit change a copy of config.yaml, like crontab -e, or the
// draft of an earlier rejected edit if there is one. A valid result is
// applied as by [Apply]. For an invalid one, retry decides whether to edit
// again, with the error on top of the file. If config.yaml changed since
// the edit, or its draft, started from it, the edit is kept as the draft
// and retry decides whether to edit the current file. A result that
// equals config.yaml returns an empty result.
func Edit(ctx context.Context, d agentdir.Dir, client *controlapi.Client, edit func(path string) error, retry func(error) (bool, error)) (*Result, error) {
	cur, err := os.ReadFile(d.ConfigPath())
	if err != nil {
		return nil, err
	}
	base, content, err := startOf(d, cur)
	if err != nil {
		return nil, err
	}
	tmp, err := os.CreateTemp(d.Path(), "config-*.yaml")
	if err != nil {
		return nil, err
	}
	path := tmp.Name()
	defer func() {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			slog.Warn("removing the edited copy of config.yaml", "path", path, "error", err)
		}
	}()
	if err := tmp.Close(); err != nil {
		return nil, err
	}
	drafted := false
	for {
		if err := os.WriteFile(path, content, 0o600); err != nil {
			return nil, err
		}
		if err := edit(path); err != nil {
			return nil, err
		}
		edited, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		edited = stripErrors(edited)
		if bytes.Equal(edited, cur) {
			if !drafted {
				removeDraft(d)
			}
			return withUnknownKeys(&Result{Path: d.ConfigPath()}, edited), nil
		}
		res, err := Replace(ctx, d, client, edited, base, false)
		var se *agentdir.SettingsError
		if err == nil {
			removeDraft(d)
			return withUnknownKeys(res, edited), nil
		}
		if errors.Is(err, ErrConfigChanged) {
			drafted = true
			err = errors.Join(err, keepDraft(d, edited, base))
			again, rerr := retry(err)
			if rerr != nil {
				return nil, rerr
			}
			if !again {
				return nil, err
			}
			if cur, rerr = os.ReadFile(d.ConfigPath()); rerr != nil {
				return nil, rerr
			}
			base, content = cur, withErrors(cur, err)
			continue
		}
		if !errors.As(err, &se) {
			return res, err
		}
		again, rerr := retry(err)
		if rerr != nil {
			return nil, rerr
		}
		if !again {
			return nil, &agentdir.SettingsError{Err: errors.Join(err, keepDraft(d, edited, base))}
		}
		content = withErrors(edited, err)
	}
}

// startOf returns the content an edit starts from, and the content of
// config.yaml that it is based on: the draft of an earlier rejected edit
// if there is one, else cur, the current config.yaml.
func startOf(d agentdir.Dir, cur []byte) (base, content []byte, err error) {
	draft, err := os.ReadFile(d.ConfigDraftPath())
	if errors.Is(err, os.ErrNotExist) {
		return cur, cur, nil
	}
	if err != nil {
		return nil, nil, err
	}
	base, err = os.ReadFile(d.ConfigDraftBasePath())
	if errors.Is(err, os.ErrNotExist) {
		base, err = cur, nil
	}
	if err != nil {
		return nil, nil, err
	}
	path := d.ConfigDraftPath()
	note := fmt.Sprintf("%sthis is your edit saved in %s; to discard it, quit, run `rm %s`, and run `a2a-serve config edit` again\n", notePrefix, path, path)
	return base, append([]byte(note), draft...), nil
}

// keepDraft saves the rejected edit, and base, the content of config.yaml
// it started from, so that the work isn't lost, and returns the note that
// config.yaml stayed as it was.
func keepDraft(d agentdir.Dir, edited, base []byte) error {
	path := d.ConfigDraftPath()
	err := fsutil.MkdirPrivate(d.StateDir())
	if err == nil {
		err = fsutil.WriteAtomic(d.ConfigDraftBasePath(), base, 0o600)
	}
	if err == nil {
		err = fsutil.WriteAtomic(path, edited, 0o600)
	}
	if err != nil {
		return fmt.Errorf("config.yaml was not changed, and saving your edit failed: %w", err)
	}
	return fmt.Errorf("config.yaml was not changed; your edit is saved in %s", path)
}

// removeDraft deletes the draft of an earlier rejected edit, which a
// finished edit replaces.
func removeDraft(d agentdir.Dir) {
	for _, path := range []string{d.ConfigDraftPath(), d.ConfigDraftBasePath()} {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			slog.Warn("removing the draft of config.yaml", "path", path, "error", err)
		}
	}
}

// withUnknownKeys adds a warning to res for each key of the config.yaml
// content data that the server ignores.
func withUnknownKeys(res *Result, data []byte) *Result {
	keys, err := agentdir.UnknownKeys(data)
	if err != nil {
		return res
	}
	for _, k := range keys {
		res.Warnings = append(res.Warnings, k.String())
	}
	return res
}

func withErrors(data []byte, err error) []byte {
	var sb strings.Builder
	for line := range strings.SplitSeq(err.Error(), "\n") {
		sb.WriteString(errorPrefix + line + "\n")
	}
	return append([]byte(sb.String()), data...)
}

func stripErrors(data []byte) []byte {
	for bytes.HasPrefix(data, []byte(errorPrefix)) || bytes.HasPrefix(data, []byte(notePrefix)) {
		i := bytes.IndexByte(data, '\n')
		if i < 0 {
			return nil
		}
		data = data[i+1:]
	}
	return data
}
