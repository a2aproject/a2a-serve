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
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/a2aproject/a2a-serve/internal/clierr"
	"github.com/a2aproject/a2a-serve/internal/credstore"
	"github.com/a2aproject/a2a-serve/internal/mailboxapi"
)

// credentialRequest and credentialResponse are the messages of the Bazel
// credential helper protocol.
type credentialRequest struct {
	URI string `json:"uri"`
}

type credentialResponse struct {
	Headers map[string][]string `json:"headers,omitempty"`
}

func newCredentialGetCmd(cfg *globalConfig) *cobra.Command {
	return &cobra.Command{
		Use:   "get",
		Short: "Read {\"uri\": ...} from stdin and print the headers for it",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			var req credentialRequest
			if err := json.NewDecoder(cmd.InOrStdin()).Decode(&req); err != nil {
				return clierr.Usage(fmt.Sprintf("reading the credential request from stdin: %v", err))
			}
			if req.URI == "" {
				return clierr.Usage("the credential request has no uri")
			}
			c, ok, err := lookupCredential(cfg, req.URI)
			if err != nil {
				return err
			}
			var resp credentialResponse
			if ok {
				resp.Headers = map[string][]string{"Authorization": {"Bearer " + c.Token}}
			}
			return json.NewEncoder(cmd.OutOrStdout()).Encode(resp)
		},
	}
}

// lookupCredential returns the saved token for uri, or the node token in
// $A2A_NODE_TOKEN when uri is its agent.
func lookupCredential(cfg *globalConfig, uri string) (credstore.Credential, bool, error) {
	store, err := cfg.credentialStore()
	if err != nil {
		return credstore.Credential{}, false, err
	}
	c, ok, err := store.Lookup(uri)
	if err != nil || ok {
		return c, ok, err
	}
	c, ok = credstore.NodeCredential(mailboxapi.EnvToken(), uri)
	return c, ok, nil
}
