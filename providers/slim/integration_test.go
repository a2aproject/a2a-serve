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

package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"iter"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/a2aproject/a2a-go/v2/a2aclient/agentcard"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	a2aslimrpc "github.com/agntcy/slim-a2a-go/a2aslimrpc/v1"
	slim_bindings "github.com/agntcy/slim-bindings-go/v3"
	slim_rpc "github.com/agntcy/slim-bindings-go/v3/slim_rpc"
	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/netprovider"
	"github.com/a2aproject/a2a-serve/netprovider/conformance"
	"github.com/a2aproject/a2a-serve/netprovider/host"
)

const testSecret = "a2a-serve-slim-provider-test-secret"

var (
	providerBin string
	endpoint    = "http://127.0.0.1:46357"
)

func TestMain(m *testing.M) {
	if e := os.Getenv("SLIM_ENDPOINT"); e != "" {
		endpoint = e
	}
	dir, err := os.MkdirTemp("", "netprovider-slim")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	providerBin = filepath.Join(dir, "netprovider-slim")
	if out, err := exec.Command("go", "build", "-o", providerBin, ".").CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "building the provider: %v\n%s", err, out)
		os.Exit(1)
	}
	code := m.Run()
	if err := os.RemoveAll(dir); err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
	os.Exit(code)
}

// nodeReachable reports whether the SLIM node at endpoint takes connections.
func nodeReachable() error {
	u, err := url.Parse(endpoint)
	if err != nil {
		return err
	}
	conn, err := net.DialTimeout("tcp", u.Host, 2*time.Second)
	if err != nil {
		return err
	}
	return conn.Close()
}

func requireNode(t *testing.T) {
	t.Helper()
	if err := nodeReachable(); err != nil {
		t.Skipf("no SLIM node at %s (%v); start one, or set SLIM_ENDPOINT", endpoint, err)
	}
}

// namespace returns a namespace no other test run uses.
func namespace(t *testing.T) string {
	t.Helper()
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("rand.Read() error = %v", err)
	}
	return "test-" + hex.EncodeToString(b)
}

func runDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "slim")
	if err != nil {
		t.Fatalf("os.MkdirTemp() error = %v", err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Errorf("os.RemoveAll() error = %v", err)
		}
	})
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatalf("os.Chmod() error = %v", err)
	}
	return dir
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(b []byte) (int, error) {
	w.t.Logf("provider: %s", strings.TrimRight(string(b), "\n"))
	return len(b), nil
}

func ctx(t *testing.T) context.Context {
	c, cancel := context.WithTimeout(t.Context(), time.Minute)
	t.Cleanup(cancel)
	return c
}

func start(t *testing.T) *host.Process {
	t.Helper()
	p, err := host.Start(host.Config{Path: providerBin, RunDir: runDir(t), Env: host.Environ(os.Environ()), Stderr: testWriter{t}})
	if err != nil {
		t.Fatalf("host.Start() error = %v", err)
	}
	t.Cleanup(func() {
		if err := p.Close(); err != nil {
			t.Errorf("Process.Close() error = %v", err)
		}
	})
	return p
}

// agent is a provider process with a fake a2a-serve behind it: an echo
// agent that records who calls it.
type agent struct {
	p  *host.Process
	ep *host.Endpoints

	mu      sync.Mutex
	callers map[netprovider.Caller]bool
}

func join(t *testing.T, network, name string) *agent {
	t.Helper()
	a := &agent{p: start(t), callers: map[netprovider.Caller]bool{}}
	res, err := a.p.Join(ctx(t), netprovider.JoinRequest{Agent: name, URL: network, Token: testSecret, StateDir: t.TempDir(), Private: true})
	if err != nil {
		t.Fatalf("Process.Join() error = %v", err)
	}
	u, err := url.Parse(network)
	if err != nil {
		t.Fatalf("url.Parse() error = %v", err)
	}
	if want := "a2a/" + u.Query().Get("namespace") + "/" + name; res.Node != want {
		t.Fatalf("Process.Join() node = %q, want %q", res.Node, want)
	}
	if a.ep, err = a.p.Listen(ctx(t), true, false); err != nil {
		t.Fatalf("Process.Listen() error = %v", err)
	}
	a.serveEcho(t, name)
	return a
}

func (a *agent) serveEcho(t *testing.T, name string) {
	t.Helper()
	card := &a2a.AgentCard{
		Name:                name,
		Description:         "echoes messages",
		Version:             "1.0.0",
		SupportedInterfaces: []*a2a.AgentInterface{a2a.NewAgentInterface(a.ep.PrivateURL, a2a.TransportProtocolJSONRPC)},
		Capabilities:        a2a.AgentCapabilities{Streaming: true},
		DefaultInputModes:   []string{"text/plain"},
		DefaultOutputModes:  []string{"text/plain"},
		Skills:              []a2a.AgentSkill{{ID: "echo", Name: "echo", Description: "echoes messages", Tags: []string{"echo"}}},
	}
	mux := http.NewServeMux()
	mux.Handle("GET "+a2asrv.WellKnownAgentCardPath, a2asrv.NewStaticAgentCardHandler(card))
	mux.Handle("POST /{$}", a2asrv.NewJSONRPCHandler(a2asrv.NewHandler(echoExecutor{})))
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, ok := a.ep.Private.Caller(r.RemoteAddr)
		if !ok {
			http.Error(w, "no caller", http.StatusInternalServerError)
			return
		}
		a.mu.Lock()
		a.callers[c] = true
		a.mu.Unlock()
		mux.ServeHTTP(w, r)
	})}
	go func() {
		if err := srv.Serve(a.ep.Private); err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("Server.Serve() error = %v", err)
		}
	}()
	t.Cleanup(func() {
		if err := srv.Close(); err != nil {
			t.Errorf("Server.Close() error = %v", err)
		}
	})
}

func (a *agent) seenCallers() map[netprovider.Caller]bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := map[netprovider.Caller]bool{}
	for c := range a.callers {
		out[c] = true
	}
	return out
}

type echoExecutor struct{}

func (echoExecutor) Execute(_ context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		if ec.StoredTask == nil && !yield(a2a.NewSubmittedTask(ec, ec.Message), nil) {
			return
		}
		if !yield(a2a.NewArtifactEvent(ec, a2a.NewTextPart("echo: "+text(ec.Message))), nil) {
			return
		}
		yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateCompleted, nil), nil)
	}
}

func (echoExecutor) Cancel(_ context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateCanceled, nil), nil)
	}
}

func message(s string) *a2a.SendMessageRequest {
	return &a2a.SendMessageRequest{Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(s))}
}

// streamText returns the text of the artifacts in a stream and its last
// task state.
func streamText(t *testing.T, events iter.Seq2[a2a.Event, error]) (string, a2a.TaskState) {
	t.Helper()
	var s string
	var state a2a.TaskState
	for ev, err := range events {
		if err != nil {
			t.Fatalf("stream error = %v", err)
		}
		switch e := ev.(type) {
		case *a2a.TaskArtifactUpdateEvent:
			s += text(&a2a.Message{Parts: e.Artifact.Parts})
		case *a2a.TaskStatusUpdateEvent:
			state = e.Status.State
		case *a2a.Task:
			state = e.Status.State
		}
	}
	return s, state
}

func hasInterface(card *a2a.AgentCard, binding a2a.TransportProtocol, u string) bool {
	for _, iface := range card.SupportedInterfaces {
		if iface.ProtocolBinding == binding && iface.URL == u {
			return true
		}
	}
	return false
}

func slimCaller(n string) netprovider.Caller {
	return netprovider.Caller{Node: n, Machine: n, ClientKey: "slim:" + n}
}

func TestAgentsCallEachOther(t *testing.T) {
	t.Parallel()
	requireNode(t)
	ns := namespace(t)
	network := endpoint + "?namespace=" + ns + "&peers=reviewer,coder"
	coder := join(t, network, "coder")
	reviewer := join(t, network, "reviewer")
	coderURL := "http://slim.local/a2a/" + ns + "/coder"

	if coder.ep.PrivateURL != coderURL {
		t.Fatalf("PrivateURL = %q, want %q", coder.ep.PrivateURL, coderURL)
	}
	peers, err := reviewer.p.Peers(ctx(t))
	if err != nil {
		t.Fatalf("Process.Peers() error = %v", err)
	}
	wantPeers := []netprovider.PeerInfo{{Name: "coder", URL: coderURL, Node: "a2a/" + ns + "/coder"}}
	if diff := cmp.Diff(wantPeers, peers); diff != "" {
		t.Fatalf("Process.Peers() wrong result (-want +got) diff = %s", diff)
	}

	httpClient := &http.Client{Transport: reviewer.p.Transport(), Timeout: time.Minute}
	card, err := agentcard.NewResolver(httpClient).Resolve(ctx(t), coderURL, agentcard.WithPath(a2asrv.WellKnownAgentCardPath))
	if err != nil {
		t.Fatalf("Resolver.Resolve() error = %v", err)
	}
	if !hasInterface(card, a2a.TransportProtocolJSONRPC, coderURL) || !hasInterface(card, a2aslimrpc.SLIMProtocol, "a2a/"+ns+"/coder") {
		t.Fatalf("card interfaces = %+v, want JSON-RPC at %s and SLIMRPC at a2a/%s/coder", card.SupportedInterfaces, coderURL, ns)
	}
	client, err := a2aclient.NewFromCard(ctx(t), card, a2aclient.WithJSONRPCTransport(httpClient))
	if err != nil {
		t.Fatalf("a2aclient.NewFromCard() error = %v", err)
	}
	t.Cleanup(func() {
		if err := client.Destroy(); err != nil {
			t.Errorf("Client.Destroy() error = %v", err)
		}
	})

	res, err := client.SendMessage(ctx(t), message("hello"))
	if err != nil {
		t.Fatalf("Client.SendMessage() error = %v", err)
	}
	if got, want := resultText(t, res), "echo: hello"; got != want {
		t.Fatalf("Client.SendMessage() = %q, want %q", got, want)
	}
	got, state := streamText(t, client.SendStreamingMessage(ctx(t), message("streamed")))
	if got != "echo: streamed" || state != a2a.TaskStateCompleted {
		t.Fatalf("Client.SendStreamingMessage() = %q, %v; want %q, %v", got, state, "echo: streamed", a2a.TaskStateCompleted)
	}

	want := map[netprovider.Caller]bool{slimCaller("a2a/" + ns + "/reviewer"): true}
	if diff := cmp.Diff(want, coder.seenCallers()); diff != "" {
		t.Fatalf("callers of coder wrong result (-want +got) diff = %s", diff)
	}
}

var slimService = sync.OnceValue(func() *slim_bindings.Service {
	slim_bindings.InitializeWithDefaults()
	return slim_bindings.GetGlobalService()
})

// newSLIMTransport returns a SLIM A2A transport to target from a SLIM app
// of its own, self.
func newSLIMTransport(t *testing.T, self, target *slim_bindings.Name) *a2aslimrpc.Transport {
	t.Helper()
	svc := slimService()
	app, err := svc.CreateAppWithSecret(self, testSecret)
	if err != nil {
		t.Fatalf("Service.CreateAppWithSecret() error = %v", err)
	}
	t.Cleanup(app.Destroy)
	conn, err := svc.Connect(slim_bindings.NewInsecureClientConfig(endpoint))
	if err != nil {
		id := svc.GetConnectionId(endpoint)
		if id == nil {
			t.Fatalf("Service.Connect() error = %v", err)
		}
		conn = *id
	}
	if err := app.Subscribe(self, &conn); err != nil {
		t.Fatalf("App.Subscribe() error = %v", err)
	}
	tr := a2aslimrpc.NewTransport(slim_rpc.ChannelNewWithConnection(app, target, &conn))
	t.Cleanup(func() {
		if err := tr.Destroy(); err != nil {
			t.Errorf("Transport.Destroy() error = %v", err)
		}
	})
	return tr
}

func TestSLIMClientCallsAgent(t *testing.T) {
	t.Parallel()
	requireNode(t)
	ns := namespace(t)
	coder := join(t, endpoint+"?namespace="+ns, "coder")
	tr := newSLIMTransport(t, slim_bindings.NewName("a2a", ns, "client"), slim_bindings.NewName("a2a", ns, "coder"))

	card, err := tr.GetExtendedAgentCard(ctx(t), nil, &a2a.GetExtendedAgentCardRequest{})
	if err != nil {
		t.Fatalf("Transport.GetExtendedAgentCard() error = %v", err)
	}
	if !hasInterface(card, a2aslimrpc.SLIMProtocol, "a2a/"+ns+"/coder") {
		t.Fatalf("card interfaces = %+v, want SLIMRPC at a2a/%s/coder", card.SupportedInterfaces, ns)
	}
	res, err := tr.SendMessage(ctx(t), nil, message("over slim"))
	if err != nil {
		t.Fatalf("Transport.SendMessage() error = %v", err)
	}
	if got, want := resultText(t, res), "echo: over slim"; got != want {
		t.Fatalf("Transport.SendMessage() = %q, want %q", got, want)
	}
	got, state := streamText(t, tr.SendStreamingMessage(ctx(t), nil, message("streamed")))
	if got != "echo: streamed" || state != a2a.TaskStateCompleted {
		t.Fatalf("Transport.SendStreamingMessage() = %q, %v; want %q, %v", got, state, "echo: streamed", a2a.TaskStateCompleted)
	}

	want := map[netprovider.Caller]bool{slimCaller("a2a/" + ns + "/client"): true}
	if diff := cmp.Diff(want, coder.seenCallers()); diff != "" {
		t.Fatalf("callers of coder wrong result (-want +got) diff = %s", diff)
	}
}

func TestJoinErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		req       netprovider.JoinRequest
		wantToken bool
		wantText  string
	}{
		{
			name:      "no token",
			req:       netprovider.JoinRequest{Agent: "coder", URL: endpoint, Private: true},
			wantToken: true,
			wantText:  "shared secret",
		},
		{
			name:      "short token",
			req:       netprovider.JoinRequest{Agent: "coder", URL: endpoint, Token: "short", Private: true},
			wantToken: true,
			wantText:  "32 bytes",
		},
		{
			name:     "public URL",
			req:      netprovider.JoinRequest{Agent: "coder", URL: endpoint, Token: testSecret, Private: true, Public: true},
			wantText: "--public tailscale or a relay",
		},
		{
			name:     "unreachable node",
			req:      netprovider.JoinRequest{Agent: "coder", URL: "http://127.0.0.1:1", Token: testSecret, Private: true},
			wantText: "connecting to the SLIM node at http://127.0.0.1:1",
		},
		{
			name:     "unknown setting",
			req:      netprovider.JoinRequest{Agent: "coder", URL: endpoint + "?space=demo", Token: testSecret, Private: true},
			wantText: `unknown setting "space"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tt.req.StateDir = t.TempDir()
			_, err := start(t).Join(ctx(t), tt.req)
			if err == nil || !strings.Contains(err.Error(), tt.wantText) {
				t.Fatalf("Process.Join() error = %v, want one that says %q", err, tt.wantText)
			}
			var e *netprovider.Error
			if gotToken := errors.As(err, &e) && e.Kind == netprovider.KindToken; gotToken != tt.wantToken {
				t.Fatalf("Process.Join() error = %#v, want a token error: %v", err, tt.wantToken)
			}
		})
	}
}

func TestConformance(t *testing.T) {
	t.Parallel()
	cfg := conformance.Config{Binary: providerBin, Token: testSecret}
	if err := nodeReachable(); err == nil {
		cfg.URL = endpoint + "?namespace=" + namespace(t)
	} else {
		t.Logf("no SLIM node at %s (%v); skipping the network checks", endpoint, err)
	}
	conformance.Run(t, cfg)
}
