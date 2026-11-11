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

package speech_test

import (
	"encoding/binary"
	"testing"
	"time"

	"github.com/a2aproject/a2a-serve/internal/speech"
)

const mp3Frame = 1152 * time.Second / 44100

// wavWithDataSize returns wav(d) whose data chunk claims size.
func wavWithDataSize(d time.Duration, size uint32) []byte {
	data := wav(d)
	binary.LittleEndian.PutUint32(data[40:], size)
	return data
}

// wavChunk returns a RIFF chunk of id holding body.
func wavChunk(id string, body []byte) []byte {
	c := binary.LittleEndian.AppendUint32([]byte(id), uint32(len(body)))
	c = append(c, body...)
	if len(body)%2 == 1 {
		c = append(c, 0)
	}
	return c
}

// apeTag returns an APEv2 tag of items, with a header if header is set.
func apeTag(items []byte, header bool) []byte {
	part := func(isHeader bool) []byte {
		p := make([]byte, 32)
		copy(p, "APETAGEX")
		binary.LittleEndian.PutUint32(p[8:], 2000)
		binary.LittleEndian.PutUint32(p[12:], uint32(len(items)+32))
		binary.LittleEndian.PutUint32(p[16:], 1)
		var flags uint32
		if header {
			flags |= 1 << 31
		}
		if isHeader {
			flags |= 1 << 29
		}
		binary.LittleEndian.PutUint32(p[20:], flags)
		return p
	}
	var tag []byte
	if header {
		tag = part(true)
	}
	tag = append(tag, items...)
	return append(tag, part(false)...)
}

var apeItems = append(binary.LittleEndian.AppendUint32(make([]byte, 0, 13), 3), "\x00\x00\x00\x00Title\x00abc"...)

// oggPage returns an Ogg page of stream serial holding whole packets.
func oggPage(serial uint32, packets ...[]byte) []byte {
	p := make([]byte, 27)
	copy(p, "OggS")
	binary.LittleEndian.PutUint32(p[14:], serial)
	p[26] = byte(len(packets))
	for _, pk := range packets {
		p = append(p, byte(len(pk)))
	}
	for _, pk := range packets {
		p = append(p, pk...)
	}
	return p
}

// oggOpusStream returns the pages of an Opus stream of serial holding d in
// 20 ms packets.
func oggOpusStream(serial uint32, d time.Duration) [][]byte {
	pages := [][]byte{
		oggPage(serial, []byte("OpusHead\x01\x01\x38\x01\x80\xbb\x00\x00\x00\x00\x00")),
		oggPage(serial, []byte("OpusTags")),
	}
	var audio [][]byte
	for range d / (20 * time.Millisecond) {
		audio = append(audio, []byte{0x08})
	}
	for len(audio) > 0 {
		n := min(len(audio), 255)
		pages = append(pages, oggPage(serial, audio[:n]...))
		audio = audio[n:]
	}
	return pages
}

func concat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// oggOpusWithOtherStream returns a short Opus stream whose pages are
// interleaved with those of a long stream of another serial, whose packets
// read as Opus packets of no frames.
func oggOpusWithOtherStream() []byte {
	opus := oggOpusStream(1, time.Second)
	other := [][]byte{oggPage(2, []byte("\x01vorbis"))}
	for range 100 {
		packets := make([][]byte, 255)
		for i := range packets {
			packets[i] = []byte{0x03, 0x00}
		}
		other = append(other, oggPage(2, packets...))
	}
	var data []byte
	for i := range max(len(opus), len(other)) {
		if i < len(opus) {
			data = append(data, opus[i]...)
		}
		if i < len(other) {
			data = append(data, other[i]...)
		}
	}
	return data
}

func TestDurationOfCraftedAudio(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		data   []byte
		want   time.Duration
		wantOK bool
	}{
		{name: "wav whose data chunk claims no size", data: wavWithDataSize(6*time.Minute, 0), want: 6 * time.Minute, wantOK: true},
		{name: "streamed wav", data: wavWithDataSize(6*time.Minute, 0xFFFFFFFF), want: 6 * time.Minute, wantOK: true},
		{name: "wav with a second data chunk", data: append(wav(time.Second), wavChunk("data", make([]byte, 6*60*8000))...), want: 6*time.Minute + time.Second, wantOK: true},
		{name: "wav with a data chunk after a list chunk", data: concat(wav(time.Second), wavChunk("LIST", []byte("INFOabc")), wavChunk("data", make([]byte, 8000))), want: 2 * time.Second, wantOK: true},
		{name: "wav with a second fmt chunk", data: concat(wav(time.Second), wavChunk("fmt ", wav(0)[20:36]), wavChunk("data", make([]byte, 8000)))},
		{name: "mp3 with an ape tag", data: append(mp3(10), apeTag(apeItems, true)...), want: 10 * mp3Frame, wantOK: true},
		{name: "mp3 with an ape tag without a header", data: append(mp3(10), apeTag(nil, false)...), want: 10 * mp3Frame, wantOK: true},
		{name: "mp3 with an ape tag and an id3v1 tag", data: concat(mp3(10), apeTag(apeItems, true), []byte("TAG"), make([]byte, 125)), want: 10 * mp3Frame, wantOK: true},
		{name: "mp3 with frames after an ape tag", data: concat(mp3(10), apeTag(apeItems, true), mp3(1000)[12:])},
		{name: "mp3 with frames inside an ape tag", data: concat(mp3(10), apeTag(mp3(1000)[12:], true))},
		{name: "mp3 with an ape tag that claims less than follows", data: concat(mp3(10), apeTag(apeItems, true), make([]byte, 10))},
		{name: "mp3 with an id3v1 tag before frames", data: concat(mp3(10), []byte("TAG"), make([]byte, 125), mp3(1000)[12:])},
		{name: "ogg opus of another serial", data: concat(oggOpusStream(7, 2*time.Second)...), want: 2 * time.Second, wantOK: true},
		{name: "ogg opus with another stream", data: oggOpusWithOtherStream()},
		{name: "ogg opus followed by another stream", data: concat(append(oggOpusStream(1, time.Second), oggPage(2, []byte{0x03, 0x00}))...)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := speech.Duration(speech.Audio{Data: tc.data})
			if got != tc.want || ok != tc.wantOK {
				t.Fatalf("Duration() = %v, %v, want %v, %v", got, ok, tc.want, tc.wantOK)
			}
		})
	}
}
