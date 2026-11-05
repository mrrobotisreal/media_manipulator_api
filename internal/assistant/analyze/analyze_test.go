package analyze

import (
	"bytes"
	"context"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Fixtures are generated in the tests (pure Go for images and tones; ffmpeg
// for the black-frame video, skipped when the binary is missing), so nothing
// binary is committed.

func pngBytes(t *testing.T, img image.Image) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// clippedImage is 64x64: the left half pure black, the right 30 % pure white,
// the rest mid grey.
func clippedImage() image.Image {
	img := image.NewRGBA(image.Rect(0, 0, 64, 64))
	for y := 0; y < 64; y++ {
		for x := 0; x < 64; x++ {
			c := color.RGBA{128, 128, 128, 255}
			switch {
			case x < 16:
				c = color.RGBA{0, 0, 0, 255}
			case x >= 64-16:
				c = color.RGBA{255, 255, 255, 255}
			}
			img.Set(x, y, c)
		}
	}
	return img
}

func TestAnalyzeImageClipped(t *testing.T) {
	m, err := AnalyzeImage(bytes.NewReader(pngBytes(t, clippedImage())))
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(m.ShadowsClipped-0.25) > 0.01 || math.Abs(m.HighlightsClipped-0.25) > 0.01 {
		t.Fatalf("clip shadows %v highlights %v, want 0.25 each", m.ShadowsClipped, m.HighlightsClipped)
	}
	if math.Abs(m.MeanLuma-0.5) > 0.03 {
		t.Fatalf("mean luma %v", m.MeanLuma)
	}
	var sum float64
	for _, v := range m.Luma32 {
		sum += v
	}
	if math.Abs(sum-1) > 0.01 || m.Luma32[0] < 0.2 || m.Luma32[31] < 0.2 {
		t.Fatalf("histogram off: sum %v first %v last %v", sum, m.Luma32[0], m.Luma32[31])
	}
	if math.Abs(m.Warmth) > 0.001 || math.Abs(m.Tint) > 0.001 {
		t.Fatalf("neutral image has warmth %v tint %v", m.Warmth, m.Tint)
	}
	if len(m.DominantHues) != 0 {
		t.Fatalf("grey image has hues %v", m.DominantHues)
	}
	s := m.Set()
	if s["shadowsClipped"] != m.ShadowsClipped || s.Validate() != nil {
		t.Fatalf("metric set: %v / %v", s, s.Validate())
	}
}

func TestAnalyzeImageWarmAndHue(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 32, 32))
	for y := 0; y < 32; y++ {
		for x := 0; x < 32; x++ {
			img.Set(x, y, color.RGBA{200, 120, 60, 255})
		}
	}
	m, err := AnalyzeImage(bytes.NewReader(pngBytes(t, img)))
	if err != nil {
		t.Fatal(err)
	}
	if m.Warmth < 0.3 {
		t.Fatalf("orange image warmth %v", m.Warmth)
	}
	if len(m.DominantHues) == 0 || m.DominantHues[0].Hue < 15 || m.DominantHues[0].Hue > 35 || m.DominantHues[0].Share < 0.9 {
		t.Fatalf("dominant hues %v", m.DominantHues)
	}
}

func TestSharpnessAndNoise(t *testing.T) {
	flat := image.NewGray(image.Rect(0, 0, 64, 64))
	checker := image.NewGray(image.Rect(0, 0, 64, 64))
	for y := 0; y < 64; y++ {
		for x := 0; x < 64; x++ {
			flat.SetGray(x, y, color.Gray{100})
			if (x+y)%2 == 0 {
				checker.SetGray(x, y, color.Gray{255})
			}
		}
	}
	f, _ := AnalyzeImage(bytes.NewReader(pngBytes(t, flat)))
	c, _ := AnalyzeImage(bytes.NewReader(pngBytes(t, checker)))
	if f.Sharpness != 0 || f.NoiseSigma != 0 {
		t.Fatalf("flat sharpness %v noise %v", f.Sharpness, f.NoiseSigma)
	}
	if c.Sharpness < 1000 {
		t.Fatalf("checkerboard sharpness %v", c.Sharpness)
	}
}

func TestAnalyzeImageRejectsGarbage(t *testing.T) {
	if _, err := AnalyzeImage(strings.NewReader("not an image")); err == nil {
		t.Fatal("garbage decoded")
	}
}

// writeTone writes a mono 16-bit WAV of a 997 Hz sine whose loudness is lufs
// (a sine's integrated loudness is its peak dBFS − 3.01).
func writeTone(t *testing.T, path string, lufs float64, seconds int) {
	t.Helper()
	const rate = 8000
	peak := math.Pow(10, (lufs+3.01)/20)
	n := rate * seconds
	data := make([]byte, 0, n*2)
	for i := 0; i < n; i++ {
		v := peak * math.Sin(2*math.Pi*997*float64(i)/rate)
		s := int16(math.Round(math.Max(-1, math.Min(1, v)) * 32767))
		data = binary.LittleEndian.AppendUint16(data, uint16(s))
	}
	var b bytes.Buffer
	b.WriteString("RIFF")
	binary.Write(&b, binary.LittleEndian, uint32(36+len(data)))
	b.WriteString("WAVEfmt ")
	for _, v := range []any{uint32(16), uint16(1), uint16(1), uint32(rate), uint32(rate * 2), uint16(2), uint16(16)} {
		binary.Write(&b, binary.LittleEndian, v)
	}
	b.WriteString("data")
	binary.Write(&b, binary.LittleEndian, uint32(len(data)))
	b.Write(data)
	if err := os.WriteFile(path, b.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

func needFFmpeg(t *testing.T) {
	t.Helper()
	if !HasFFmpeg() {
		t.Skip("ffmpeg/ffprobe not installed")
	}
}

func TestAnalyzeAudioMinus23LUFS(t *testing.T) {
	needFFmpeg(t)
	p := filepath.Join(t.TempDir(), "tone.wav")
	writeTone(t, p, -23, 3)
	m, err := AnalyzeAudio(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(m.IntegratedLUFS-(-23)) > 0.6 {
		t.Fatalf("integrated %v LUFS, want -23", m.IntegratedLUFS)
	}
	if m.TruePeakDBFS > -18 || m.TruePeakDBFS < -22 || m.ClippedShare != 0 || m.Silent {
		t.Fatalf("true peak %v clipped %v silent %v", m.TruePeakDBFS, m.ClippedShare, m.Silent)
	}
	// LRA of a short synthetic tone is dominated by ffmpeg's start-up windows,
	// so only its presence is checked here (the parser test pins the reading).
	if m.LRA < 0 {
		t.Fatalf("LRA %v", m.LRA)
	}
}

func TestAnalyzeAudioClipping(t *testing.T) {
	needFFmpeg(t)
	p := filepath.Join(t.TempDir(), "hot.wav")
	writeTone(t, p, -1.5, 2) // peak above full scale: clipped hard
	m, err := AnalyzeAudio(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if m.PeakDB < -0.1 || m.ClippedShare < 0.05 {
		t.Fatalf("clipped tone: peak %v clipped share %v", m.PeakDB, m.ClippedShare)
	}
}

func TestParseAudioReport(t *testing.T) {
	report := `[Parsed_ebur128_0 @ 0x1] t: 2.0  TARGET:-23 LUFS    M: -22.9 S: -22.8     I: -23.0 LUFS       LRA:   0.0 LU
[Parsed_ebur128_0 @ 0x1] Summary:

  Integrated loudness:
    I:         -23.0 LUFS
    Threshold: -33.0 LUFS

  Loudness range:
    LRA:         3.5 LU

  True peak:
    Peak:      -1.2 dBFS
[Parsed_astats_1 @ 0x2] Overall
[Parsed_astats_1 @ 0x2] Peak level dB: -1.300000
[Parsed_astats_1 @ 0x2] RMS level dB: -20.500000
[Parsed_astats_1 @ 0x2] RMS trough dB: -55.000000
[Parsed_astats_1 @ 0x2] Peak count: 0.000000
[Parsed_astats_1 @ 0x2] Number of samples: 96000
`
	m, err := parseAudio(report)
	if err != nil {
		t.Fatal(err)
	}
	if m.IntegratedLUFS != -23 || m.LRA != 3.5 || m.TruePeakDBFS != -1.2 || m.PeakDB != -1.3 || m.NoiseFloorDB != -55 || m.ShortTermMaxLUFS != -22.8 {
		t.Fatalf("parsed %+v", m)
	}
	if _, err := parseAudio("nothing here"); err == nil {
		t.Fatal("no loudness should fail")
	}
}

func TestAnalyzeVideoBlackFrames(t *testing.T) {
	needFFmpeg(t)
	p := filepath.Join(t.TempDir(), "black.mp4")
	if out, err := exec.Command("ffmpeg", "-v", "error", "-y", "-f", "lavfi", "-i", "color=c=black:s=128x72:r=10:d=2",
		"-f", "lavfi", "-i", "sine=f=440:d=2:sample_rate=8000", "-shortest", "-c:v", "mpeg4", "-q:v", "10", "-c:a", "aac", "-b:a", "16k", p).CombinedOutput(); err != nil {
		t.Skipf("cannot build the fixture: %v %s", err, out)
	}
	if st, _ := os.Stat(p); st.Size() > 50<<10 {
		t.Fatalf("fixture is %d bytes", st.Size())
	}
	m, err := AnalyzeVideo(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if m.Width != 128 || m.Height != 72 || m.DurationSec < 1.5 || m.DurationSec > 2.6 || m.AudioCodec == "" {
		t.Fatalf("probe %+v", m)
	}
	if len(m.SceneLuma) == 0 || m.BlackShare != 1 {
		t.Fatalf("black video: scene luma %v black share %v", m.SceneLuma, m.BlackShare)
	}
	if m.Audio == nil {
		t.Fatal("audio was not measured")
	}
	if s := m.Set(); s["blackShare"] != 1 || s["meanLuma"] > 0.04 {
		t.Fatalf("set %v", s)
	}
}

func TestParseProbe(t *testing.T) {
	m, err := parseProbe([]byte(`{"streams":[{"codec_type":"video","codec_name":"h264","width":1920,"height":1080,"avg_frame_rate":"30000/1001"},{"codec_type":"audio","codec_name":"aac"}],"format":{"duration":"12.5","bit_rate":"2500000"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if m.Width != 1920 || m.FPS != 29.97 || m.DurationSec != 12.5 || m.BitrateKbps != 2500 || m.AudioCodec != "aac" {
		t.Fatalf("%+v", m)
	}
	if _, err := parseProbe([]byte(`{"streams":[{"codec_type":"audio"}]}`)); err == nil {
		t.Fatal("audio-only is not a video")
	}
}

func TestCompareFilesPSNRSSIM(t *testing.T) {
	needFFmpeg(t)
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a.png"), filepath.Join(dir, "b.png")
	if err := os.WriteFile(a, pngBytes(t, clippedImage()), 0o600); err != nil {
		t.Fatal(err)
	}
	dim := image.NewRGBA(image.Rect(0, 0, 64, 64))
	for y := 0; y < 64; y++ {
		for x := 0; x < 64; x++ {
			dim.Set(x, y, color.RGBA{60, 60, 60, 255})
		}
	}
	if err := os.WriteFile(b, pngBytes(t, dim), 0o600); err != nil {
		t.Fatal(err)
	}
	same, err := CompareFiles(context.Background(), a, a)
	if err != nil {
		t.Fatal(err)
	}
	if same.SSIM < 0.999 || same.PSNR < 90 {
		t.Fatalf("identical: %+v", same)
	}
	diff, err := CompareFiles(context.Background(), a, b)
	if err != nil {
		t.Fatal(err)
	}
	if diff.SSIM > 0.9 || diff.PSNR > 30 {
		t.Fatalf("different: %+v", diff)
	}
}

func TestCompareMetricSets(t *testing.T) {
	before := MetricSet{"meanLuma": 0.2, "shadowsClipped": 0.08, "warmth": 0.1, "onlyBefore": 1}
	after := MetricSet{"meanLuma": 0.42, "shadowsClipped": 0.004, "warmth": 0.1, "onlyAfter": 2}
	deltas, only := Compare(before, after)
	if len(deltas) != 3 || len(only) != 2 {
		t.Fatalf("deltas %v only %v", deltas, only)
	}
	text := FormatCompare(before, after)
	for _, want := range []string{"meanLuma: 0.2 → 0.42 (+0.22)", "shadowsClipped: 0.08 → 0.004 (-0.076)", "Measured only once: onlyBefore, onlyAfter"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in:\n%s", want, text)
		}
	}
	if b := Brief(before, after, 1); !strings.HasPrefix(b, "meanLuma") {
		t.Fatalf("brief %q", b)
	}
	if !strings.Contains(FormatCompare(MetricSet{"a": 1}, MetricSet{"b": 1}), "No metrics in common") {
		t.Fatal("disjoint sets")
	}
}

func TestMetricSetValidate(t *testing.T) {
	if err := (MetricSet{"meanLuma": 0.4}).Validate(); err != nil {
		t.Fatal(err)
	}
	bad := []MetricSet{
		{"bad name": 1}, {"1x": 1}, {"x": math.NaN()}, {"x": math.Inf(1)}, {"x": 1e9},
	}
	for _, m := range bad {
		if m.Validate() == nil {
			t.Errorf("%v should be invalid", m)
		}
	}
	big := MetricSet{}
	for i := 0; i < 49; i++ {
		big["m"+strings.Repeat("x", 3)+string(rune('a'+i%26))+string(rune('a'+i/26))] = 1
	}
	if big.Validate() == nil {
		t.Fatal("49 metrics accepted")
	}
}
