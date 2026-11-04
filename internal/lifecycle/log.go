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

package lifecycle

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
)

// isProgress reports whether a log line is a progress line for the user
// rather than a structured log record.
func isProgress(line string) bool {
	return line != "" && !strings.HasPrefix(line, "time=")
}

// readFrom returns the complete lines of the file at path after offset, and
// the offset after them. A file shorter than offset was replaced, as on a
// restart, and is read from the start.
func readFrom(path string, offset int64) ([]string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, offset, err
	}
	defer func() {
		if err := f.Close(); err != nil {
			slog.Debug("closing log", "error", err)
		}
	}()
	fi, err := f.Stat()
	if err != nil {
		return nil, offset, err
	}
	if fi.Size() < offset {
		offset = 0
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return nil, offset, err
	}
	var lines []string
	r := bufio.NewReader(f)
	for {
		line, err := r.ReadString('\n')
		if errors.Is(err, io.EOF) {
			// A partial line is read again once it is complete.
			return lines, offset, nil
		}
		if err != nil {
			return lines, offset, err
		}
		offset += int64(len(line))
		lines = append(lines, strings.TrimRight(line, "\r\n"))
	}
}

// FollowLog prints the last n lines of the log, then new lines until ctx
// ends if follow is set.
func FollowLog(ctx context.Context, dir agentdir.Dir, n int, follow bool, out io.Writer) error {
	data, err := os.ReadFile(dir.LogPath())
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("no log yet at %s", dir.LogPath())
	}
	if err != nil {
		return err
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if n > 0 && len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	if err := printLines(out, lines); err != nil {
		return err
	}
	if !follow {
		return nil
	}
	offset := int64(len(data))
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(500 * time.Millisecond):
		}
		lines, next, err := readFrom(dir.LogPath(), offset)
		if err != nil {
			return err
		}
		offset = next
		if err := printLines(out, lines); err != nil {
			return err
		}
	}
}

func printLines(out io.Writer, lines []string) error {
	for _, l := range lines {
		if _, err := fmt.Fprintln(out, l); err != nil {
			return err
		}
	}
	return nil
}
