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
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
)

// Dest is where snapshots are kept.
type Dest interface {
	// Put stores a snapshot under name.
	Put(ctx context.Context, name string, r io.Reader) error
	// List returns the names of the stored files.
	List(ctx context.Context) ([]string, error)
	// Get returns the snapshot stored under name.
	Get(ctx context.Context, name string) (io.ReadCloser, error)
	// Delete removes the snapshot stored under name.
	Delete(ctx context.Context, name string) error
}

// Runner runs a command with stdin, writing its stdout to stdout. It
// returns an error with the command's stderr when it fails.
type Runner func(ctx context.Context, stdin io.Reader, stdout io.Writer, name string, args ...string) error

// ExecRunner runs commands on the host.
func ExecRunner(ctx context.Context, stdin io.Reader, stdout io.Writer, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	var stderr bytes.Buffer
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return fmt.Errorf("%s: %w: %s", name, err, msg)
		}
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}

// Open returns the destination of to: a directory (or file:// URL), a
// gs://bucket/prefix URL, which uses the gcloud CLI, or an
// s3://bucket/prefix URL, which uses the aws CLI. The CLIs find
// credentials the usual way. run runs them; nil runs them on the host.
func Open(to string, run Runner) (Dest, error) {
	if run == nil {
		run = ExecRunner
	}
	switch {
	case strings.HasPrefix(to, "gs://"):
		return &cliDest{url: strings.TrimSuffix(to, "/"), run: run, cli: gcloudCLI{}}, nil
	case strings.HasPrefix(to, "s3://"):
		return &cliDest{url: strings.TrimSuffix(to, "/"), run: run, cli: awsCLI{}}, nil
	case strings.HasPrefix(to, "file://"):
		return dirDest(strings.TrimPrefix(to, "file://")), nil
	case strings.Contains(to, "://"):
		return nil, fmt.Errorf("unsupported backup destination %q: want a directory, gs:// or s3://", to)
	case to == "":
		return nil, errors.New("no backup destination: set one with `a2a-serve config set --backup <dir|gs://…|s3://…>`")
	}
	return dirDest(to), nil
}

// CLI returns the command a destination needs on the host, or "".
func CLI(to string) string {
	switch {
	case strings.HasPrefix(to, "gs://"):
		return "gcloud"
	case strings.HasPrefix(to, "s3://"):
		return "aws"
	}
	return ""
}

type dirDest string

func (d dirDest) Put(_ context.Context, name string, r io.Reader) error {
	if err := os.MkdirAll(string(d), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(string(d), ".tmp-"+name)
	if err != nil {
		return err
	}
	_, err = io.Copy(tmp, r)
	if err == nil {
		err = tmp.Sync()
	}
	if err := errors.Join(err, tmp.Close()); err != nil {
		return errors.Join(err, os.Remove(tmp.Name()))
	}
	if err := os.Rename(tmp.Name(), filepath.Join(string(d), name)); err != nil {
		return errors.Join(err, os.Remove(tmp.Name()))
	}
	return syncDir(string(d))
}

func syncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	return errors.Join(f.Sync(), f.Close())
}

func (d dirDest) List(context.Context) ([]string, error) {
	entries, err := os.ReadDir(string(d))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.Type().IsRegular() {
			out = append(out, e.Name())
		}
	}
	return out, nil
}

func (d dirDest) Get(_ context.Context, name string) (io.ReadCloser, error) {
	return os.Open(filepath.Join(string(d), name))
}

func (d dirDest) Delete(_ context.Context, name string) error {
	return os.Remove(filepath.Join(string(d), name))
}

// objectCLI is the command line of a cloud storage CLI.
type objectCLI interface {
	put(url string) []string
	list(url string) []string
	get(url string) []string
	del(url string) []string
	// names returns the object names in the output of list.
	names(out string) []string
	// empty reports whether list failed because nothing is stored yet.
	empty(err error) bool
}

type cliDest struct {
	url string
	run Runner
	cli objectCLI
}

func (c *cliDest) object(name string) string { return c.url + "/" + name }

func (c *cliDest) Put(ctx context.Context, name string, r io.Reader) error {
	args := c.cli.put(c.object(name))
	return c.run(ctx, r, io.Discard, args[0], args[1:]...)
}

func (c *cliDest) List(ctx context.Context) ([]string, error) {
	var out bytes.Buffer
	args := c.cli.list(c.url + "/")
	if err := c.run(ctx, nil, &out, args[0], args[1:]...); err != nil {
		if c.cli.empty(err) {
			return nil, nil
		}
		return nil, err
	}
	return c.cli.names(out.String()), nil
}

func (c *cliDest) Get(ctx context.Context, name string) (io.ReadCloser, error) {
	pr, pw := io.Pipe()
	args := c.cli.get(c.object(name))
	go func() {
		pw.CloseWithError(c.run(ctx, nil, pw, args[0], args[1:]...))
	}()
	return pr, nil
}

func (c *cliDest) Delete(ctx context.Context, name string) error {
	args := c.cli.del(c.object(name))
	return c.run(ctx, nil, io.Discard, args[0], args[1:]...)
}

type gcloudCLI struct{}

func (gcloudCLI) put(url string) []string  { return []string{"gcloud", "storage", "cp", "-", url} }
func (gcloudCLI) list(url string) []string { return []string{"gcloud", "storage", "ls", url} }
func (gcloudCLI) get(url string) []string  { return []string{"gcloud", "storage", "cat", url} }
func (gcloudCLI) del(url string) []string  { return []string{"gcloud", "storage", "rm", url} }

func (gcloudCLI) names(out string) []string {
	var names []string
	for line := range strings.SplitSeq(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasSuffix(line, "/") {
			continue
		}
		names = append(names, path.Base(line))
	}
	return names
}

func (gcloudCLI) empty(err error) bool { return strings.Contains(err.Error(), "matched no objects") }

type awsCLI struct{}

func (awsCLI) put(url string) []string  { return []string{"aws", "s3", "cp", "-", url} }
func (awsCLI) list(url string) []string { return []string{"aws", "s3", "ls", url} }
func (awsCLI) get(url string) []string  { return []string{"aws", "s3", "cp", url, "-"} }
func (awsCLI) del(url string) []string  { return []string{"aws", "s3", "rm", url} }

// names parses lines of `aws s3 ls`: "2026-11-02 19:00:00  1234 name", or
// "PRE dir/" for prefixes.
func (awsCLI) names(out string) []string {
	var names []string
	for line := range strings.SplitSeq(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 4 || f[0] == "PRE" {
			continue
		}
		names = append(names, f[len(f)-1])
	}
	return names
}

// empty: `aws s3 ls` of a prefix with no objects exits 1 without a message.
func (awsCLI) empty(err error) bool { return strings.HasSuffix(err.Error(), "exit status 1") }
