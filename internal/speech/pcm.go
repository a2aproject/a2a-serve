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
	"errors"
	"mime"
	"strconv"
	"strings"
)

// pcm is mono 16-bit audio.
type pcm struct {
	samples []int16
	rate    int
}

// maxRate bounds the sample rate a service may claim.
const maxRate = 192000

// readPCM reads audio that is a WAV file of 16-bit PCM, or raw 16-bit
// little-endian PCM of the media type audio/L16 or audio/pcm, whose rate
// parameter defaults to 24 kHz. Stereo is mixed down to mono.
func readPCM(mediaType string, data []byte) (pcm, error) {
	if len(data) >= 12 && bytes.Equal(data[:4], []byte("RIFF")) && bytes.Equal(data[8:12], []byte("WAVE")) {
		return readWAV(data)
	}
	t, params, err := mime.ParseMediaType(mediaType)
	if err != nil || (t != "audio/l16" && t != "audio/pcm") {
		return pcm{}, errors.New("unknown audio format")
	}
	rate, channels := 24000, 1
	if v, ok := params["rate"]; ok {
		if rate, err = strconv.Atoi(v); err != nil {
			return pcm{}, errors.New("invalid sample rate")
		}
	}
	if v, ok := params["channels"]; ok {
		if channels, err = strconv.Atoi(v); err != nil {
			return pcm{}, errors.New("invalid channel count")
		}
	}
	return decodePCM(data, rate, channels)
}

// readWAV reads the samples of a WAV file of 16-bit PCM. A data chunk whose
// size claims more than the file holds, as streamed WAV files have, ends
// with the file.
func readWAV(data []byte) (pcm, error) {
	rate, channels := 0, 0
	for off := 12; off+8 <= len(data); {
		id, size := string(data[off:off+4]), int64(binary.LittleEndian.Uint32(data[off+4:]))
		body := data[off+8:]
		switch {
		case id == "data" && rate == 0:
			return pcm{}, errors.New("WAV data before its format")
		case id == "data":
			if size < int64(len(body)) {
				body = body[:size]
			}
			return decodePCM(body, rate, channels)
		case id == "fmt " && (size < 16 || size > int64(len(body))):
			return pcm{}, errors.New("invalid WAV format")
		case id == "fmt ":
			format := binary.LittleEndian.Uint16(body)
			if (format != 1 && format != 0xFFFE) || binary.LittleEndian.Uint16(body[14:]) != 16 {
				return pcm{}, errors.New("WAV audio isn't 16-bit PCM")
			}
			channels, rate = int(binary.LittleEndian.Uint16(body[2:])), int(binary.LittleEndian.Uint32(body[4:]))
			if rate == 0 {
				return pcm{}, errors.New("invalid sample rate")
			}
		}
		off += 8 + int(min(size+size%2, int64(len(body))))
	}
	return pcm{}, errors.New("WAV file without data")
}

// decodePCM reads 16-bit little-endian samples, and mixes stereo down to
// mono.
func decodePCM(data []byte, rate, channels int) (pcm, error) {
	if rate <= 0 || rate > maxRate {
		return pcm{}, errors.New("invalid sample rate")
	}
	if channels != 1 && channels != 2 {
		return pcm{}, errors.New("audio isn't mono or stereo")
	}
	frame := 2 * channels
	samples := make([]int16, len(data)/frame)
	for i := range samples {
		var sum int32
		for c := range channels {
			sum += int32(int16(binary.LittleEndian.Uint16(data[i*frame+2*c:])))
		}
		samples[i] = int16(sum / int32(channels))
	}
	return pcm{samples: samples, rate: rate}, nil
}

// wav returns p as a WAV file.
func (p pcm) wav() []byte {
	size := 2 * len(p.samples)
	data := make([]byte, 44, 44+size)
	copy(data, "RIFF")
	binary.LittleEndian.PutUint32(data[4:], uint32(36+size))
	copy(data[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(data[16:], 16)
	binary.LittleEndian.PutUint16(data[20:], 1)
	binary.LittleEndian.PutUint16(data[22:], 1)
	binary.LittleEndian.PutUint32(data[24:], uint32(p.rate))
	binary.LittleEndian.PutUint32(data[28:], uint32(2*p.rate))
	binary.LittleEndian.PutUint16(data[32:], 2)
	binary.LittleEndian.PutUint16(data[34:], 16)
	copy(data[36:], "data")
	binary.LittleEndian.PutUint32(data[40:], uint32(size))
	for _, s := range p.samples {
		data = binary.LittleEndian.AppendUint16(data, uint16(s))
	}
	return data
}

// resample returns p at rate, by linear interpolation.
func (p pcm) resample(rate int) pcm {
	if p.rate == rate || len(p.samples) == 0 {
		return p
	}
	n := int(int64(len(p.samples)) * int64(rate) / int64(p.rate))
	out := make([]int16, n)
	last := len(p.samples) - 1
	for i := range out {
		pos := int64(i) * int64(p.rate)
		j, frac := int(pos/int64(rate)), float64(pos%int64(rate))/float64(rate)
		s0, s1 := float64(p.samples[j]), float64(p.samples[min(j+1, last)])
		out[i] = int16(s0 + (s1-s0)*frac)
	}
	return pcm{samples: out, rate: rate}
}

// lowerMediaType returns the media type of v in lower case, without
// parameters.
func lowerMediaType(v string) string {
	t, _, err := mime.ParseMediaType(v)
	if err != nil {
		t = v
	}
	return strings.ToLower(strings.TrimSpace(t))
}
