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

//go:build linux

package fusefs

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/a2aproject/a2a-serve/core/fsys"
)

// serveEnv makes the test binary serve a mount for a test, as described
// by its JSON value, instead of running the tests.
const serveEnv = "FUSEFS_TEST_SERVE"

// mountWatchdog bounds a test on a mount. Past it, the test's mount is
// detached and its server killed, so that a call stuck on the mount
// fails the test instead of hanging the binary.
const mountWatchdog = 2 * time.Minute

func TestMain(m *testing.M) {
	if spec := os.Getenv(serveEnv); spec != "" {
		os.Exit(serveForTest(spec))
	}
	os.Exit(m.Run())
}

// serveSpec is what a server of a test mounts.
type serveSpec struct {
	Dir, Socket string
	Opts        Options
}

// serveForTest mounts a tree in memory, serves the tree on a socket for
// the test to change and read, and unmounts it once its stdin closes.
func serveForTest(spec string) int {
	var s serveSpec
	if err := json.Unmarshal([]byte(spec), &s); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	tree := fsys.Memory()
	ln, err := net.Listen("unix", s.Socket)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	srv := &http.Server{Handler: fsys.Handler(tree), ReadHeaderTimeout: time.Minute}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintln(os.Stderr, err)
		}
	}()
	server, err := mount(s.Dir, tree, s.Opts, false)
	if err != nil {
		fmt.Printf("skip: %v\n", err)
		return 0
	}
	fmt.Println("ready")
	if _, err := io.Copy(io.Discard, os.Stdin); err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
	if err := unmountWhenIdle(server.Unmount); err != nil {
		fmt.Fprintln(os.Stderr, "unmounting:", err)
		return 1
	}
	return 0
}

// mountTree mounts an empty tree with opts, served by a child process, and
// returns the mount point and a client of the tree. A process that serves
// FUSE and forks can deadlock: the child closes its copies of the files on
// the mount when it execs, and waits for the flush, which the forking
// process, stopped for the garbage collector, never answers. The launcher
// also serves the mount from its own process. It skips the test where this
// host can't mount FUSE.
func mountTree(t *testing.T, opts Options) (string, fsys.FS) {
	t.Helper()
	if _, err := os.Stat("/dev/fuse"); err != nil {
		t.Skipf("no /dev/fuse: %v", err)
	}
	dir := t.TempDir()
	sockDir, err := os.MkdirTemp("", "fusefs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(sockDir); err != nil {
			t.Errorf("removing the socket directory error = %v", err)
		}
	})
	opts.UID, opts.GID = uint32(os.Getuid()), uint32(os.Getgid())
	opts.TempDir = t.TempDir()
	socket := filepath.Join(sockDir, "fs.sock")
	spec, err := json.Marshal(serveSpec{Dir: dir, Socket: socket, Opts: opts})
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), serveEnv+"="+string(spec), "GORACE=atexit_sleep_ms=0")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting the FUSE server error = %v", err)
	}
	exited := make(chan error, 1)
	var wedged atomic.Bool
	watchdog := time.AfterFunc(mountWatchdog, func() {
		wedged.Store(true)
		if err := cmd.Process.Kill(); err != nil {
			fmt.Fprintln(os.Stderr, "killing the FUSE server:", err)
		}
		if err := detach(dir); err != nil {
			fmt.Fprintln(os.Stderr, err)
		}
	})
	t.Cleanup(func() {
		watchdog.Stop()
		if wedged.Load() {
			t.Errorf("the test on %s ran past %v, and its mount was detached", dir, mountWatchdog)
		}
		if err := stdin.Close(); err != nil {
			t.Errorf("closing the FUSE server's stdin error = %v", err)
		}
		select {
		case err := <-exited:
			if err != nil && !wedged.Load() {
				t.Errorf("the FUSE server error = %v: %s", err, stderr.String())
			}
		case <-time.After(30 * time.Second):
			if err := cmd.Process.Kill(); err != nil {
				t.Errorf("killing the FUSE server error = %v", err)
			}
			<-exited
			t.Errorf("the FUSE server didn't exit: %s", stderr.String())
		}
		if err := detach(dir); err != nil {
			t.Error(err)
		}
	})
	line := make(chan string, 1)
	go func() {
		s, err := bufio.NewReader(stdout).ReadString('\n')
		if err != nil {
			s = "error: " + err.Error()
		}
		line <- strings.TrimSpace(s)
		if _, err := io.Copy(io.Discard, stdout); err != nil {
			fmt.Fprintln(os.Stderr, "reading the FUSE server's output:", err)
		}
		exited <- cmd.Wait()
	}()
	select {
	case got := <-line:
		if reason, ok := strings.CutPrefix(got, "skip: "); ok {
			t.Skipf("can't mount FUSE here: %s", reason)
		}
		if got != "ready" {
			t.Fatalf("the FUSE server said %q: %s", got, stderr.String())
		}
	case <-time.After(time.Minute):
		t.Fatalf("the FUSE server didn't mount %s: %s", dir, stderr.String())
	}
	tree, err := fsys.Client("unix://" + socket)
	if err != nil {
		t.Fatal(err)
	}
	return dir, tree
}

// detach lazily unmounts dir if it is still mounted, such as after a test
// that failed with a file on it open.
func detach(dir string) error {
	mounted, err := isMounted(dir)
	if err != nil || !mounted {
		return err
	}
	bin := "fusermount3"
	if _, err := exec.LookPath(bin); err != nil {
		bin = "fusermount"
	}
	if out, err := exec.Command(bin, "-uz", dir).CombinedOutput(); err != nil {
		return fmt.Errorf("detaching %s error = %v: %s", dir, err, out)
	}
	return nil
}

func isMounted(dir string) (bool, error) {
	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return false, err
	}
	for line := range strings.Lines(string(data)) {
		if f := strings.Fields(line); len(f) > 4 && f[4] == dir {
			return true, nil
		}
	}
	return false, nil
}

// unmountWhenIdle calls unmount until it succeeds, for a mount that another
// process, such as one that reads the temporary directories, keeps busy
// for a moment.
func unmountWhenIdle(unmount func() error) error {
	deadline := time.Now().Add(10 * time.Second)
	for {
		err := unmount()
		if err == nil || time.Now().After(deadline) {
			return err
		}
		time.Sleep(10 * time.Millisecond)
	}
}
