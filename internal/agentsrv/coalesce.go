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
	"iter"
	"maps"
	"slices"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/a2aproject/a2a-serve/internal/a2atext"
)

// coalesceQueue is how many events wait while the consumer is busy.
const coalesceQueue = 256

// coalesceWindow is the least time between two events of chunks that
// continue one artifact.
const coalesceWindow = 100 * time.Millisecond

type eventOrErr struct {
	ev  a2a.Event
	err error
}

// coalesceArtifacts merges artifact chunks which queue up while the consumer
// is busy into one event. a2a-go saves the task after every event, so an
// agent streaming one chunk per token would otherwise rewrite the task per
// token. a2a-go buffers events before it saves them, so a slow consumer
// doesn't hold up yield. After an event of chunks, the next one therefore
// also takes in the chunks that come within window. Otherwise up to a
// buffer of unmerged chunks waits to be saved ahead of a cancel.
func coalesceArtifacts(events iter.Seq2[a2a.Event, error], window time.Duration) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		queue := make(chan eventOrErr, coalesceQueue)
		done := make(chan struct{})
		go func() {
			defer close(queue)
			for ev, err := range events {
				select {
				case queue <- eventOrErr{ev, err}:
				case <-done:
					return
				}
			}
		}()
		// The producer must stop before Execute returns, so that the agent
		// doesn't keep running for a consumer which went away.
		defer func() {
			close(done)
			for range queue {
			}
		}()
		var pending *eventOrErr
		var hold time.Time
		for {
			next := pending
			pending = nil
			if next == nil {
				item, ok := <-queue
				if !ok {
					return
				}
				next = &item
			}
			var chunks []*a2a.TaskArtifactUpdateEvent
			if first, ok := next.ev.(*a2a.TaskArtifactUpdateEvent); ok && next.err == nil {
				chunks = append(chunks, first)
			}
			for len(chunks) > 0 && !chunks[len(chunks)-1].LastChunk {
				item, ok := receiveBefore(queue, hold)
				if !ok {
					break
				}
				chunk, ok := continues(chunks[len(chunks)-1], item)
				if !ok {
					pending = &item
					break
				}
				chunks = append(chunks, chunk)
			}
			if len(chunks) > 1 {
				next.ev = mergeChunks(chunks)
			}
			if !yield(next.ev, next.err) {
				return
			}
			if len(chunks) > 0 {
				hold = time.Now().Add(window)
			}
		}
	}
}

// receiveBefore returns the next item of queue, waiting for it until
// deadline. It reports false if the queue closed or no item came in time.
func receiveBefore(queue <-chan eventOrErr, deadline time.Time) (eventOrErr, bool) {
	wait := time.Until(deadline)
	if wait <= 0 {
		select {
		case item, ok := <-queue:
			return item, ok
		default:
			return eventOrErr{}, false
		}
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case item, ok := <-queue:
		return item, ok
	case <-timer.C:
		return eventOrErr{}, false
	}
}

// continues returns the chunk of item if it appends to the artifact of prev.
func continues(prev *a2a.TaskArtifactUpdateEvent, item eventOrErr) (*a2a.TaskArtifactUpdateEvent, bool) {
	if item.err != nil || prev.Artifact == nil || prev.LastChunk {
		return nil, false
	}
	next, ok := item.ev.(*a2a.TaskArtifactUpdateEvent)
	if !ok || next.Artifact == nil || !next.Append || next.Artifact.ID != prev.Artifact.ID || next.TaskID != prev.TaskID {
		return nil, false
	}
	return next, true
}

// mergeChunks returns chunks, each of which continues the one before, as one
// event. Adjacent text parts become one part.
func mergeChunks(chunks []*a2a.TaskArtifactUpdateEvent) *a2a.TaskArtifactUpdateEvent {
	first := chunks[0]
	merged, art := *first, *first.Artifact
	parts := slices.Clone(first.Artifact.Parts)
	art.Metadata, merged.Metadata = maps.Clone(first.Artifact.Metadata), maps.Clone(first.Metadata)
	for _, c := range chunks[1:] {
		parts = append(parts, c.Artifact.Parts...)
		art.Metadata = addMetadata(art.Metadata, c.Artifact.Metadata)
		merged.Metadata = addMetadata(merged.Metadata, c.Metadata)
		merged.LastChunk = c.LastChunk
	}
	art.Parts = a2atext.MergeChunks(parts, max(len(first.Artifact.Parts)-1, 0))
	merged.Artifact = &art
	return &merged
}

func addMetadata(m, more map[string]any) map[string]any {
	if more == nil {
		return m
	}
	if m == nil {
		m = make(map[string]any, len(more))
	}
	maps.Copy(m, more)
	return m
}
