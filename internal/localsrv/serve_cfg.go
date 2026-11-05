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

// Package localsrv runs local A2A servers in echo, script and proxy modes for
// development and testing.
package localsrv

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"

	"google.golang.org/grpc"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2acompat/a2av0"
	a2agrpcv0 "github.com/a2aproject/a2a-go/v2/a2agrpc/v0"
	a2agrpc "github.com/a2aproject/a2a-go/v2/a2agrpc/v1"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
)

// LoggerFunc logs a formatted diagnostic message from the local server.
type LoggerFunc func(format string, args ...any)

// Config holds the settings shared by the local server modes (echo, script, proxy).
type Config struct {
	CardParams

	Listener        net.Listener
	ProtocolVersion a2a.ProtocolVersion
	Logger          LoggerFunc
	CardCompat      bool

	Quiet bool
}

// serve starts a server with the approprate transport.
func serve(ctx context.Context, cfg Config, handler a2asrv.RequestHandler, card *a2a.AgentCard) error {
	if cfg.Transport == a2a.TransportProtocolGRPC {
		return startGRPCServer(ctx, cfg, handler, card)
	}
	mux := buildMux(handler, card, cfg.Transport, cfg)
	return startHTTPServer(ctx, cfg, mux)
}

func startHTTPServer(ctx context.Context, cfg Config, handler http.Handler) error {
	addr := cfg.Listener.Addr().String()
	srv := &http.Server{Handler: protect(handler, cfg.Listener.Addr())}

	go func() {
		<-ctx.Done()
		if err := srv.Shutdown(context.Background()); err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "HTTP server shutdown: %v\n", err)
		}
	}()

	if !cfg.Quiet {
		fmt.Fprintf(os.Stderr, "Listening on %s\n", addr)
	}

	if err := srv.Serve(cfg.Listener); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// protect refuses cross-site requests that change state. On a loopback
// listener it also refuses other host names, which a web page reaches
// through DNS rebinding.
func protect(h http.Handler, addr net.Addr) http.Handler {
	h = http.NewCrossOriginProtection().Handler(h)
	tcp, ok := addr.(*net.TCPAddr)
	if !ok || !tcp.IP.IsLoopback() {
		return h
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !loopbackHost(r.Host) {
			http.Error(w, "the server answers only requests for localhost", http.StatusForbidden)
			return
		}
		h.ServeHTTP(w, r)
	})
}

func loopbackHost(hostport string) bool {
	host, _, err := net.SplitHostPort(hostport)
	if err != nil {
		host = hostport
	}
	host = strings.TrimSuffix(strings.Trim(host, "[]"), ".")
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// listenCard opens the agent card listener of a gRPC server on the host of
// its listener.
func listenCard(ln net.Listener) (net.Listener, error) {
	host, _, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		return nil, err
	}
	return net.Listen("tcp", net.JoinHostPort(host, "0"))
}

func startGRPCServer(ctx context.Context, cfg Config, handler a2asrv.RequestHandler, card *a2a.AgentCard) error {
	s := grpc.NewServer()
	if cfg.ProtocolVersion == "0.3" {
		a2agrpcv0.NewHandler(handler).RegisterWith(s)
	} else {
		a2agrpc.NewHandler(handler).RegisterWith(s)
	}

	cardMux := http.NewServeMux()
	cardMux.Handle(a2asrv.WellKnownAgentCardPath, agentCardHandler(card, cfg))
	cardListener, err := listenCard(cfg.Listener)
	if err != nil {
		return fmt.Errorf("creating agent card listener: %w", err)
	}

	if !cfg.Quiet {
		fmt.Fprintf(os.Stderr, "gRPC listening on %s\n", cfg.Listener.Addr())
		fmt.Fprintf(os.Stderr, "Agent card at http://%s%s\n", cardListener.Addr(), a2asrv.WellKnownAgentCardPath)
	}

	go func() {
		<-ctx.Done()
		s.GracefulStop()
		if err := cardListener.Close(); err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "Agent card listener close: %v\n", err)
		}
	}()

	go func() {
		if err := http.Serve(cardListener, cardMux); err != nil && !errors.Is(err, net.ErrClosed) {
			_, _ = fmt.Fprintf(os.Stderr, "Agent card server: %v\n", err)
		}
	}()

	if err := s.Serve(cfg.Listener); err != nil {
		return fmt.Errorf("gRPC server: %w", err)
	}
	return nil
}

func buildMux(handler a2asrv.RequestHandler, card *a2a.AgentCard, transport a2a.TransportProtocol, sc Config) *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle(a2asrv.WellKnownAgentCardPath, agentCardHandler(card, sc))

	if sc.ProtocolVersion == a2av0.Version {
		switch transport {
		case a2a.TransportProtocolJSONRPC:
			mux.Handle("/", a2av0.NewJSONRPCHandler(handler))
		default:
			mux.Handle("/", a2av0.NewRESTHandler(handler))
		}
	} else {
		switch transport {
		case a2a.TransportProtocolJSONRPC:
			mux.Handle("/", a2asrv.NewJSONRPCHandler(handler))
		default:
			mux.Handle("/", a2asrv.NewRESTHandler(handler))
		}
	}
	return mux
}

func agentCardHandler(card *a2a.AgentCard, sc Config) http.Handler {
	if sc.CardCompat {
		return a2asrv.NewAgentCardHandler(a2av0.NewStaticAgentCardProducer(card))
	}
	return a2asrv.NewStaticAgentCardHandler(card)
}
