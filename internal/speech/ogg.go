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
	"context"
	"encoding/binary"
	"errors"
	"math/rand/v2"

	"github.com/pion/opus"
)

const (
	// opusRate is the rate the Opus encoder takes.
	opusRate = 48000
	// opusFrame is the samples of a 20 ms frame at opusRate, the one frame
	// size the encoder takes.
	opusFrame = opusRate / 50
	// opusPreSkip is the delay of the encoder in samples at opusRate,
	// which players skip at the start.
	opusPreSkip = 120
	// opusBitrate suits speech in mono.
	opusBitrate = 32000
	// pagePackets is the most packets of one Ogg page: a second of audio.
	pagePackets = 50
)

// oggOpus encodes p as Ogg Opus (RFC 7845).
func oggOpus(ctx context.Context, p pcm) ([]byte, error) {
	if len(p.samples) == 0 {
		return nil, errors.New("no audio")
	}
	inputRate := p.rate
	p = p.resample(opusRate)
	enc, err := opus.NewEncoder(opus.WithBitrate(opusBitrate))
	if err != nil {
		return nil, err
	}
	// The encoder delays the audio by opusPreSkip, so the last frame must
	// reach past the end by as much. Padding fills it with silence.
	end := opusPreSkip + len(p.samples)
	frames := (end + opusFrame - 1) / opusFrame
	frame := make([]byte, 2*opusFrame)
	buf := make([]byte, 1500)
	packets := make([][]byte, 0, frames)
	for f := range frames {
		if f%pagePackets == 0 && ctx.Err() != nil {
			return nil, ctx.Err()
		}
		clear(frame)
		for i, s := range p.samples[min(f*opusFrame, len(p.samples)):min((f+1)*opusFrame, len(p.samples))] {
			binary.LittleEndian.PutUint16(frame[2*i:], uint16(s))
		}
		n, err := enc.Encode(frame, buf)
		if err != nil {
			return nil, err
		}
		packets = append(packets, bytes.Clone(buf[:n]))
	}
	return muxOgg(packets, uint64(end), inputRate), nil
}

// Flags of an Ogg page header.
const (
	oggBOS = 0x02
	oggEOS = 0x04
)

// muxOgg returns an Ogg stream of a mono Opus stream with packets of 20 ms,
// whose last page has the granule position end.
func muxOgg(packets [][]byte, end uint64, inputRate int) []byte {
	w := oggWriter{serial: rand.Uint32()}
	head := make([]byte, 19)
	copy(head, "OpusHead")
	head[8], head[9] = 1, 1
	binary.LittleEndian.PutUint16(head[10:], opusPreSkip)
	binary.LittleEndian.PutUint32(head[12:], uint32(inputRate))
	w.page(oggBOS, 0, [][]byte{head})
	const vendor = "a2a-serve"
	tags := []byte("OpusTags")
	tags = binary.LittleEndian.AppendUint32(tags, uint32(len(vendor)))
	tags = append(tags, vendor...)
	tags = binary.LittleEndian.AppendUint32(tags, 0)
	w.page(0, 0, [][]byte{tags})
	start, segments := 0, 0
	for i, p := range packets {
		if n := len(p)/255 + 1; segments+n <= 255 && i-start < pagePackets {
			segments += n
			continue
		}
		w.page(0, min(uint64(i)*opusFrame, end), packets[start:i])
		start, segments = i, len(p)/255+1
	}
	w.page(oggEOS, end, packets[start:])
	return w.buf.Bytes()
}

// oggWriter writes the pages of one logical Ogg stream (RFC 3533).
type oggWriter struct {
	buf    bytes.Buffer
	serial uint32
	seq    uint32
}

// page writes a page of whole packets, which must fit in its 255 lacing
// values.
func (w *oggWriter) page(flags byte, granule uint64, packets [][]byte) {
	h := make([]byte, 27, 27+255)
	copy(h, "OggS")
	h[5] = flags
	binary.LittleEndian.PutUint64(h[6:], granule)
	binary.LittleEndian.PutUint32(h[14:], w.serial)
	binary.LittleEndian.PutUint32(h[18:], w.seq)
	w.seq++
	var body []byte
	for _, p := range packets {
		n := len(p)
		for ; n >= 255; n -= 255 {
			h = append(h, 255)
		}
		h = append(h, byte(n))
		body = append(body, p...)
	}
	h[26] = byte(len(h) - 27)
	crc := oggCRC(oggCRC(0, h), body)
	binary.LittleEndian.PutUint32(h[22:], crc)
	w.buf.Write(h)
	w.buf.Write(body)
}

// oggCRCTable is the table of the Ogg CRC-32: polynomial 0x04c11db7, not
// reflected.
var oggCRCTable = func() (t [256]uint32) {
	for i := range t {
		c := uint32(i) << 24
		for range 8 {
			if c&0x80000000 != 0 {
				c = c<<1 ^ 0x04c11db7
			} else {
				c <<= 1
			}
		}
		t[i] = c
	}
	return t
}()

func oggCRC(crc uint32, data []byte) uint32 {
	for _, b := range data {
		crc = crc<<8 ^ oggCRCTable[byte(crc>>24)^b]
	}
	return crc
}
