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

// Command probe runs in the containers of the tests of the sandbox package
// and prints, one line per argument, what it finds: "read:<path>" prints
// the file, "write:<path>" whether creating it works, "ls:<dir>" its
// entries, and "dial:<host:port>" whether a TCP connection opens. For the
// tests of live workspaces, "mkdir:<dir>", "rm:<path>", "mv:<from>=<to>",
// "ln:<target>=<path>", "cp:<from>=<to>" and "chmod:<path>=<octal mode>"
// print whether they work, "readlink:<path>" prints the target, "mode:<path>"
// the permission bits, and "exec:<path>" whether the file runs.
// "unshare:user" prints the errno of unshare(CLONE_NEWUSER), and
// "userns:<path>" runs path in new user and network namespaces and prints
// whether it runs or EPERM.
package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func main() {
	for _, arg := range os.Args[1:] {
		kind, target, _ := strings.Cut(arg, ":")
		fmt.Println(arg + " " + probe(kind, target))
	}
}

func probe(kind, target string) string {
	switch kind {
	case "read":
		data, err := os.ReadFile(target)
		if err != nil {
			return "error"
		}
		return strings.TrimSpace(string(data))
	case "write":
		if err := os.WriteFile(target, []byte("written"), 0o644); err != nil {
			return "error"
		}
		return "ok"
	case "ls":
		entries, err := os.ReadDir(target)
		if err != nil {
			return "error"
		}
		return fmt.Sprintf("%d entries", len(entries))
	case "mkdir":
		return result(os.Mkdir(target, 0o755))
	case "rm":
		return result(os.Remove(target))
	case "mv":
		from, to, _ := strings.Cut(target, "=")
		return result(os.Rename(from, to))
	case "ln":
		oldname, newname, _ := strings.Cut(target, "=")
		return result(os.Symlink(oldname, newname))
	case "cp":
		from, to, _ := strings.Cut(target, "=")
		data, err := os.ReadFile(from)
		if err != nil {
			return "error"
		}
		return result(os.WriteFile(to, data, 0o644))
	case "chmod":
		file, mode, _ := strings.Cut(target, "=")
		m, err := strconv.ParseUint(mode, 8, 32)
		if err != nil {
			return "error"
		}
		return result(os.Chmod(file, os.FileMode(m)))
	case "readlink":
		dest, err := os.Readlink(target)
		if err != nil {
			return "error"
		}
		return dest
	case "mode":
		fi, err := os.Stat(target)
		if err != nil {
			return "error"
		}
		return fmt.Sprintf("%04o", fi.Mode().Perm())
	case "exec":
		return result(exec.Command(target).Run())
	case "unshare":
		if err := syscall.Unshare(syscall.CLONE_NEWUSER); err != nil {
			return errnoName(err)
		}
		return "ok"
	case "userns":
		cmd := exec.Command(target)
		cmd.SysProcAttr = &syscall.SysProcAttr{Cloneflags: syscall.CLONE_NEWUSER | syscall.CLONE_NEWNET}
		if err := cmd.Run(); err != nil {
			return errnoName(err)
		}
		return "ok"
	case "dial":
		c, err := net.DialTimeout("tcp", target, 10*time.Second)
		if err != nil {
			return "refused"
		}
		if err := c.Close(); err != nil {
			return "refused"
		}
		return "ok"
	}
	return "unknown"
}

// errnoName names the errno of err, such as EPERM.
func errnoName(err error) string {
	switch {
	case errors.Is(err, syscall.EPERM):
		return "EPERM"
	case errors.Is(err, syscall.EINVAL):
		return "EINVAL"
	}
	return "error: " + err.Error()
}

func result(err error) string {
	if err != nil {
		return "error"
	}
	return "ok"
}
