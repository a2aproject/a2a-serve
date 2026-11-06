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
	"strconv"
	"strings"
	"time"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
)

// isProgress reports whether a log line is a progress line for the user
// rather than a structured log record.
func isProgress(line string) bool {
	return line != "" && !strings.HasPrefix(line, "time=")
}

// stopMsg is the message of the record the server logs when it stops with
// an error.
const stopMsg = "agent server stopped"

// isStop reports whether a log line is the record of a server that stopped
// with an error.
func isStop(line string) bool {
	return !isProgress(line) && strings.Contains(line, " msg="+strconv.Quote(stopMsg))
}

// logAttr returns the value of the attribute key of a log record line.
func logAttr(line, key string) string {
	_, v, ok := strings.Cut(line, " "+key+"=")
	if !ok {
		return ""
	}
	if q, err := strconv.QuotedPrefix(v); err == nil {
		unq, err := strconv.Unquote(q)
		if err == nil {
			return unq
		}
	}
	v, _, _ = strings.Cut(v, " ")
	return v
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
// ends if follow is set. Notes for the user, such as that the agent isn't
// running, go to notes.
func FollowLog(ctx context.Context, dir agentdir.Dir, n int, follow bool, out, notes io.Writer) error {
	offset, err := printTail(dir.LogPath(), n, out)
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("no log yet at %s", dir.LogPath())
	}
	if err != nil {
		return err
	}
	if !follow {
		return nil
	}
	if !serverRunning(dir) {
		if _, err := fmt.Fprintln(notes, "note: the agent is not running; waiting for it to write to its log (Ctrl-C to stop)"); err != nil {
			return err
		}
	}
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

// printTail prints the last n lines of the file at path, or all of them if n
// is not positive, and returns the size of the file it read. Trailing blank
// lines are dropped.
func printTail(path string, n int, out io.Writer) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer func() {
		if err := f.Close(); err != nil {
			slog.Debug("closing log", "error", err)
		}
	}()
	fi, err := f.Stat()
	if err != nil {
		return 0, err
	}
	size := fi.Size()
	var start int64
	if n > 0 {
		if start, err = tailStart(f, size, n); err != nil {
			return 0, err
		}
	}
	return size, printTrimmed(out, io.NewSectionReader(f, start, size-start))
}

// tailStart returns the offset at which the last n lines of the first size
// bytes of r start, reading from the end. Trailing newlines don't count.
func tailStart(r io.ReaderAt, size int64, n int) (int64, error) {
	buf := make([]byte, 32<<10)
	trailing, newlines := true, 0
	for pos := size; pos > 0; {
		chunk := buf[:min(int64(len(buf)), pos)]
		pos -= int64(len(chunk))
		if _, err := r.ReadAt(chunk, pos); err != nil && !errors.Is(err, io.EOF) {
			return 0, err
		}
		for i := len(chunk) - 1; i >= 0; i-- {
			if trailing && chunk[i] == '\n' {
				continue
			}
			trailing = false
			if chunk[i] != '\n' {
				continue
			}
			if newlines++; newlines == n {
				return pos + int64(i) + 1, nil
			}
		}
	}
	return 0, nil
}

// printTrimmed prints the lines of r without the blank lines at its end. It
// prints one blank line if r has no other.
func printTrimmed(out io.Writer, r io.Reader) error {
	br := bufio.NewReader(r)
	blank, printed := 0, false
	for {
		line, err := br.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		if text := strings.TrimSuffix(line, "\n"); text == "" {
			if line != "" {
				blank++
			}
		} else {
			if err := printLines(out, append(make([]string, blank), text)); err != nil {
				return err
			}
			blank, printed = 0, true
		}
		if err != nil {
			break
		}
	}
	if printed {
		return nil
	}
	return printLines(out, []string{""})
}

func printLines(out io.Writer, lines []string) error {
	for _, l := range lines {
		if _, err := fmt.Fprintln(out, l); err != nil {
			return err
		}
	}
	return nil
}
