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

package agentsrv

import (
	"errors"
	"iter"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/go-cmp/cmp"
)

func chunk(artifact, text string, appendTo bool) *a2a.TaskArtifactUpdateEvent {
	return &a2a.TaskArtifactUpdateEvent{TaskID: "t1", Append: appendTo, Artifact: &a2a.Artifact{ID: a2a.ArtifactID(artifact), Parts: []*a2a.Part{a2a.NewTextPart(text)}}}
}

func eventsOf(evs ...a2a.Event) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		for _, ev := range evs {
			if !yield(ev, nil) {
				return
			}
		}
	}
}

// describe renders events as "<artifact>:<text>" for chunks and the state
// for status updates.
func describe(evs []a2a.Event) []string {
	var out []string
	for _, ev := range evs {
		switch e := ev.(type) {
		case *a2a.TaskArtifactUpdateEvent:
			var sb strings.Builder
			for _, p := range e.Artifact.Parts {
				sb.WriteString(p.Text())
			}
			out = append(out, string(e.Artifact.ID)+":"+sb.String())
		case *a2a.TaskStatusUpdateEvent:
			out = append(out, string(e.Status.State))
		}
	}
	return out
}

func TestCoalesceArtifactsMergesQueuedChunks(t *testing.T) {
	t.Parallel()
	working := a2a.NewStatusUpdateEvent(&a2a.Task{ID: "t1"}, a2a.TaskStateWorking, nil)
	done := a2a.NewStatusUpdateEvent(&a2a.Task{ID: "t1"}, a2a.TaskStateCompleted, nil)
	in := eventsOf(working, chunk("a", "1", false), chunk("a", "2", true), chunk("a", "3", true), chunk("b", "x", false), chunk("b", "y", true), done)

	var got []a2a.Event
	first := true
	for ev, err := range coalesceArtifacts(in) {
		if err != nil {
			t.Fatalf("coalesceArtifacts() error = %v", err)
		}
		if first {
			first = false
			time.Sleep(100 * time.Millisecond)
		}
		got = append(got, ev)
	}

	want := []string{string(a2a.TaskStateWorking), "a:123", "b:xy", string(a2a.TaskStateCompleted)}
	if diff := cmp.Diff(want, describe(got)); diff != "" {
		t.Fatalf("coalesceArtifacts() wrong result (-want +got) diff = %s", diff)
	}
}

func TestCoalesceArtifactsKeepsErrorsAndStops(t *testing.T) {
	t.Parallel()
	failure := errors.New("agent crashed")
	in := func(yield func(a2a.Event, error) bool) {
		if !yield(chunk("a", "1", false), nil) {
			return
		}
		yield(nil, failure)
	}

	var gotErr error
	n := 0
	for _, err := range coalesceArtifacts(in) {
		n++
		if err != nil {
			gotErr = err
			break
		}
	}

	if !errors.Is(gotErr, failure) || n != 2 {
		t.Fatalf("coalesceArtifacts() = %d events, error %v, want 2 events ending with %v", n, gotErr, failure)
	}
}
