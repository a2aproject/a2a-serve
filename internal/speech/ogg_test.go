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
	"io"
	"math"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/pion/opus"
	"github.com/pion/opus/pkg/oggreader"
)

// oggPage is the header of a page.
type oggPage struct {
	Flags    byte
	Granule  uint64
	Seq      uint32
	Packets  int
	Segments int
}

// readOgg returns the pages and audio packets of an Ogg Opus stream, and
// fails on a page with a wrong CRC.
func readOgg(t *testing.T, data []byte) ([]oggPage, [][]byte) {
	t.Helper()
	r, _, err := oggreader.NewWith(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("oggreader.NewWith() error = %v, want nil", err)
	}
	var packets [][]byte
	for {
		p, _, err := r.ParseNextPacket()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("ParseNextPacket() error = %v, want nil", err)
		}
		packets = append(packets, p)
	}
	if len(packets) == 0 || !bytes.HasPrefix(packets[0], []byte("OpusTags")) {
		t.Fatalf("oggreader packets don't start with OpusTags")
	}
	var pages []oggPage
	for off := 0; off < len(data); {
		segs := data[off+27 : off+27+int(data[off+26])]
		p := oggPage{Flags: data[off+5], Granule: binary.LittleEndian.Uint64(data[off+6:]), Seq: binary.LittleEndian.Uint32(data[off+18:]), Segments: len(segs)}
		size := 0
		for _, s := range segs {
			size += int(s)
			if s < 255 {
				p.Packets++
			}
		}
		pages = append(pages, p)
		off += 27 + len(segs) + size
	}
	return pages, packets[1:]
}

func TestOggOpus(t *testing.T) {
	t.Parallel()
	const rate, length = 24000, 2 * time.Second
	samples := make([]int16, int(length*rate/time.Second))
	for i := range samples {
		samples[i] = int16(8000 * math.Sin(2*math.Pi*440*float64(i)/rate))
	}
	data, err := oggOpus(t.Context(), pcm{samples: samples, rate: rate})
	if err != nil {
		t.Fatalf("oggOpus() error = %v, want nil", err)
	}
	if d, ok := Duration(Audio{Data: data}); !ok || d != 2020*time.Millisecond {
		t.Fatalf("Duration(oggOpus()) = %v, %v, want 2.02s, true", d, ok)
	}
	pages, packets := readOgg(t, data)
	end := uint64(opusPreSkip + 96000)
	want := []oggPage{
		{Flags: oggBOS, Seq: 0, Packets: 1, Segments: 1},
		{Seq: 1, Packets: 1, Segments: 1},
		{Granule: 50 * opusFrame, Seq: 2, Packets: 50, Segments: 50},
		{Granule: 100 * opusFrame, Seq: 3, Packets: 50, Segments: 50},
		{Flags: oggEOS, Granule: end, Seq: 4, Packets: 1, Segments: 1},
	}
	if diff := cmp.Diff(want, pages); diff != "" {
		t.Fatalf("oggOpus() wrong pages (-want +got) diff = %s", diff)
	}
	dec, err := opus.NewDecoderWithOutput(opusRate, 1)
	if err != nil {
		t.Fatal(err)
	}
	var decoded []int16
	for _, p := range packets {
		out := make([]int16, 5760)
		n, err := dec.DecodeToInt16(p, out)
		if err != nil {
			t.Fatalf("DecodeToInt16() error = %v, want nil", err)
		}
		decoded = append(decoded, out[:n]...)
	}
	var energy float64
	for _, s := range decoded[opusPreSkip:end] {
		energy += float64(s) * float64(s)
	}
	if rms := math.Sqrt(energy / float64(end-opusPreSkip)); rms < 3000 {
		t.Fatalf("decoded RMS = %.0f, want about 5600 of the tone", rms)
	}
}

func TestMuxOggLacesLargePackets(t *testing.T) {
	t.Parallel()
	var packets [][]byte
	for i := range 120 {
		packets = append(packets, bytes.Repeat([]byte{byte(i)}, 255+i))
	}
	data := muxOgg(packets, 120*opusFrame, opusRate)
	pages, got := readOgg(t, data)
	if diff := cmp.Diff(packets, got); diff != "" {
		t.Fatalf("muxOgg() wrong packets (-want +got) diff = %s", diff)
	}
	for _, p := range pages {
		if p.Segments > 255 {
			t.Fatalf("muxOgg() page %d has %d segments, want at most 255", p.Seq, p.Segments)
		}
	}
	if last := pages[len(pages)-1]; last.Flags != oggEOS || last.Granule != 120*opusFrame {
		t.Fatalf("muxOgg() last page = %+v, want EOS at granule %d", last, 120*opusFrame)
	}
}

func TestReadPCM(t *testing.T) {
	t.Parallel()
	mono := pcm{samples: []int16{1, -2, 300, -32768}, rate: 24000}
	stereo := []byte{2, 0, 4, 0, 0xfe, 0xff, 0xfa, 0xff}
	streamed := mono.wav()
	binary.LittleEndian.PutUint32(streamed[40:], 0xFFFFFFFF)
	tests := []struct {
		name      string
		mediaType string
		data      []byte
		want      pcm
		wantErr   bool
	}{
		{name: "wav", mediaType: "audio/wav", data: mono.wav(), want: mono},
		{name: "streamed wav", mediaType: "audio/wav", data: streamed, want: mono},
		{name: "raw pcm", mediaType: "audio/L16;codec=pcm;rate=24000", data: mono.wav()[44:], want: mono},
		{name: "raw pcm at the default rate", mediaType: "audio/pcm", data: mono.wav()[44:], want: mono},
		{name: "stereo", mediaType: "audio/L16; rate=16000; channels=2", data: stereo, want: pcm{samples: []int16{3, -4}, rate: 16000}},
		{name: "mp3", mediaType: "audio/mpeg", data: []byte("ID3"), wantErr: true},
		{name: "rate out of range", mediaType: "audio/L16;rate=0", data: stereo, wantErr: true},
		{name: "wav without format", mediaType: "audio/wav", data: append([]byte("RIFF\x00\x00\x00\x00WAVEdata\x02\x00\x00\x00"), 0, 0), wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := readPCM(tc.mediaType, tc.data)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("readPCM() error = nil, want an error")
				}
				return
			}
			if err != nil {
				t.Fatalf("readPCM() error = %v, want nil", err)
			}
			if diff := cmp.Diff(tc.want, got, cmp.AllowUnexported(pcm{})); diff != "" {
				t.Fatalf("readPCM() wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}

func FuzzReadPCM(f *testing.F) {
	f.Add((pcm{samples: []int16{1, 2, 3}, rate: 24000}).wav())
	f.Add([]byte("RIFF\x00\x00\x00\x00WAVEfmt \x10\x00\x00\x00"))
	f.Fuzz(func(t *testing.T, data []byte) {
		if _, err := readPCM("audio/wav", data); err != nil {
			return
		}
	})
}
