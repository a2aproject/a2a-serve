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

// Package channeltest provides fakes for testing channels.
package channeltest

import (
	"context"
	"maps"
	"sync"

	"github.com/a2aproject/a2a-serve/internal/channel"
)

// KV is an in-memory [channel.KV].
type KV struct {
	mu sync.Mutex
	m  map[string]string
}

var _ channel.KV = (*KV)(nil)

// NewKV returns a KV holding a copy of init.
func NewKV(init map[string]string) *KV {
	m := map[string]string{}
	maps.Copy(m, init)
	return &KV{m: m}
}

// Get implements [channel.KV].
func (kv *KV) Get(_ context.Context, key string) (string, bool, error) {
	kv.mu.Lock()
	defer kv.mu.Unlock()
	v, ok := kv.m[key]
	return v, ok, nil
}

// Set implements [channel.KV].
func (kv *KV) Set(_ context.Context, key, value string) error {
	kv.mu.Lock()
	defer kv.mu.Unlock()
	kv.m[key] = value
	return nil
}

// All returns a copy of the stored values.
func (kv *KV) All() map[string]string {
	kv.mu.Lock()
	defer kv.mu.Unlock()
	return maps.Clone(kv.m)
}
