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

package sandbox

import (
	"crypto/rand"
	"errors"
	"io"
	"log/slog"
	"sync"
)

// EnvProcessMark names the variable that marks an agent process on the host
// and the processes it starts.
const EnvProcessMark = "A2A_SERVE_PROCESS"

// ProcessMark marks an agent process that runs on the host, and the
// processes it starts, through a variable they inherit. Agents start tools
// in process groups and sessions of their own, so the mark is what finds
// them after the agent process exits.
type ProcessMark struct{ id string }

// NewProcessMark returns a mark of its own for an agent process.
func NewProcessMark() ProcessMark { return ProcessMark{id: rand.Text()} }

// Env is the variable to start the agent process with.
func (m ProcessMark) Env() string { return EnvProcessMark + "=" + m.id }

// Watch returns rwc, the stdio of the agent process with the mark, with
// a Read that kills the processes with the mark once the output ends, as
// it does when the agent process exits, and a Close that kills them too.
func (m ProcessMark) Watch(rwc io.ReadWriteCloser) io.ReadWriteCloser {
	return &markedProcess{ReadWriteCloser: rwc, mark: m}
}

type markedProcess struct {
	io.ReadWriteCloser
	mark ProcessMark

	once sync.Once
	err  error
}

func (p *markedProcess) kill() error {
	p.once.Do(func() { p.err = p.mark.Kill() })
	return p.err
}

func (p *markedProcess) Read(b []byte) (int, error) {
	n, err := p.ReadWriteCloser.Read(b)
	if err != nil {
		if err := p.kill(); err != nil {
			slog.Warn("killing the processes the agent process started", "error", err)
		}
	}
	return n, err
}

func (p *markedProcess) Close() error {
	return errors.Join(p.ReadWriteCloser.Close(), p.kill())
}
