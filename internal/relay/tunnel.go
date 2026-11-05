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

package relay

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"regexp"
	"syscall"
	"time"
)

// tunnelURL matches the URL a Cloudflare quick tunnel prints.
var tunnelURL = regexp.MustCompile(`https://[a-z0-9-]+\.trycloudflare\.com`)

// tunnel is a running cloudflared quick tunnel. err is set when exited
// closes.
type tunnel struct {
	url    string
	exited chan struct{}
	err    error
	stop   context.CancelFunc
}

// startTunnel runs cloudflared from PATH with a quick tunnel to target, and
// waits for the URL it prints. Its output goes to log.
func startTunnel(ctx context.Context, cloudflared, target string, log io.Writer) (*tunnel, error) {
	ctx, stop := context.WithCancel(ctx)
	cmd := exec.CommandContext(ctx, cloudflared, "tunnel", "--no-autoupdate", "--url", target)
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 10 * time.Second
	pr, pw := io.Pipe()
	cmd.Stdout, cmd.Stderr = pw, pw
	if err := cmd.Start(); err != nil {
		stop()
		return nil, fmt.Errorf("running cloudflared for --tunnel cloudflare: %w", err)
	}
	t := &tunnel{exited: make(chan struct{}), stop: stop}
	go func() {
		t.err = cmd.Wait()
		close(t.exited)
		if err := pw.CloseWithError(errors.New("cloudflared exited")); err != nil {
			slog.Debug("closing the cloudflared output", "error", err)
		}
	}()
	found := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(pr)
		for sc.Scan() {
			line := sc.Text()
			if _, err := fmt.Fprintln(log, line); err != nil {
				continue
			}
			if u := tunnelURL.FindString(line); u != "" {
				select {
				case found <- u:
				default:
				}
			}
		}
	}()
	select {
	case t.url = <-found:
		return t, nil
	case <-t.exited:
		stop()
		return nil, fmt.Errorf("cloudflared exited before it printed the tunnel URL: %v", t.err)
	case <-time.After(time.Minute):
		return nil, errors.Join(errors.New("cloudflared printed no tunnel URL within a minute"), t.close())
	case <-ctx.Done():
		return nil, errors.Join(ctx.Err(), t.close())
	}
}

// close stops cloudflared and waits for it.
func (t *tunnel) close() error {
	t.stop()
	<-t.exited
	var exit *exec.ExitError
	if errors.As(t.err, &exit) || errors.Is(t.err, context.Canceled) {
		return nil
	}
	return t.err
}
