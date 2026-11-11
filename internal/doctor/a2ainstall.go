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

package doctor

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/a2aproject/a2a-serve/internal/shellquote"
)

const (
	installScriptURL = "https://raw.githubusercontent.com/a2aproject/a2a-serve/main/install.sh"
	// brewUpgradeA2A names the formula of the a2a CLI in the a2a-cli tap,
	// which the formula of a2a-serve depends on.
	brewUpgradeA2A = "brew upgrade a2aproject/a2a-cli/a2a"
)

// a2aLocations is where package managers put binaries on a machine.
type a2aLocations struct {
	home           string
	homebrewPrefix string
	gobin          string
	gopath         string
	writable       func(dir string) bool
}

func (e Env) a2aLocations() a2aLocations {
	return a2aLocations{
		home:           e.Home,
		homebrewPrefix: getenv(e, "HOMEBREW_PREFIX"),
		gobin:          getenv(e, "GOBIN"),
		gopath:         getenv(e, "GOPATH"),
		writable:       e.DirWritable,
	}
}

// install returns the fix that installs the latest a2a CLI over the
// binary at path, the way that binary was installed. An empty path means
// that there is no binary yet.
func (l a2aLocations) install(path string) string {
	if path == "" {
		return installScript("")
	}
	dir := filepath.Dir(path)
	if l.homebrew(path) {
		return brewUpgradeA2A
	}
	if l.goBin(dir) {
		return goInstallA2A(dir)
	}
	userBin := filepath.Join(l.home, ".local", "bin")
	if sameDir(dir, userBin) {
		return installScript("")
	}
	if l.writable != nil && l.writable(dir) {
		return installScript(dir)
	}
	return fmt.Sprintf("run `%s`, then remove %s or put ~/.local/bin before %s on PATH", installScript(""), path, dir)
}

func (l a2aLocations) homebrew(path string) bool {
	if strings.Contains(filepath.ToSlash(resolve(path)), "/Cellar/") {
		return true
	}
	if l.homebrewPrefix == "" {
		return false
	}
	return within(l.homebrewPrefix, path) || within(resolve(l.homebrewPrefix), resolve(path))
}

func (l a2aLocations) goBin(dir string) bool {
	if l.gobin != "" {
		return sameDir(l.gobin, dir)
	}
	gopath := filepath.SplitList(l.gopath)
	if len(gopath) == 0 {
		gopath = []string{filepath.Join(l.home, "go")}
	}
	for _, p := range gopath {
		if sameDir(filepath.Join(p, "bin"), dir) {
			return true
		}
	}
	return false
}

// goInstallA2A installs the a2a CLI into dir. go install names the binary
// a2a-cli, so it is renamed.
func goInstallA2A(dir string) string {
	return "GOBIN=" + shellquote.Quote(dir) + " go install github.com/a2aproject/a2a-cli@latest" +
		" && mv " + shellquote.Quote(filepath.Join(dir, "a2a-cli")) + " " + shellquote.Quote(filepath.Join(dir, "a2a"))
}

// installScript runs the install script for the a2a CLI only, into dir or
// into ~/.local/bin if dir is empty.
func installScript(dir string) string {
	cmd := "curl -fsSL " + installScriptURL + " | "
	if dir != "" {
		cmd += "INSTALL_DIR=" + shellquote.Quote(dir) + " "
	}
	return cmd + "A2A_CLI_ONLY=1 sh"
}

func sameDir(a, b string) bool {
	return a == b || resolve(a) == resolve(b)
}

func within(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
