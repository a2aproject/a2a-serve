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
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/a2aproject/a2a-serve/internal/agentmesh"
	"github.com/a2aproject/a2a-serve/internal/download"
	"github.com/a2aproject/a2a-serve/internal/relay"
	"github.com/a2aproject/a2a-serve/internal/relay/control"
	"github.com/a2aproject/a2a-serve/internal/relay/meshbackend"
	"github.com/a2aproject/a2a-serve/internal/relay/slimbackend"
	"github.com/a2aproject/a2a-serve/internal/relaydir"
)

// relayBackend is how this a2a-serve runs one backend of a relay.
type relayBackend struct {
	id, name, version string
	// release is the program the backend runs, and binaryEnv names one to
	// run in its place.
	release   download.Release
	binaryEnv string
	check     func(ctx context.Context, path string) error
	newFunc   func(bin string, log io.Writer) relay.Backend
}

var relayBackends = []relayBackend{
	{
		id: relaydir.BackendAgentMesh, name: meshbackend.Name, version: agentmesh.Version,
		release: meshbackend.Release, binaryEnv: meshbackend.BinaryEnv, check: meshbackend.CheckBinary,
		newFunc: func(bin string, log io.Writer) relay.Backend {
			return meshbackend.New(meshbackend.Options{Binary: bin, Log: log, Now: time.Now})
		},
	},
	{
		id: relaydir.BackendSLIM, name: slimbackend.Name, version: slimbackend.Version,
		release: slimbackend.Release, binaryEnv: slimbackend.BinaryEnv, check: slimbackend.CheckBinary,
		newFunc: func(bin string, log io.Writer) relay.Backend {
			return slimbackend.New(slimbackend.Options{Binary: bin, Log: log, Now: time.Now})
		},
	},
}

// relayBackendOf returns the backend of a relay with conf.
func relayBackendOf(conf *relaydir.Config) relayBackend {
	for _, b := range relayBackends {
		if b.id == conf.BackendName() {
			return b
		}
	}
	return relayBackends[0]
}

// relayBackendNamed returns the backend a running relay names in its
// versions.
func relayBackendNamed(v control.Versions) relayBackend {
	name, _ := v.BackendOrDefault()
	for _, b := range relayBackends {
		if b.name == name {
			return b
		}
	}
	return relayBackends[0]
}

// pinned returns the versions this a2a-serve runs the backend with.
func (b relayBackend) pinned() control.Versions {
	v := control.Versions{A2AServe: buildVersionInfo().Version, Backend: b.name, BackendVersion: b.version}
	if b.id == relaydir.BackendAgentMesh {
		v.AgentMesh = b.version
	}
	return v
}

func (b relayBackend) fetcher(hc *http.Client, logf func(string, ...any)) (*download.Fetcher, error) {
	cache, err := os.UserCacheDir()
	if err != nil {
		return nil, err
	}
	return download.NewFetcher(b.release, filepath.Join(cache, "a2a-serve"), os.Getenv(b.binaryEnv), hc, runtime.GOOS, runtime.GOARCH, logf), nil
}

// binaryPath returns where the backend's pinned program is, or is
// downloaded to.
func (b relayBackend) binaryPath() (string, error) {
	f, err := b.fetcher(nil, nil)
	if err != nil {
		return "", err
	}
	return f.Location(), nil
}

// binary returns the backend's pinned program, downloading it if needed.
func (b relayBackend) binary(ctx context.Context, cfg *globalConfig) (string, error) {
	hc := &http.Client{Timeout: 10 * time.Minute, Transport: cfg.httpTransport}
	f, err := b.fetcher(hc, func(format string, args ...any) {
		if _, err := fmt.Fprintf(cfg.progress(), format+"\n", args...); err != nil {
			slog.Debug("writing progress", "error", err)
		}
	})
	if err != nil {
		return "", err
	}
	bin, err := f.Get(ctx)
	if err != nil {
		return "", err
	}
	if err := b.check(ctx, bin); err != nil {
		return "", err
	}
	return bin, nil
}
