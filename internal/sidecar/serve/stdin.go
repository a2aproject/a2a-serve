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

package serve

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

	"golang.org/x/term"
)

// maxStdinSecret bounds a secret line read from stdin.
const maxStdinSecret = 64 << 10

// stdinWaitHint is how long a2a-serve-sidecar waits for a line on stdin
// before it says on stderr what it waits for.
const stdinWaitHint = 2 * time.Second

// stdinSecret reads a single secret line from stdin: a line from a pipe, or a
// prompt without echo on a terminal.
type stdinSecret struct {
	in     io.Reader
	prompt io.Writer
	// waitHint is how long a read from a pipe waits before it writes what it
	// waits for to prompt; 0 writes nothing.
	waitHint   time.Duration
	consumedBy string
}

// read returns the secret line for owner, named label in messages, or ctx's
// error once ctx ends first. The read that ctx abandons stays blocked on
// stdin until the process exits.
func (s *stdinSecret) read(ctx context.Context, owner, label string) (string, error) {
	switch s.consumedBy {
	case "":
	case "host-token":
		return "", usagef("%s: stdin carries the loopback host token; pass this value as env:VAR", label)
	default:
		return "", usagef("%s: only one --contact-auth can read its value from stdin; pass the others as env:VAR", label)
	}
	s.consumedBy = owner
	if s.in == nil {
		if owner == "host-token" {
			return "", usagef("%s: loopback control endpoints need a host token on stdin, which a2a-serve-sidecar doesn't have here", label)
		}
		return "", usagef("%s: - reads the value from stdin, which a2a-serve-sidecar doesn't have here", label)
	}
	read := func() (string, error) { return readSecretLine(s.in, label) }
	var hint <-chan time.Time
	if f, ok := s.in.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		state, err := term.GetState(int(f.Fd()))
		if err != nil {
			return "", fmt.Errorf("%s: reading the prompt: %w", label, err)
		}
		defer func() {
			if ctx.Err() == nil {
				return
			}
			if err := term.Restore(int(f.Fd()), state); err != nil {
				slog.Warn("restoring the terminal", "error", err)
			}
		}()
		read = func() (string, error) { return s.readTerminal(f, label) }
	} else if s.prompt != nil && s.waitHint > 0 {
		timer := time.NewTimer(s.waitHint)
		defer timer.Stop()
		hint = timer.C
	}
	type result struct {
		value string
		err   error
	}
	done := make(chan result, 1)
	go func() {
		v, err := read()
		done <- result{v, err}
	}()
	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case r := <-done:
			return r.value, r.err
		case <-hint:
			hint = nil
			if _, err := fmt.Fprintf(s.prompt, "%s: waiting for one line on stdin\n", label); err != nil {
				slog.Warn("writing the stdin hint", "error", err)
			}
		}
	}
}

// readSecretLine reads the first line of in, of at most maxStdinSecret bytes.
func readSecretLine(in io.Reader, label string) (string, error) {
	line, err := bufio.NewReader(io.LimitReader(in, maxStdinSecret+1)).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("%s: reading stdin: %w", label, err)
	}
	line = strings.TrimRight(line, "\r\n")
	if len(line) > maxStdinSecret {
		return "", usagef("%s: the line on stdin is longer than 64 KiB", label)
	}
	if line == "" {
		return "", usagef("%s: stdin had no value", label)
	}
	return line, nil
}

func (s *stdinSecret) readTerminal(f *os.File, label string) (string, error) {
	if s.prompt != nil {
		if _, err := fmt.Fprintf(s.prompt, "%s: ", label); err != nil {
			return "", err
		}
	}
	b, err := term.ReadPassword(int(f.Fd()))
	if s.prompt != nil {
		if _, perr := fmt.Fprintln(s.prompt); perr != nil {
			return "", perr
		}
	}
	if err != nil {
		return "", fmt.Errorf("%s: reading the prompt: %w", label, err)
	}
	v := strings.TrimSpace(string(b))
	if v == "" {
		return "", usagef("%s: empty value", label)
	}
	if len(v) > maxStdinSecret {
		return "", usagef("%s: the value is longer than 64 KiB", label)
	}
	return v, nil
}
