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
	"github.com/spf13/cobra"

	"github.com/a2aproject/a2a-serve/internal/flagparse"
	"github.com/a2aproject/a2a-serve/internal/localsrv"
)

func newProxyCmd(cfg *globalConfig) *cobra.Command {
	var (
		tf testServerFlags
		p  = localsrv.ProxyOptions{SvcParams: &flagparse.ServiceParams{}}
	)
	cmd := &cobra.Command{
		Use:   "proxy <agent-url>",
		Short: "Run a test server which forwards each request to another agent",
		Long: "Run a test server which forwards each request to the agent with the agent card URL <agent-url>, logging the traffic.\n\n" +
			"--auth takes a header value, which may end in a secret value: env:VAR reads the environment variable VAR, and - a prompt without echo or stdin.",
		Example: "  a2a-serve start proxy https://agent.example.com --auth \"Bearer env:AGENT_TOKEN\"",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p.URL = args[0]
			r := cfg.secretReader(cfg.stderr())
			if err := p.SvcParams.ResolveAuth(func(v string) (string, error) {
				_, v, err := flagparse.ResolveSecretHeader("auth", "Authorization: "+v, r)
				return v, err
			}); err != nil {
				return err
			}
			ctx, stop := signalContext(cmd.Context())
			defer stop()
			return localsrv.RunProxy(ctx, tf.options(cfg), p)
		},
	}
	tf.register(cmd)
	f := cmd.Flags()
	p.SvcParams.Attach(f)
	f.Lookup("svc-param").Usage += " for the proxied agent"
	f.Lookup("auth").Usage += " for the proxied agent; may end in env:VAR or -"
	f.StringVar(&p.Tenant, "tenant", "", "Tenant of the proxied agent")
	f.BoolVar(&p.InsecureGRPC, "insecure", false, "Reach the proxied agent over plaintext gRPC")
	return cmd
}
