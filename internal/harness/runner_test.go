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
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

func TestDockerRunnerStopSurvivesCancelledContext(t *testing.T) {
	log := fakeDocker(t, "exit 0")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	r := &DockerRunner{Name: "a2a-coder"}
	if err := r.Stop(ctx); err != nil {
		t.Fatalf("DockerRunner.Stop() error = %v", err)
	}

	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("os.ReadFile() error = %v", err)
	}
	if diff := cmp.Diff("rm -f a2a-coder\n", string(data)); diff != "" {
		t.Fatalf("DockerRunner.Stop() wrong docker calls (-want +got) diff = %s", diff)
	}
}

func TestPruneContainers(t *testing.T) {
	log := fakeDocker(t, `case "$1" in
ps) printf 'abc\ndef\n' ;;
esac
`)

	n, err := PruneContainers(context.Background(), map[string]string{"a2a.dir": "/work/agent", "a2a.agent": "coder"})
	if err != nil {
		t.Fatalf("PruneContainers() error = %v", err)
	}
	if n != 2 {
		t.Fatalf("PruneContainers() = %d, want 2", n)
	}

	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("os.ReadFile() error = %v", err)
	}
	want := []string{
		"ps -aq --filter label=a2a.agent=coder --filter label=a2a.dir=/work/agent",
		"rm -f -v abc def",
	}
	if diff := cmp.Diff(want, strings.Split(strings.TrimSpace(string(data)), "\n")); diff != "" {
		t.Fatalf("PruneContainers() wrong docker calls (-want +got) diff = %s", diff)
	}
}

func TestPruneContainersWithoutMatches(t *testing.T) {
	log := fakeDocker(t, "exit 0")

	n, err := PruneContainers(context.Background(), map[string]string{"a2a.dir": "/work/agent"})
	if err != nil {
		t.Fatalf("PruneContainers() error = %v", err)
	}
	if n != 0 {
		t.Fatalf("PruneContainers() = %d, want 0", n)
	}
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("os.ReadFile() error = %v", err)
	}
	if diff := cmp.Diff("ps -aq --filter label=a2a.dir=/work/agent\n", string(data)); diff != "" {
		t.Fatalf("PruneContainers() wrong docker calls (-want +got) diff = %s", diff)
	}
}

func TestPruneContainersWithoutAnswer(t *testing.T) {
	fakeDocker(t, "exec sleep 60")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, err := pruneContainers(ctx, map[string]string{"a2a.dir": "/work/agent"}, 100*time.Millisecond)
	if !errors.Is(err, ErrDockerNoAnswer) {
		t.Fatalf("pruneContainers() error = %v, want %v", err, ErrDockerNoAnswer)
	}
}
