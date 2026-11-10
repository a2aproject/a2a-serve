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

package speech

import (
	"bytes"
	"encoding/binary"
	"time"
)

// Duration returns the length of Ogg (Opus or Vorbis) and WAV audio. It
// reports false for other formats and for data it can't read, which only
// the size limit bounds.
func Duration(a Audio) (time.Duration, bool) {
	switch {
	case bytes.HasPrefix(a.Data, []byte("OggS")):
		return oggDuration(a.Data)
	case len(a.Data) >= 12 && bytes.Equal(a.Data[:4], []byte("RIFF")) && bytes.Equal(a.Data[8:12], []byte("WAVE")):
		return wavDuration(a.Data)
	}
	return 0, false
}

// oggDuration reads the granule position of the last page, which counts
// samples at 48 kHz for Opus and at the stream's rate for Vorbis.
func oggDuration(data []byte) (time.Duration, bool) {
	const header = 27
	if len(data) < header+19 {
		return 0, false
	}
	rate := uint32(0)
	first := data[header+int(data[26]):]
	switch {
	case bytes.HasPrefix(first, []byte("OpusHead")):
		rate = 48000
	case len(first) >= 16 && bytes.HasPrefix(first, []byte("\x01vorbis")):
		rate = binary.LittleEndian.Uint32(first[12:16])
	}
	if rate == 0 {
		return 0, false
	}
	for i := bytes.LastIndex(data, []byte("OggS")); i >= 0; i = bytes.LastIndex(data[:i], []byte("OggS")) {
		if len(data)-i < header || data[i+4] != 0 {
			continue
		}
		granule := int64(binary.LittleEndian.Uint64(data[i+6 : i+14]))
		if granule < 0 {
			continue
		}
		return samples(uint64(granule), uint64(rate)), true
	}
	return 0, false
}

// wavDuration divides the size of the data chunk by the byte rate.
func wavDuration(data []byte) (time.Duration, bool) {
	byteRate := uint32(0)
	for off := 12; off+8 <= len(data); {
		id, size := string(data[off:off+4]), binary.LittleEndian.Uint32(data[off+4:off+8])
		body := off + 8
		switch id {
		case "fmt ":
			if body+12 > len(data) {
				return 0, false
			}
			byteRate = binary.LittleEndian.Uint32(data[body+8 : body+12])
		case "data":
			if byteRate == 0 {
				return 0, false
			}
			// Streamed WAV files may give the data chunk the largest size.
			n := min(uint64(size), uint64(len(data)-body))
			return samples(n, uint64(byteRate)), true
		}
		off = body + int(size) + int(size%2)
	}
	return 0, false
}

// samples returns the duration of n samples at rate, capped well before
// it overflows.
func samples(n, rate uint64) time.Duration {
	const maxSeconds = 1 << 32
	if n/rate >= maxSeconds {
		return maxSeconds * time.Second
	}
	return time.Duration(n/rate)*time.Second + time.Duration(n%rate)*time.Second/time.Duration(rate)
}
