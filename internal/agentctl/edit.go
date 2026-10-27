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
)

// errorPrefix marks the lines Edit puts on top of the file to show why the
// last edit was rejected.
const errorPrefix = "# ERROR: "

// Edit lets edit change a copy of config.yaml, like crontab -e. A valid
// result is applied as by [Apply]. For an invalid one, retry decides
// whether to edit again, with the error on top of the file. An unchanged
// file returns an empty result.
func Edit(ctx context.Context, d agentdir.Dir, edit func(path string) error, retry func(error) (bool, error)) (*Result, error) {
	orig, err := os.ReadFile(d.ConfigPath())
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
	content := orig
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
		if bytes.Equal(edited, orig) {
			return &Result{Path: d.ConfigPath()}, nil
		}
		res, err := Replace(ctx, d, edited, false)
		var se *agentdir.SettingsError
		if !errors.As(err, &se) {
			return res, err
		}
		again, rerr := retry(err)
		if rerr != nil {
			return nil, rerr
		}
		if !again {
			return nil, &agentdir.SettingsError{Err: fmt.Errorf("config.yaml was not changed: %w", err)}
		}
		content = withErrors(edited, err)
	}
}

func withErrors(data []byte, err error) []byte {
	var sb strings.Builder
	for line := range strings.SplitSeq(err.Error(), "\n") {
		sb.WriteString(errorPrefix + line + "\n")
	}
	return append([]byte(sb.String()), data...)
}

func stripErrors(data []byte) []byte {
	for bytes.HasPrefix(data, []byte(errorPrefix)) {
		i := bytes.IndexByte(data, '\n')
		if i < 0 {
			return nil
		}
		data = data[i+1:]
	}
	return data
}
