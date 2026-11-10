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

package testutil

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
)

var (
	staleDirsOnce sync.Once
	tempDirSeq    atomic.Int64
	tempDirName   = regexp.MustCompile(`^a2a-test-([0-9]+)-`)
)

// ShortTempDir returns a new directory under os.TempDir with a short path,
// for tests that put Unix sockets in it. The test removes it when it ends.
// A killed test binary leaves its directory behind, and the next test
// process that calls ShortTempDir or MkdirShortTemp removes it.
func ShortTempDir(t testing.TB) string {
	t.Helper()
	dir, err := MkdirShortTemp()
	if err != nil {
		t.Fatalf("MkdirShortTemp() error = %v", err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Errorf("removing %s: %v; left: %v", dir, err, leftFiles(dir))
		}
	})
	return dir
}

// MkdirShortTemp creates a directory like ShortTempDir for code that has no
// testing.TB, such as TestMain. The caller removes it.
func MkdirShortTemp() (string, error) {
	staleDirsOnce.Do(func() {
		if err := removeStaleDirs(os.TempDir(), processRunning); err != nil {
			fmt.Fprintln(os.Stderr, "testutil: removing the temp dirs of finished test processes:", err)
		}
	})
	for {
		dir := filepath.Join(os.TempDir(), fmt.Sprintf("a2a-test-%d-%d", os.Getpid(), tempDirSeq.Add(1)))
		err := os.Mkdir(dir, 0o700)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		return dir, err
	}
}

// removeStaleDirs removes the a2a-test directories under root whose process
// running reports as finished. It tries every directory and returns the
// errors together.
func removeStaleDirs(root string, running func(pid int) bool) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	var errs []error
	for _, e := range entries {
		m := tempDirName.FindStringSubmatch(e.Name())
		if m == nil || !e.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(m[1])
		if err != nil || running(pid) {
			continue
		}
		if err := os.RemoveAll(filepath.Join(root, e.Name())); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func leftFiles(dir string) []string {
	var left []string
	if err := filepath.WalkDir(dir, func(path string, _ fs.DirEntry, err error) error {
		left = append(left, path)
		return err
	}); err != nil {
		left = append(left, err.Error())
	}
	return left
}
