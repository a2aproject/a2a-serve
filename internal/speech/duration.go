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

// Duration returns the length of Ogg Opus, MP3 and WAV audio, as a decoder
// plays it: from the Opus packets and MP3 frames themselves, which a sender
// can't misstate the way it can a header field. It reports false for other
// formats and for data it can't read.
func Duration(a Audio) (time.Duration, bool) {
	switch d := a.Data; {
	case bytes.HasPrefix(d, []byte("OggS")):
		return oggOpusDuration(d)
	case len(d) >= 12 && bytes.Equal(d[:4], []byte("RIFF")) && bytes.Equal(d[8:12], []byte("WAVE")):
		return wavDuration(d)
	case bytes.HasPrefix(d, []byte("ID3")) || (len(d) >= 2 && d[0] == 0xFF && d[1]&0xE0 == 0xE0):
		return mp3Duration(d)
	}
	return 0, false
}

// oggOpusDuration adds up the frames of the Opus packets of an Ogg stream.
func oggOpusDuration(data []byte) (time.Duration, bool) {
	const header = 27
	var total uint64
	var packet []byte
	index := 0
	end := func() bool {
		defer func() { packet, index = packet[:0], index+1 }()
		switch index {
		case 0:
			return bytes.HasPrefix(packet, []byte("OpusHead"))
		case 1:
			return true
		}
		n, ok := opusSamples(packet)
		total += n
		return ok
	}
	for off := 0; off < len(data); {
		if len(data)-off < header || !bytes.Equal(data[off:off+4], []byte("OggS")) || data[off+4] != 0 {
			return 0, false
		}
		segments := int(data[off+26])
		body := off + header + segments
		if body > len(data) {
			return 0, false
		}
		for _, l := range data[off+header : off+header+segments] {
			if body+int(l) > len(data) {
				return 0, false
			}
			// Only the start of a packet tells its length.
			if keep := 8 - len(packet); keep > 0 {
				packet = append(packet, data[body:body+min(keep, int(l))]...)
			}
			body += int(l)
			if l < 255 && !end() {
				return 0, false
			}
		}
		off = body
	}
	if index < 2 {
		return 0, false
	}
	return samples(total, 48000), true
}

// opusSamples returns the samples at 48 kHz of an Opus packet, from its
// table-of-contents byte (RFC 6716, section 3.1).
func opusSamples(packet []byte) (uint64, bool) {
	if len(packet) == 0 {
		return 0, true
	}
	toc := packet[0]
	config := toc >> 3
	var frame uint64
	switch {
	case config < 12:
		frame = []uint64{480, 960, 1920, 2880}[config%4]
	case config < 16:
		frame = []uint64{480, 960}[config%2]
	default:
		frame = []uint64{120, 240, 480, 960}[config%4]
	}
	switch toc & 3 {
	case 0:
		return frame, true
	case 1, 2:
		return 2 * frame, true
	}
	if len(packet) < 2 {
		return 0, false
	}
	return uint64(packet[1]&0x3F) * frame, true
}

// mp3Duration adds up the durations of the MPEG audio frames, which must
// make up all of the data but an ID3v2 tag at the start, zero padding, and
// an ID3v1 or APE tag or a cut frame at the end. Other data could hold
// audio the server doesn't count.
func mp3Duration(data []byte) (time.Duration, bool) {
	off := 0
	if len(data) >= 10 && bytes.HasPrefix(data, []byte("ID3")) {
		off = 10 + (int(data[6]&0x7F)<<21 | int(data[7]&0x7F)<<14 | int(data[8]&0x7F)<<7 | int(data[9]&0x7F))
	}
	for off < len(data) && data[off] == 0 {
		off++
	}
	var total time.Duration
	frames := 0
	for off < len(data) {
		rest := data[off:]
		if len(rest) < 4 {
			break
		}
		length, d, ok := mp3Frame(rest[:4])
		if !ok {
			if bytes.HasPrefix(rest, []byte("TAG")) && len(rest) == 128 || bytes.HasPrefix(rest, []byte("APETAGEX")) {
				break
			}
			return 0, false
		}
		if length > len(rest) {
			break
		}
		total += d
		frames++
		off += length
	}
	return total, frames > 0
}

var (
	// mp3Bitrates are the bitrates in kbit/s of MPEG-1 and of MPEG-2 and
	// 2.5, for layers I, II and III, by bitrate index.
	mp3Bitrates = [2][3][15]int{
		{
			{0, 32, 64, 96, 128, 160, 192, 224, 256, 288, 320, 352, 384, 416, 448},
			{0, 32, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320, 384},
			{0, 32, 40, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320},
		},
		{
			{0, 32, 48, 56, 64, 80, 96, 112, 128, 144, 160, 176, 192, 224, 256},
			{0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160},
			{0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160},
		},
	}
	// mp3Rates are the sample rates of MPEG-1 (3), 2 (2) and 2.5 (0), by
	// the version bits.
	mp3Rates = map[byte][3]int{3: {44100, 48000, 32000}, 2: {22050, 24000, 16000}, 0: {11025, 12000, 8000}}
)

// mp3Frame reads a frame header, and returns the frame's length in bytes
// and its duration.
func mp3Frame(h []byte) (int, time.Duration, bool) {
	if h[0] != 0xFF || h[1]&0xE0 != 0xE0 {
		return 0, 0, false
	}
	version, layerBits := (h[1]>>3)&3, (h[1]>>1)&3
	rateIndex, bitrateIndex, padding := (h[2]>>2)&3, int(h[2]>>4), int(h[2]>>1)&1
	rates, ok := mp3Rates[version]
	if !ok || layerBits == 0 || rateIndex == 3 || bitrateIndex == 0 || bitrateIndex == 15 {
		return 0, 0, false
	}
	layer := 3 - int(layerBits) // 0 is layer I, 2 is layer III.
	v := 0
	if version != 3 {
		v = 1
	}
	bitrate, rate := mp3Bitrates[v][layer][bitrateIndex]*1000, rates[rateIndex]
	frameSamples := 1152
	switch {
	case layer == 0:
		frameSamples = 384
	case layer == 2 && v == 1:
		frameSamples = 576
	}
	length := frameSamples/8*bitrate/rate + padding
	if layer == 0 {
		length = (12*bitrate/rate + padding) * 4
	}
	if length < 4 {
		return 0, 0, false
	}
	return length, samples(uint64(frameSamples), uint64(rate)), true
}

// wavFormats are the WAV sample formats whose byte rate follows from the
// sample rate and block size: PCM, float, A-law and mu-law.
var wavFormats = map[uint16]bool{1: true, 3: true, 6: true, 7: true}

// wavDuration divides the size of the data chunk by the byte rate that the
// sample rate and block size give, which is what decoders play.
func wavDuration(data []byte) (time.Duration, bool) {
	byteRate := uint64(0)
	for off := 12; off+8 <= len(data); {
		id, size := string(data[off:off+4]), binary.LittleEndian.Uint32(data[off+4:off+8])
		body := off + 8
		switch id {
		case "fmt ":
			if body+16 > len(data) {
				return 0, false
			}
			format := binary.LittleEndian.Uint16(data[body:])
			// WAVE_FORMAT_EXTENSIBLE names the format in its subformat.
			if format == 0xFFFE {
				if body+26 > len(data) {
					return 0, false
				}
				format = binary.LittleEndian.Uint16(data[body+24:])
			}
			rate, align := binary.LittleEndian.Uint32(data[body+4:]), binary.LittleEndian.Uint16(data[body+12:])
			if !wavFormats[format] || rate < 1000 || rate > 384000 || align == 0 || align > 64 {
				return 0, false
			}
			byteRate = uint64(rate) * uint64(align)
		case "data":
			if byteRate == 0 {
				return 0, false
			}
			// Streamed WAV files may give the data chunk the largest size.
			n := min(uint64(size), uint64(len(data)-body))
			return samples(n, byteRate), true
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
