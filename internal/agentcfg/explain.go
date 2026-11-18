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

package agentcfg

import (
	"errors"
	"fmt"
	"strings"

	"github.com/a2aproject/a2a-serve/core/runner"
	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/agentprofile"
	"github.com/a2aproject/a2a-serve/internal/egressrules"
	"github.com/a2aproject/a2a-serve/internal/sandboxrt"
)

// Explain returns err with the errors of the core that it wraps worded with
// the settings and commands that fix them. It keeps what err wraps.
func Explain(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	for _, typed := range typedErrors(err) {
		msg = strings.Replace(msg, typed.Error(), Warning(typed), 1)
	}
	if msg == err.Error() {
		return err
	}
	return &explained{msg: msg, err: err}
}

type explained struct {
	msg string
	err error
}

func (e *explained) Error() string { return e.msg }

func (e *explained) Unwrap() error { return e.err }

// typedErrors returns the errors of the core in err that Warning words.
func typedErrors(err error) []error {
	var out []error
	add := func(e error, ok bool) {
		if ok {
			out = append(out, e)
		}
	}
	add(asType[*agentprofile.UnknownAgentError](err))
	add(asType[*agentprofile.NoImageError](err))
	add(asType[*agentprofile.NotInstalledError](err))
	add(asType[*agentprofile.WorkspacePathError](err))
	add(asType[*agentprofile.NestedAgentsError](err))
	add(asType[*agentprofile.AppearedError](err))
	add(asType[*agentprofile.EgressError](err))
	add(asType[*sandboxrt.DockerError](err))
	add(asType[*runner.MissingSecretError](err))
	add(asType[*runner.CopyError](err))
	add(asType[*runner.InjectRuleError](err))
	for _, sentinel := range []error{runner.ErrGrantChanged, runner.ErrGrantNarrowed} {
		add(sentinel, errors.Is(err, sentinel))
	}
	return out
}

func asType[T error](err error) (error, bool) {
	e, ok := errors.AsType[T](err)
	return e, ok
}

// restartHint follows the errors of a sandbox that doesn't hide what
// changed since the agent started.
const restartHint = "; restart this agent with `a2a-serve restart`"

// localOrDoctor follows the problems of docker.
const localOrDoctor = "run a2a-serve config set --sandbox local, or run a2a-serve doctor"

// MissingSecretHint says how to store the secret name that the agent gets
// but that has no value.
func MissingSecretHint(name string) string {
	return fmt.Sprintf("secret %s is not set; pass --secret %s again with the variable set", name, name)
}

// Warning words an error or warning of the core for the user.
func Warning(err error) string {
	switch e := err.(type) {
	case *agentprofile.UnknownAgentError:
		return e.Error() + ", or pass --command for any ACP agent"
	case *agentprofile.NoImageError:
		return e.Error() + "; pass --image or --sandbox local"
	case *agentprofile.NotInstalledError:
		return e.Error() + "; install it, or run a2a-serve config set --sandbox docker, or run a2a-serve doctor"
	case *agentprofile.WorkspacePathError:
		return fmt.Sprintf("the first --mount is the workspace and must keep its path (%s), got %s", e.Src, e.Dst)
	case *agentprofile.NestedAgentsError:
		return e.Error() + restartHint
	case *agentprofile.AppearedError:
		return e.Error() + restartHint
	case *agentprofile.NestedAgentsWarning:
		if e.Exposed {
			return fmt.Sprintf("found other agents in %s; --sandbox local doesn't hide their .a2a directories, so the agent can read their secrets", strings.Join(e.Roots, ", "))
		}
		return e.Error()
	case *agentprofile.EgressError:
		if e.Local {
			return noInject(e.Egress.Inject, "the agent runs with --sandbox local")
		}
		return egressProblem(e.Egress, e.Reason)
	case *agentprofile.NoBoundaryWarning:
		return NoBoundary(e.Reason)
	case *agentprofile.EgressIgnoredWarning:
		return egressIgnored(e)
	case agentprofile.EgressWarnings:
		texts := make([]string, len(e))
		for i, w := range e {
			texts[i] = Warning(w)
		}
		return strings.Join(texts, "; ")
	case *sandboxrt.DockerError:
		sep := " or "
		if strings.Contains(e.Fix, " or ") {
			sep = ", or "
		}
		s := "docker " + e.Problem + "; " + e.Fix + sep + localOrDoctor
		if e.Err != nil {
			s += ": " + e.Err.Error()
		}
		return s
	}
	if e, ok := err.(*runner.MissingSecretError); ok {
		if e.Main {
			return MissingSecretHint(e.Name)
		}
		return fmt.Sprintf("the owner of the agent hasn't stored the secret %s that this conversation's agent gets; ask them to store it", e.Name)
	}
	if e, ok := err.(*runner.InjectRuleError); ok {
		why := e.Err.Error()
		if p, ok := errors.AsType[*egressrules.UnknownProviderError](e.Err); ok {
			why = agentdir.UnknownProviderText(p.Name)
		}
		return "--" + agentdir.InjectSetting(e.Rule) + " " + e.Rule.Entry() + ": " + why
	}
	if e, ok := err.(*runner.CopyError); ok {
		return e.Error() + "; the sandbox rule of this caller gives own storage, a private copy of the workspace, and the owner can give read-only storage, which needs no copy"
	}
	switch err {
	case runner.ErrGrantChanged:
		return "the sandbox rules of this conversation changed, and its agent container restarts with them once its current tasks end; send the message again then"
	case runner.ErrGrantNarrowed:
		return "the sandbox rules of this conversation changed and give less, so its waiting tasks ended; send the message again"
	}
	if errors.Is(err, agentprofile.ErrNoAllowRules) {
		return noAllowlistRules
	}
	return err.Error()
}
