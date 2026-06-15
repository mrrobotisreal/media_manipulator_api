package services

import (
	"encoding/binary"
	"encoding/json"
	"testing"
)

func pcmFromSamples(samples []int16) []byte {
	b := make([]byte, len(samples)*2)
	for i, s := range samples {
		binary.LittleEndian.PutUint16(b[i*2:], uint16(s))
	}
	return b
}

func TestQuantizePeak(t *testing.T) {
	cases := map[int16]int8{0: 0, 32767: 127, -32768: -127, 16384: 64, -16384: -64}
	for in, want := range cases {
		if got := quantizePeak(in); got != want {
			t.Errorf("quantizePeak(%d) = %d, want %d", in, got, want)
		}
	}
}

func TestBuildPeaksJSON(t *testing.T) {
	// 320 samples = 2 full buckets (160 each).
	samples := make([]int16, 320)
	samples[10] = 32767  // max in bucket 0
	samples[20] = -32768 // min in bucket 0
	samples[200] = 16384 // max in bucket 1
	samples[210] = -8192 // min in bucket 1
	out := buildPeaksJSON(pcmFromSamples(samples))

	var p StudioPeaks
	if err := json.Unmarshal(out, &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if p.Version != 1 || p.BucketsPerSecond != 50 {
		t.Errorf("header: %+v", p)
	}
	if p.Length != 2 || len(p.Peaks) != 4 {
		t.Fatalf("expected 2 buckets / 4 values, got length=%d peaks=%v", p.Length, p.Peaks)
	}
	// [min0,max0,min1,max1]
	if p.Peaks[0] != -127 || p.Peaks[1] != 127 {
		t.Errorf("bucket 0 min/max = %d/%d, want -127/127", p.Peaks[0], p.Peaks[1])
	}
	if p.Peaks[2] != -32 || p.Peaks[3] != 64 {
		t.Errorf("bucket 1 min/max = %d/%d, want -32/64", p.Peaks[2], p.Peaks[3])
	}
}

func TestBuildPeaksJSON_Empty(t *testing.T) {
	var p StudioPeaks
	if err := json.Unmarshal(buildPeaksJSON(nil), &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if p.Length != 0 {
		t.Errorf("empty pcm → 0 buckets, got %d", p.Length)
	}
}

func TestPeaksMaxBytes(t *testing.T) {
	// known short duration → ~ (sec+2)*16000
	if got := peaksMaxBytes(10); got != int64((10+2)*studioPeaksSampleRate*2) {
		t.Errorf("peaksMaxBytes(10) = %d", got)
	}
	// unknown → hard ceiling (4h)
	if got := peaksMaxBytes(0); got != int64(studioPeaksSampleRate*2*4*3600) {
		t.Errorf("peaksMaxBytes(0) = %d", got)
	}
}
