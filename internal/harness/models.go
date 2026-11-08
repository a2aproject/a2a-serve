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

package harness

import (
	"net"
	"slices"
	"strings"

	"github.com/a2aproject/a2a-serve/internal/modelproxy"
)

// ModelRoute is how a harness in a sandbox reaches the model APIs through
// the server, which adds the API keys, so that the keys stay out of the
// sandbox.
type ModelRoute struct {
	// Keys are the variables of [Spec.Env] that hold model API keys.
	Keys []string
	// Socket is the host path of the Unix socket that serves the model
	// routes. The sandbox gets a forwarder to it on its loopback.
	Socket string
	// URL is the root URL of the model routes over TCP, used when Socket
	// is empty.
	URL string
	// Key is what the harness sends in place of the API keys. Over TCP, the
	// server tells agent processes apart by it.
	Key string
}

// modelForwardAddr is where in the sandbox a forwarder passes the model
// requests on to the socket of the model routes.
const modelForwardAddr = "127.0.0.1:47413"

// routedModels is the result of routeModels.
type routedModels struct {
	// env is the environment of the harness, with placeholders in place of
	// the routed keys and the base URLs of their providers.
	env  []string
	apis ModelAPIs
	// keys are the variables of the routed keys.
	keys []string
	// kept are the variables of the keys whose base URL env sets, which
	// the harness gets as they are, since their requests go elsewhere.
	kept []string
}

// routeModels routes the keys of route through the server, those of the
// providers that the harness takes the base URL of.
func routeModels(env []string, route *ModelRoute, providers []string) routedModels {
	out := routedModels{env: env}
	if route == nil {
		return out
	}
	root := route.URL
	if route.Socket != "" {
		root = "http://" + modelForwardAddr
	}
	apis := ModelAPIs{BaseURLs: map[string]string{}, Key: route.Key}
	var routedEnv []string
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		p, ok := modelproxy.ProviderOfKey(name)
		switch {
		case !ok || !slices.Contains(route.Keys, name) || !slices.Contains(providers, p.Name):
			routedEnv = append(routedEnv, kv)
		case setsBaseURL(env, p):
			routedEnv = append(routedEnv, kv)
			if !slices.Contains(out.kept, name) {
				out.kept = append(out.kept, name)
			}
		default:
			routedEnv = append(routedEnv, name+"="+route.Key)
			if !slices.Contains(out.keys, name) {
				out.keys = append(out.keys, name)
			}
			apis.BaseURLs[p.Name] = root + modelproxy.PathPrefix + p.Name + p.SDKBase
		}
	}
	if len(out.keys) == 0 {
		return out
	}
	for _, p := range modelproxy.Providers {
		if _, ok := apis.BaseURLs[p.Name]; ok && p.BaseURLEnv != "" {
			routedEnv = append(routedEnv, p.BaseURLEnv+"="+root+modelproxy.PathPrefix+p.Name+p.EnvBase)
		}
	}
	out.env, out.apis = routedEnv, apis
	return out
}

func setsBaseURL(env []string, p modelproxy.Provider) bool {
	return p.BaseURLEnv != "" && slices.ContainsFunc(env, func(kv string) bool { return strings.HasPrefix(kv, p.BaseURLEnv+"=") })
}

// modelForwarder is a Node.js program that passes the connections to a
// port on the loopback on to a Unix socket. The harness images have
// Node.js, and the SDKs of the harnesses can't reach a socket themselves.
const modelForwarder = `const net = require("net");
const [, socket, port] = process.argv;
net.createServer((c) => {
  const u = net.connect(socket);
  const fail = () => { c.destroy(); u.destroy(); };
  c.on("error", fail);
  u.on("error", fail);
  c.pipe(u);
  u.pipe(c);
}).on("error", (err) => {
  console.error("a2a-serve: forwarding model requests to the server: " + err.message);
  process.exit(1);
}).listen(Number(port), "127.0.0.1");`

// forwardScript starts the forwarder in the background and replaces itself
// with the harness. Without node the agent can't reach its model, so the
// container exits.
const forwardScript = `command -v node >/dev/null || { echo "a2a-serve: the image has no node, which the agent needs to reach its model through the server; use an image with node, or --sandbox local" >&2; exit 127; }; ` +
	`node -e "$1" "$2" "$3" & shift 3; exec "$@"`

// withModelForwarder returns c started after a forwarder from
// modelForwardAddr to socket, a path in the sandbox.
func withModelForwarder(c Command, socket string) Command {
	_, port, err := net.SplitHostPort(modelForwardAddr)
	if err != nil {
		panic(err)
	}
	args := append([]string{"-c", forwardScript, "sh", modelForwarder, socket, port, c.Path}, c.Args...)
	c.Path, c.Args = "sh", args
	return c
}
