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
	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/channel"
	"github.com/a2aproject/a2a-serve/internal/channel/configured"
)

// buildChannels creates the channels of the config.
func (s *Server) buildChannels() ([]channel.Channel, error) {
	return configured.All(s.config(), s.secrets(), s.channelOptions())
}

func (s *Server) buildChannel(cfg *agentdir.Config, secrets agentdir.Secrets, kind string) (channel.Channel, error) {
	return configured.New(cfg, secrets, kind, s.channelOptions())
}

func (s *Server) channelOptions() configured.Options {
	return configured.Options{KV: s.store.ChannelState(), TelegramURL: s.telegramServerURL}
}
