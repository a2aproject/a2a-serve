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

//go:build e2e

package modelproxy

import (
	"net/url"
	"os"
)

// EnvUpstream points the model routes of an e2e build at a stub model.
// Release builds don't read it, so an API key can't be sent elsewhere.
const EnvUpstream = "A2A_SERVE_E2E_MODEL_URL"

func init() {
	upstreamOverride = os.Getenv(EnvUpstream)
	if _, err := url.Parse(upstreamOverride); err != nil {
		panic(EnvUpstream + ": " + err.Error())
	}
}
