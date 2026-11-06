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
	"github.com/a2aproject/a2a-serve/internal/messenger"
	"github.com/a2aproject/a2a-serve/internal/messenger/configured"
)

// buildMessengers creates the messengers of the config.
func (s *Server) buildMessengers() ([]messenger.Messenger, error) {
	return configured.All(s.rt.Config(), s.rt.Secrets(), s.messengerOptions())
}

func (s *Server) buildMessenger(cfg *agentdir.Config, secrets agentdir.Secrets, kind string) (messenger.Messenger, error) {
	return configured.New(cfg, secrets, kind, s.messengerOptions())
}

func (s *Server) messengerOptions() configured.Options {
	return configured.Options{KV: s.store.MessengerState(), TelegramURL: s.telegramServerURL, SaveSecret: s.rt.ReplaceSecret}
}
