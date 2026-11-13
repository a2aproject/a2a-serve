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
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"
	"testing"
)

func TestWriteExecutableRunsWhileOthersFork(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("needs a POSIX shell")
	}
	ctx, cancel := context.WithCancel(t.Context())
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for ctx.Err() == nil {
				if err := exec.Command("true").Run(); err != nil {
					t.Errorf("forking error = %v", err)
					return
				}
			}
		})
	}
	defer wg.Wait()
	defer cancel()

	dir := t.TempDir()
	for i := range 500 {
		path := filepath.Join(dir, fmt.Sprintf("script-%d", i))
		WriteExecutable(t, path, []byte("#!/bin/sh\n"))
		if err := exec.Command(path).Run(); errors.Is(err, syscall.ETXTBSY) {
			t.Fatalf("running script %d error = %v, want no ETXTBSY", i, err)
		}
	}
}
