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
	"sync/atomic"
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
	for ev, err := range coalesceArtifacts(in, 0) {
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

func TestCoalesceArtifactsMergesChunksWithinTheWindow(t *testing.T) {
	t.Parallel()
	working := a2a.NewStatusUpdateEvent(&a2a.Task{ID: "t1"}, a2a.TaskStateWorking, nil)
	done := a2a.NewStatusUpdateEvent(&a2a.Task{ID: "t1"}, a2a.TaskStateCompleted, nil)
	firstSeen := make(chan struct{})
	in := func(yield func(a2a.Event, error) bool) {
		if !yield(working, nil) || !yield(chunk("a", "1", false), nil) {
			return
		}
		<-firstSeen
		if !yield(chunk("a", "2", true), nil) {
			return
		}
		time.Sleep(50 * time.Millisecond)
		if !yield(chunk("a", "3", true), nil) {
			return
		}
		yield(done, nil)
	}

	var got []a2a.Event
	for ev, err := range coalesceArtifacts(in, time.Hour) {
		if err != nil {
			t.Fatalf("coalesceArtifacts() error = %v", err)
		}
		got = append(got, ev)
		if len(got) == 2 {
			close(firstSeen)
		}
	}

	want := []string{string(a2a.TaskStateWorking), "a:1", "a:23", string(a2a.TaskStateCompleted)}
	if diff := cmp.Diff(want, describe(got)); diff != "" {
		t.Fatalf("coalesceArtifacts() wrong result (-want +got) diff = %s", diff)
	}
}

func TestCoalesceArtifactsSendsLastChunkAtOnce(t *testing.T) {
	t.Parallel()
	done := a2a.NewStatusUpdateEvent(&a2a.Task{ID: "t1"}, a2a.TaskStateCompleted, nil)
	last := chunk("a", "2", true)
	last.LastChunk = true
	firstSeen, lastSeen := make(chan struct{}), make(chan struct{})
	var late atomic.Bool
	in := func(yield func(a2a.Event, error) bool) {
		if !yield(chunk("a", "1", false), nil) {
			return
		}
		<-firstSeen
		if !yield(last, nil) {
			return
		}
		select {
		case <-lastSeen:
		case <-time.After(waitTimeout):
			late.Store(true)
		}
		yield(done, nil)
	}

	var got []a2a.Event
	for ev, err := range coalesceArtifacts(in, time.Hour) {
		if err != nil {
			t.Fatalf("coalesceArtifacts() error = %v", err)
		}
		got = append(got, ev)
		switch len(got) {
		case 1:
			close(firstSeen)
		case 2:
			close(lastSeen)
		}
	}

	if late.Load() {
		t.Fatalf("coalesceArtifacts() held the last chunk for the window, want it sent at once")
	}
	want := []string{"a:1", "a:2", string(a2a.TaskStateCompleted)}
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
	for _, err := range coalesceArtifacts(in, 0) {
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

func TestMergeChunks(t *testing.T) {
	t.Parallel()
	last := chunk("a", "3", true)
	last.LastChunk = true
	last.Artifact.Metadata = map[string]any{"k": "v"}
	withRaw := chunk("a", "2", true)
	withRaw.Artifact.Parts = append(withRaw.Artifact.Parts, a2a.NewRawPart([]byte("png")))
	tests := []struct {
		name   string
		chunks []*a2a.TaskArtifactUpdateEvent
		want   *a2a.TaskArtifactUpdateEvent
	}{
		{
			name:   "text joined",
			chunks: []*a2a.TaskArtifactUpdateEvent{chunk("a", "1", false), chunk("a", "2", true), last},
			want: &a2a.TaskArtifactUpdateEvent{TaskID: "t1", LastChunk: true, Artifact: &a2a.Artifact{
				ID: "a", Parts: []*a2a.Part{a2a.NewTextPart("123")}, Metadata: map[string]any{"k": "v"},
			}},
		},
		{
			name:   "raw part kept",
			chunks: []*a2a.TaskArtifactUpdateEvent{chunk("a", "1", true), withRaw, chunk("a", "3", true)},
			want: &a2a.TaskArtifactUpdateEvent{TaskID: "t1", Append: true, Artifact: &a2a.Artifact{
				ID: "a", Parts: []*a2a.Part{a2a.NewTextPart("12"), a2a.NewRawPart([]byte("png")), a2a.NewTextPart("3")},
			}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if diff := cmp.Diff(tc.want, mergeChunks(tc.chunks)); diff != "" {
				t.Fatalf("mergeChunks() wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}
