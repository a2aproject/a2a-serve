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

	"github.com/a2aproject/a2a-go/v2/a2a"
)

// coalesceQueue is how many events wait while the consumer is busy.
const coalesceQueue = 256

type eventOrErr struct {
	ev  a2a.Event
	err error
}

// coalesceArtifacts merges artifact chunks which queue up while the consumer
// is busy into one event. a2a-go saves the task after every event, so an
// agent streaming one chunk per token would otherwise rewrite the task per
// token. Chunks are only merged when they already wait, so a fast consumer
// still sees every chunk at once.
func coalesceArtifacts(events iter.Seq2[a2a.Event, error]) iter.Seq2[a2a.Event, error] {
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
			for next.err == nil {
				item, ok := tryReceive(queue)
				if !ok {
					break
				}
				merged, ok := mergeChunks(next.ev, item.ev)
				if item.err != nil || !ok {
					pending = &item
					break
				}
				next.ev = merged
			}
			if !yield(next.ev, next.err) {
				return
			}
		}
	}
}

func tryReceive(queue <-chan eventOrErr) (eventOrErr, bool) {
	select {
	case item, ok := <-queue:
		return item, ok
	default:
		return eventOrErr{}, false
	}
}

// mergeChunks returns b appended to a, if b continues the artifact of a.
func mergeChunks(a, b a2a.Event) (a2a.Event, bool) {
	ea, ok := a.(*a2a.TaskArtifactUpdateEvent)
	if !ok || ea.Artifact == nil || ea.LastChunk {
		return nil, false
	}
	eb, ok := b.(*a2a.TaskArtifactUpdateEvent)
	if !ok || eb.Artifact == nil || !eb.Append || eb.Artifact.ID != ea.Artifact.ID || eb.TaskID != ea.TaskID {
		return nil, false
	}
	art := *ea.Artifact
	art.Parts = slices.Concat(ea.Artifact.Parts, eb.Artifact.Parts)
	if eb.Artifact.Metadata != nil {
		art.Metadata = maps.Clone(ea.Artifact.Metadata)
		if art.Metadata == nil {
			art.Metadata = map[string]any{}
		}
		maps.Copy(art.Metadata, eb.Artifact.Metadata)
	}
	merged := *ea
	merged.Artifact = &art
	merged.LastChunk = eb.LastChunk
	if eb.Metadata != nil {
		merged.Metadata = maps.Clone(ea.Metadata)
		if merged.Metadata == nil {
			merged.Metadata = map[string]any{}
		}
		maps.Copy(merged.Metadata, eb.Metadata)
	}
	return &merged, true
}
