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

package sandboxnet

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-serve/internal/egress"
)

func TestRelayReachesTheBoundaryOfItsKey(t *testing.T) {
	t.Parallel()
	echo := echoServer(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	relays := ServeRelays(ln)
	t.Cleanup(func() {
		if err := relays.Close(); err != nil {
			t.Errorf("RelayServer.Close() error = %v", err)
		}
	})
	b, err := New(BoundaryConfig{Policy: func() egress.Policy { return mustPolicy(t, "", "127.0.0.1") }, Log: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := b.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})
	key, unregister, err := relays.Register(b)
	if err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	defer unregister()

	dir, err := os.MkdirTemp("", "relay")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Errorf("removing %s error = %v", dir, err)
		}
	})
	good, bad := filepath.Join(dir, "egress.sock"), filepath.Join(dir, "bad.sock")
	ctx, cancel := context.WithCancel(context.Background())
	var ready bytes.Buffer
	done := make(chan error, 1)
	go func() {
		done <- Relay(ctx, []RelayPair{
			{Listen: good, Target: "tcp://" + ln.Addr().String(), Key: key},
			{Listen: bad, Target: "tcp://" + ln.Addr().String(), Key: "wrong"},
		}, &ready)
	}()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Relay() error = %v", err)
		}
	})
	waitForSocket(t, good)
	waitForSocket(t, bad)

	c, br, resp := connect(t, good, echo.String())
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT through the relay status = %d, want 200", resp.StatusCode)
	}
	if _, err := fmt.Fprint(c, "ping"); err != nil {
		t.Fatal(err)
	}
	if err := c.(*net.UnixConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(br)
	if err != nil || string(got) != "ping" {
		t.Fatalf("tunnel through the relay echoed %q, %v, want %q", got, err, "ping")
	}
	if !strings.Contains(ready.String(), RelayReady) {
		t.Fatalf("Relay() printed %q, want %q", ready.String(), RelayReady)
	}

	wrong, err := net.Dial("unix", bad)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := wrong.Close(); err != nil {
			t.Errorf("closing the connection error = %v", err)
		}
	}()
	if _, err := fmt.Fprintf(wrong, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", echo, echo); err != nil {
		t.Fatal(err)
	}
	if err := wrong.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := http.ReadResponse(bufio.NewReader(wrong), nil); err == nil {
		t.Fatalf("a relay with a wrong key got a response, want the connection closed")
	}
}

func waitForSocket(t *testing.T, path string) {
	t.Helper()
	for range 100 {
		if c, err := net.Dial("unix", path); err == nil {
			if err := c.Close(); err != nil {
				t.Fatal(err)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s doesn't listen", path)
}
