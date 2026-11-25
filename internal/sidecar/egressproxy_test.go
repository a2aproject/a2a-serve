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

package sidecar_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/core/sandbox"
	"github.com/a2aproject/a2a-serve/internal/cryptoutil"
	"github.com/a2aproject/a2a-serve/internal/sandboxrt/sandboxtest"
	"github.com/a2aproject/a2a-serve/internal/sidecar"
	"github.com/a2aproject/a2a-serve/internal/sidecar/refhost"
	"github.com/a2aproject/a2a-serve/internal/sidecar/serve"
	"github.com/a2aproject/a2a-serve/internal/testutil"
)

func TestEgressProxyReportsInvalidFlags(t *testing.T) {
	t.Parallel()
	workspace := "dir:" + t.TempDir()
	base := []string{"--agent", "opencode", "--workspace", workspace}
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "with egress rules", args: []string{"--egress-proxy", "http://127.0.0.1:9000", "--egress", "allowlist", "--egress-allow", "nas.lan"}, want: "--egress-proxy can't be used with --egress, --egress-allow: the proxy decides every connection"},
		{name: "with a model key", args: []string{"--egress-proxy", "http://127.0.0.1:9000", "--inject-model-key", "anthropic=ANTHROPIC_API_KEY"}, want: "--egress-proxy can't be used with --inject-model-key"},
		{name: "with egress middleware", args: []string{"--egress-proxy", "http://127.0.0.1:9000", "--egress-middleware", "api.anthropic.com=http://127.0.0.1:8080"}, want: "--egress-proxy can't be used with --egress-middleware"},
		{name: "with egress inject header", args: []string{"--egress-proxy", "http://127.0.0.1:9000", "--egress-inject-header", "api.github.com=Authorization:Bearer env:GH_TOKEN"}, want: "--egress-proxy can't be used with --egress-inject-header"},
		{name: "local sandbox", args: []string{"--egress-proxy", "http://127.0.0.1:9000", "--sandbox", "local"}, want: "--egress-proxy needs --sandbox docker"},
		{name: "invalid URL", args: []string{"--egress-proxy", "https://proxy.example:443"}, want: "invalid --egress-proxy: invalid egress proxy URL: the URL isn't http or unix"},
		{name: "URL with credentials", args: []string{"--egress-proxy", "http://user:hunter2@proxy.example:8080/?token=hunter2"}, want: "invalid --egress-proxy: invalid egress proxy URL: the URL can't carry credentials"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var stdout, stderr bytes.Buffer
			args := append(append([]string{}, base...), tc.args...)

			code := sidecar.Main(context.Background(), args, sidecar.IO{Stdout: &stdout, Stderr: &stderr, StdoutIsTerminal: true, Getenv: func(string) string { return "key" }})

			var ready serve.Ready
			if err := json.Unmarshal(stdout.Bytes(), &ready); err != nil || code != 2 || ready.Ready || !strings.Contains(ready.Error, tc.want) {
				t.Fatalf("sidecar.Main(%q) = %d with stdout %q, want 2 and a ready line with an error containing %q", args, code, stdout.String(), tc.want)
			}
			if out := stdout.String() + stderr.String(); strings.Contains(out, "hunter2") {
				t.Fatalf("sidecar.Main(%q) output = %q, want it without the URL's secrets", args, out)
			}
		})
	}
}

// requireDocker skips without Docker and the egress boundary of Linux.
func requireDocker(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" || os.Getenv("A2A_SERVE_SANDBOX_RELAY") == "1" {
		sandboxtest.Skip(t, "needs the egress boundary of Linux without an egress container")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		sandboxtest.Skip(t, "needs the docker CLI")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		sandboxtest.Skip(t, "docker isn't available: %v", err)
	}
}

// dockerSandbox returns the --sandbox of the Docker tests: gvisor when
// sandboxtest.EnvRuntime asks for runsc, and docker otherwise.
func dockerSandbox() string {
	if sandboxtest.Runtime() == "runsc" {
		return serve.SandboxGVisor
	}
	return serve.SandboxDocker
}

// dockerOptions returns the options of the Docker runtime that the
// a2a-serve-sidecar binary bin runs with dockerSandbox.
func dockerOptions(bin string) sandbox.DockerOptions {
	return sandbox.DockerOptions{Launcher: bin, Runtime: sandboxtest.Runtime()}
}

type cachedBuild struct {
	once sync.Once
	val  string
	err  error
}

var (
	sidecarBuilds = map[string]*cachedBuild{
		"0": {},
		"1": {},
	}

	fakeacpImageOnce sync.Once
	fakeacpImage     string
	fakeacpImageErr  error
)

// buildSidecar builds a2a-serve-sidecar with CGO_ENABLED set to cgo; releases build
// it with 0.
func buildSidecar(t *testing.T, cgo string) string {
	t.Helper()
	b, ok := sidecarBuilds[cgo]
	if !ok {
		t.Fatalf("unexpected CGO_ENABLED %q", cgo)
	}
	b.once.Do(func() {
		dir := filepath.Join(sidecarTestDir, "cgo"+cgo)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			b.err = err
			return
		}
		bin := filepath.Join(dir, "a2a-serve-sidecar")
		build := exec.Command("go", "build", "-o", bin, "../../cmd/a2a-serve-sidecar")
		build.Env = append(os.Environ(), "CGO_ENABLED="+cgo, "GOOS=linux")
		if out, err := build.CombinedOutput(); err != nil {
			b.err = fmt.Errorf("go build a2a-serve-sidecar with CGO_ENABLED=%s error = %v\n%s", cgo, err, out)
			return
		}
		b.val = bin
	})
	if b.err != nil {
		t.Fatal(b.err)
	}
	return b.val
}

// buildFakeacpImage builds an image whose /usr/local/bin/fakeacp is
// fakeacp, and returns its name.
func buildFakeacpImage(t *testing.T) string {
	t.Helper()
	fakeacpImageOnce.Do(func() {
		ctxDir := filepath.Join(sidecarTestDir, "fakeacp-image")
		if err := os.MkdirAll(ctxDir, 0o755); err != nil {
			fakeacpImageErr = err
			return
		}
		build := exec.Command("go", "build", "-o", filepath.Join(ctxDir, "fakeacp"), "../agentsrv/testdata/fakeacp")
		build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux")
		if out, err := build.CombinedOutput(); err != nil {
			fakeacpImageErr = fmt.Errorf("go build fakeacp error = %v\n%s", err, out)
			return
		}
		image := "a2a-serve-sidecar-test-fakeacp:" + cryptoutil.RandomString(12, "abcdefghijklmnopqrstuvwxyz")
		docker := exec.Command("docker", "build", "-q", "-t", image, "-f", "-", ctxDir)
		docker.Stdin = strings.NewReader("FROM scratch\nCOPY fakeacp /usr/local/bin/fakeacp\n")
		if out, err := docker.CombinedOutput(); err != nil {
			fakeacpImageErr = fmt.Errorf("docker build error = %v\n%s", err, out)
			return
		}
		fakeacpImage = image
	})
	if fakeacpImageErr != nil {
		t.Fatal(fakeacpImageErr)
	}
	return fakeacpImage
}

// dockerTestEnv builds what the Docker test needs, or skips without
// Docker or the egress boundary: a2a-serve-sidecar built as releases are, and an
// image with fakeacp.
func dockerTestEnv(t *testing.T) (bin, image string) {
	t.Helper()
	requireDocker(t)
	bin = buildSidecar(t, "0")
	caps, err := sandbox.Docker(dockerOptions(bin)).Probe(context.Background())
	if err != nil {
		t.Fatalf("Docker().Probe() error = %v", err)
	}
	if !caps.Boundary {
		sandboxtest.Skip(t, "the boundary can't work here: %s", caps.Problem)
	}
	return bin, buildFakeacpImage(t)
}

// serveHost serves h on a Unix socket in dir and returns its URL.
func serveHost(t *testing.T, h *refhost.Host, dir string) string {
	t.Helper()
	sock := filepath.Join(dir, "egress.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("net.Listen() error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.Serve(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Host.Serve() error = %v", err)
		}
	})
	return "unix://" + sock
}

// startBinary runs the a2a-serve-sidecar binary bin with args and reads its ready
// line. It stops it with SIGTERM at the end of the test.
func startBinary(t *testing.T, bin string, args ...string) serve.Ready {
	t.Helper()
	return startBinaryWithEnv(t, bin, nil, args...)
}

func startBinaryWithEnv(t *testing.T, bin string, extraEnv []string, args ...string) serve.Ready {
	t.Helper()
	return startBinaryWithIO(t, bin, "", extraEnv, args...)
}

func startBinaryWithIO(t *testing.T, bin, stdin string, extraEnv []string, args ...string) serve.Ready {
	t.Helper()
	cmd := exec.Command(bin, args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	if len(extraEnv) > 0 {
		cmd.Env = append(os.Environ(), extraEnv...)
	}
	stderr := &syncBuffer{}
	cmd.Stderr = stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting a2a-serve-sidecar error = %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() {
		if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
			t.Errorf("stopping a2a-serve-sidecar error = %v", err)
		}
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("a2a-serve-sidecar exited with %v; stderr:\n%s", err, stderr)
			}
		case <-time.After(time.Minute):
			t.Errorf("a2a-serve-sidecar didn't stop after SIGTERM")
		}
	})
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil {
		t.Fatalf("reading the ready line error = %v; stderr:\n%s", err, stderr)
	}
	var ready serve.Ready
	if err := json.Unmarshal([]byte(line), &ready); err != nil || !ready.Ready {
		t.Fatalf("ready line = %q, %v, want ready; stderr:\n%s", line, err, stderr)
	}
	go func() {
		if _, err := io.Copy(io.Discard, stdout); err != nil {
			t.Logf("reading stdout: %v", err)
		}
	}()
	return ready
}

func TestEgressProxyDecidesTheDockerSandboxConnections(t *testing.T) {
	t.Parallel()
	bin, image := dockerTestEnv(t)
	dir := testutil.ShortTempDir(t)
	host := refhost.New(refhost.Config{
		Decide: func(c refhost.Connect) refhost.Decision {
			if c.Target == "blocked.test:443" {
				return refhost.Refuse("blocked.test is off limits")
			}
			return refhost.Intercept()
		},
		Answer: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if _, err := io.WriteString(w, "host answer for "+r.Header.Get("X-Api-Key")); err != nil {
				t.Errorf("answering error = %v", err)
			}
		}),
	})
	sock := filepath.Join(dir, "a2a.sock")
	ready := startBinary(t, bin, "--sandbox", dockerSandbox(), "--image", image, "--command", "/usr/local/bin/fakeacp",
		"--workspace", "dir:"+t.TempDir(), "--listen", "unix://"+sock, "--egress-proxy", serveHost(t, host, dir))
	if ready.Egress != serve.EgressProxy || !strings.HasPrefix(ready.Sandbox, dockerSandbox()) {
		t.Fatalf("ready line = %+v, want egress %q in the sandbox %s", ready, serve.EgressProxy, dockerSandbox())
	}
	c, err := a2aclient.NewFromEndpoints(context.Background(), []*a2a.AgentInterface{a2a.NewAgentInterface("http://a2a/", a2a.TransportProtocolJSONRPC)}, a2aclient.WithJSONRPCTransport(unixClient(sock)))
	if err != nil {
		t.Fatalf("a2aclient.NewFromEndpoints() error = %v", err)
	}

	task := sendTask(t, c, "ctx-e2e", "fetch https://api.anthropic.com/v1/models")
	key := sendTask(t, c, "ctx-e2e", "env ANTHROPIC_API_KEY")
	refused := sendTask(t, c, "ctx-e2e", "fetch https://blocked.test/")

	if got, want := testutil.AllArtifactText(task), "fetch 200 host answer for "; got != want {
		t.Fatalf("fetch through the egress proxy = %q, want %q", got, want)
	}
	if got := testutil.AllArtifactText(key); !strings.Contains(got, "sk-ant-api03-") {
		t.Fatalf("env ANTHROPIC_API_KEY = %q, want a placeholder key", got)
	}
	if got := testutil.AllArtifactText(refused); !strings.HasPrefix(got, "fetch error:") {
		t.Fatalf("fetch of a refused host = %q, want an error", got)
	}
	wantRequests := []refhost.Request{{Context: "ctx-e2e", Task: string(task.ID), Method: http.MethodGet, URL: "https://api.anthropic.com/v1/models"}}
	if diff := cmp.Diff(wantRequests, host.Requests()); diff != "" {
		t.Fatalf("host requests wrong result (-want +got) diff = %s", diff)
	}
	wantConnects := []refhost.Connect{
		{Context: "ctx-e2e", Target: "api.anthropic.com:443", Offered: true},
		{Context: "ctx-e2e", Target: "blocked.test:443", Offered: true},
	}
	// The agent may retry a refused connection, on another address.
	if diff := cmp.Diff(wantConnects, slices.Compact(host.Connects())); diff != "" {
		t.Fatalf("host connects wrong result (-want +got) diff = %s", diff)
	}
}

func TestReadyLineReportsNoEgressBoundary(t *testing.T) {
	t.Parallel()
	requireDocker(t)
	if out, err := exec.Command("go", "env", "CGO_ENABLED").Output(); err != nil || strings.TrimSpace(string(out)) != "1" {
		t.Skip("needs cgo, to build an a2a-serve-sidecar that the sandbox can't launch")
	}
	bin := buildSidecar(t, "1")
	if caps, err := sandbox.Docker(sandbox.DockerOptions{Launcher: bin}).Probe(context.Background()); err != nil || caps.Boundary {
		t.Skipf("Docker().Probe() = %+v, %v, want no boundary for an a2a-serve-sidecar built with cgo", caps, err)
	}
	dir := testutil.ShortTempDir(t)

	ready := startBinary(t, bin, "--sandbox", "docker", "--image", buildFakeacpImage(t), "--command", "/usr/local/bin/fakeacp",
		"--workspace", "dir:"+t.TempDir(), "--listen", "unix://"+filepath.Join(dir, "a2a.sock"))

	if ready.Egress != serve.EgressNone {
		t.Fatalf("ready line egress without a boundary = %q, want %q", ready.Egress, serve.EgressNone)
	}
}

// sendTask sends text in the context contextID and returns the completed
// task.
func sendTask(t *testing.T, c *a2aclient.Client, contextID, text string) *a2a.Task {
	t.Helper()
	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(text))
	msg.ContextID = contextID
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	res, err := c.SendMessage(ctx, &a2a.SendMessageRequest{Message: msg})
	if err != nil {
		t.Fatalf("Client.SendMessage(%q) error = %v", text, err)
	}
	task, ok := res.(*a2a.Task)
	if !ok || task.Status.State != a2a.TaskStateCompleted {
		t.Fatalf("Client.SendMessage(%q) = %#v, want a completed task", text, res)
	}
	return task
}
