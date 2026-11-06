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

package cli

import (
	"fmt"
	"runtime"
	"runtime/debug"

	"github.com/spf13/cobra"
)

// Build metadata, which release builds set with -ldflags "-X". Other builds
// read the module version and VCS stamp of the binary.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

type versionInfo struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	Date      string `json:"date"`
	GoVersion string `json:"goVersion"`
	Platform  string `json:"platform"`
}

func newVersionCmd(cfg *globalConfig) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version information",
		Long:  "Print version information.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			info := buildVersionInfo()
			if cfg.IsJSON() {
				return cfg.PrintJSON(info)
			}
			_, err := fmt.Fprint(cfg.Out, info.text())
			return err
		},
	}
}

// text is the version information as `version` and --version print it.
func (v versionInfo) text() string {
	return fmt.Sprintf("a2a-serve %s\ncommit:  %s\nbuilt:   %s\ngo:      %s\nos/arch: %s\n",
		v.Version, v.Commit, v.Date, v.GoVersion, v.Platform)
}

func buildVersionInfo() versionInfo {
	info := versionInfo{
		Version:   version,
		Commit:    commit,
		Date:      date,
		GoVersion: runtime.Version(),
		Platform:  runtime.GOOS + "/" + runtime.GOARCH,
	}
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return info
	}
	if info.Version == "dev" && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		info.Version = bi.Main.Version
	}
	for _, s := range bi.Settings {
		switch {
		case s.Key == "vcs.revision" && info.Commit == "none":
			info.Commit = s.Value
		case s.Key == "vcs.time" && info.Date == "unknown":
			info.Date = s.Value
		case s.Key == "vcs.modified" && s.Value == "true" && info.Commit != "none":
			info.Commit += "-dirty"
		}
	}
	return info
}
