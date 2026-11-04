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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/a2aproject/a2a-serve/internal/a2acli"
	"github.com/a2aproject/a2a-serve/internal/credstore"
)

// installA2A installs the latest a2a CLI.
const installA2A = "go install github.com/a2aproject/a2a-cli@latest" +
	` && mv "$(command -v a2a-cli)" "$(dirname "$(command -v a2a-cli)")/a2a"`

const installServe = "go install github.com/a2aproject/a2a-serve@latest"

// fixCommand is the fix of the problems --fix fixes. It names the binary,
// which runs also while the a2a CLI has command plugins disabled.
const fixCommand = "a2a-serve doctor --fix"

// Keys of the a2a CLI config, and the environment variables which override them.
const (
	keyPluginsEnabled   = "plugins-enabled"
	envPluginsEnabled   = "A2ACLI_PLUGINS_ENABLED"
	keyCredentialHelper = "credential-helper"
	envCredentialHelper = "A2ACLI_CREDENTIAL_HELPER"
)

// cliChecks checks the a2a CLI, its config and the credentials file.
func cliChecks(ctx context.Context, env Env) []Result {
	results := []Result{checkA2A(ctx, env)}
	path := UserConfigPath(env.Home)
	values, err := readUserConfig(path)
	if err != nil {
		results = append(results, Result{ID: "a2a-config", Status: StatusFail, Message: err.Error(), Fix: "fix the YAML in " + path})
	} else {
		results = append(results, checkPlugins(env, path, values))
	}
	results = append(results, checkServeBinary(env))
	if err == nil {
		results = append(results, checkCredentialHelper(env, path, values))
	}
	if r, ok := checkCredentialsFile(env); ok {
		results = append(results, r)
	}
	for i := range results {
		results[i].Group = GroupCLI
	}
	return results
}

func checkA2A(ctx context.Context, env Env) Result {
	fail := func(msg, detail string) Result {
		return Result{ID: "a2a", Status: StatusFail, Message: msg, Detail: detail, Fix: installA2A}
	}
	path, err := env.LookPath(a2acli.Binary)
	if err != nil {
		return fail("a2a not on PATH", "callers send tasks with it, and harnesses without a sandbox call other agents with it")
	}
	out, err := env.Run(ctx, path, "version", "-o", "json")
	if err != nil {
		return fail(fmt.Sprintf("a2a at %s: can't read its version", path), err.Error())
	}
	var info struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(out, &info); err != nil {
		return fail(fmt.Sprintf("a2a at %s: can't read its version", path), err.Error())
	}
	shown := info.Version
	if shown == "" {
		shown = "(unknown version)"
	}
	// Replace the version comparison with the capability list of
	// `a2a version -o json` (or the plugin info document) once a2a-cli
	// reports one: https://github.com/a2aproject/a2a-cli/issues/102.
	if a2acli.Outdated(a2acli.ModuleVersion(info.Version)) {
		return fail(fmt.Sprintf("a2a %s at %s is too old", shown, path), "it lacks --unix-socket or credential helpers in the format a2a-serve configures, so calls from this machine get a bare 401")
	}
	return Result{ID: "a2a", Status: StatusOK, Message: fmt.Sprintf("a2a %s at %s", shown, path)}
}

func checkPlugins(env Env, path string, values map[string]any) Result {
	r := Result{ID: "plugins"}
	if v, ok := env.LookupEnv(envPluginsEnabled); ok {
		if parseBool(v) {
			r.Status, r.Message = StatusOK, "command plugins enabled ($"+envPluginsEnabled+")"
			return r
		}
		r.Status, r.Message, r.Fix = StatusFail, "command plugins disabled by $"+envPluginsEnabled, "unset "+envPluginsEnabled
		return r
	}
	if _, v, ok := lookupKey(values, keyPluginsEnabled, envPluginsEnabled); ok && parseBool(fmt.Sprint(v)) {
		r.Status, r.Message = StatusOK, "command plugins enabled"
		return r
	}
	r.Status, r.Message = StatusFail, "command plugins not enabled"
	r.Detail = `a2a-serve fails with unknown command "serve" for "a2a"`
	// `a2a serve` doesn't run until the fix is applied.
	r.Fix = fixCommand
	r.apply = func() (string, error) {
		if _, err := SetUserConfigKey(path, keyPluginsEnabled, scalar("!!bool", "true")); err != nil {
			return "", err
		}
		return fmt.Sprintf("set %s: true in %s", keyPluginsEnabled, path), nil
	}
	return r
}

func checkServeBinary(env Env) Result {
	r := Result{ID: "a2a-serve"}
	found, err := env.LookPath("a2a-serve")
	if err != nil {
		r.Status, r.Message, r.Fix = StatusFail, "a2a-serve not on PATH", installServe
		r.Detail = "a2a runs it as a2a serve, and as its credential helper"
		return r
	}
	self, err := env.Executable()
	if err != nil {
		r.Status, r.Message = StatusWarn, fmt.Sprintf("a2a-serve runs %s; can't tell whether it is this binary: %v", found, err)
		return r
	}
	foundReal, selfReal := resolve(found), resolve(self)
	if foundReal == selfReal {
		r.Status, r.Message = StatusOK, "a2a-serve runs "+found
		return r
	}
	pathDirs := filepath.SplitList(getenv(env, "PATH"))
	if slices.ContainsFunc(pathDirs, func(d string) bool { return d != "" && resolve(d) == resolve(filepath.Dir(self)) }) {
		r.Status = StatusFail
		r.Message = fmt.Sprintf("a2a-serve runs %s, a copy earlier on PATH than this binary (%s)", found, self)
		r.Fix = fmt.Sprintf("remove %s, or put %s earlier on PATH", found, filepath.Dir(self))
		return r
	}
	r.Status, r.Message = StatusWarn, fmt.Sprintf("a2a-serve runs %s, not this binary (%s)", found, self)
	return r
}

func checkCredentialHelper(env Env, path string, values map[string]any) Result {
	r := Result{ID: "credential-helper"}
	if v, ok := env.LookupEnv(envCredentialHelper); ok {
		r.Status, r.Message, r.Fix = StatusFail, fmt.Sprintf("$%s is %q, which replaces %s in %s", envCredentialHelper, v, keyCredentialHelper, path), "unset "+envCredentialHelper
		r.Detail = "it takes only [scope=]path, so it can't run " + credstore.HelperCommand
		return r
	}
	key, v, ok := lookupKey(values, keyCredentialHelper, envCredentialHelper)
	if !ok {
		r.Status, r.Message, r.Fix = StatusFail, "credential helper not configured", fixCommand
		r.Detail = "a2a won't attach this machine's owner token, so calls get 401"
		r.apply = setHelpers(path, keyCredentialHelper, []helperEntry{serveHelper()})
		return r
	}
	helpers, old, err := parseHelpers(v)
	if err != nil {
		r.Status, r.Message, r.Fix = StatusFail, fmt.Sprintf("%s in %s: %v", key, path, err), "fix "+key+" in "+path
		r.Detail = "a2a fails every command"
		return r
	}
	if old {
		r.Status, r.Message, r.Fix = StatusFail, fmt.Sprintf("%s in %s has the old \"[scope=]command args\" form", key, path), fixCommand
		r.Detail = "a2a now lists helpers as {scope, command}, so it doesn't run them"
		r.apply = setHelpers(path, key, helpers)
		return r
	}
	if slices.ContainsFunc(helpers, helperEntry.isServe) {
		r.Status, r.Message = StatusOK, "credential helper configured"
		return r
	}
	// Adding a second unscoped helper would replace the user's for every agent.
	r.Status = StatusFail
	r.Message = fmt.Sprintf("%s in %s is %s, not %s", key, path, joinHelpers(helpers), credstore.HelperCommand)
	r.Detail = "a2a won't attach this machine's owner token, so calls get 401"
	r.Fix = fmt.Sprintf("list %s under %s in %s", credstore.HelperEntry, key, path)
	return r
}

// setHelpers sets key to helpers in the a2a CLI config at path.
func setHelpers(path, key string, helpers []helperEntry) func() (string, error) {
	return func() (string, error) {
		if _, err := SetUserConfigKey(path, key, helpersNode(helpers)); err != nil {
			return "", err
		}
		return fmt.Sprintf("set %s in %s to %s", key, path, joinHelpers(helpers)), nil
	}
}

// checkCredentialsFile reports a credentials file others can read. It
// reports nothing when the file is private or doesn't exist.
func checkCredentialsFile(env Env) (Result, bool) {
	path := env.Credentials.Path
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return Result{}, false
	}
	if err != nil {
		return Result{ID: "credentials", Status: StatusFail, Message: err.Error()}, true
	}
	if info.Mode().Perm()&0o077 == 0 {
		return Result{}, false
	}
	return Result{
		ID:      "credentials",
		Status:  StatusFail,
		Message: fmt.Sprintf("%s is readable by others (%#o)", path, info.Mode().Perm()),
		Fix:     fixCommand,
		apply:   chmod(path, 0o600),
	}, true
}

func chmod(path string, mode os.FileMode) func() (string, error) {
	return func() (string, error) {
		if err := os.Chmod(path, mode); err != nil {
			return "", err
		}
		return fmt.Sprintf("restricted %s to %#o", path, mode), nil
	}
}

// lookupKey returns the value of a config key, which the a2a CLI also
// accepts under the name of its environment variable, and the name it has.
func lookupKey(values map[string]any, key, envName string) (string, any, bool) {
	if v, ok := values[key]; ok {
		return key, v, true
	}
	v, ok := values[envName]
	return envName, v, ok
}

func parseBool(s string) bool {
	b, err := strconv.ParseBool(strings.TrimSpace(s))
	return err == nil && b
}

func getenv(env Env, name string) string {
	v, _ := env.LookupEnv(name)
	return v
}

// resolve returns path with symlinks resolved, or path if it can't be.
func resolve(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		return real
	}
	return abs
}
