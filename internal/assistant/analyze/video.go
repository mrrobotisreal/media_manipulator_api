package analyze

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
)

// VideoMetrics is the ffprobe summary, the luma of up to 8 evenly spaced
// thumbnails and the audio measurement (nil when the file has no audio).
type VideoMetrics struct {
	DurationSec   float64
	Width, Height int
	FPS           float64
	VideoCodec    string
	AudioCodec    string
	BitrateKbps   float64
	// SceneLuma is the mean luma (0..1) of each thumbnail.
	SceneLuma []float64
	// BlackShare is the share of thumbnails darker than 0.04 mean luma.
	BlackShare float64
	Audio      *AudioMetrics
}

const (
	thumbCount = 8
	thumbW     = 64
	thumbH     = 36
)

// AnalyzeVideo probes the file and samples up to 8 thumbnails.
func AnalyzeVideo(ctx context.Context, path string) (*VideoMetrics, error) {
	out, _, err := run(ctx, probeTimeout, "ffprobe", "-v", "error", "-print_format", "json", "-show_format", "-show_streams", path)
	if err != nil {
		return nil, err
	}
	v, err := parseProbe(out)
	if err != nil {
		return nil, err
	}
	rate := "1"
	if v.DurationSec > 0 {
		rate = strconv.FormatFloat(float64(thumbCount)/v.DurationSec, 'f', 6, 64)
	}
	raw, _, err := run(ctx, analyzeTimeout, "ffmpeg", "-nostdin", "-nostats", "-hide_banner", "-v", "error", "-i", path, "-an",
		"-vf", "fps="+rate+",scale="+strconv.Itoa(thumbW)+":"+strconv.Itoa(thumbH)+":flags=area,format=gray",
		"-frames:v", strconv.Itoa(thumbCount), "-f", "rawvideo", "-")
	if err != nil {
		return nil, err
	}
	v.SceneLuma = frameLumas(raw, thumbW*thumbH)
	black := 0
	for _, l := range v.SceneLuma {
		if l < 0.04 {
			black++
		}
	}
	if len(v.SceneLuma) > 0 {
		v.BlackShare = round(float64(black)/float64(len(v.SceneLuma)), 3)
	}
	if v.AudioCodec != "" {
		if a, aerr := AnalyzeAudio(ctx, path); aerr == nil {
			v.Audio = a
		}
	}
	return v, nil
}

func frameLumas(raw []byte, frameSize int) []float64 {
	var out []float64
	for off := 0; off+frameSize <= len(raw); off += frameSize {
		var s float64
		for _, b := range raw[off : off+frameSize] {
			s += float64(b)
		}
		out = append(out, round(s/float64(frameSize)/255, 4))
	}
	return out
}

func parseProbe(raw []byte) (*VideoMetrics, error) {
	var p struct {
		Streams []struct {
			CodecType  string `json:"codec_type"`
			CodecName  string `json:"codec_name"`
			Width      int    `json:"width"`
			Height     int    `json:"height"`
			RFrameRate string `json:"avg_frame_rate"`
			Duration   string `json:"duration"`
		} `json:"streams"`
		Format struct {
			Duration string `json:"duration"`
			BitRate  string `json:"bit_rate"`
		} `json:"format"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, err
	}
	v := &VideoMetrics{}
	v.DurationSec, _ = strconv.ParseFloat(p.Format.Duration, 64)
	if br, err := strconv.ParseFloat(p.Format.BitRate, 64); err == nil {
		v.BitrateKbps = round(br/1000, 0)
	}
	seenVideo := false
	for _, s := range p.Streams {
		switch s.CodecType {
		case "video":
			if seenVideo {
				continue
			}
			seenVideo = true
			v.VideoCodec, v.Width, v.Height = s.CodecName, s.Width, s.Height
			if n, d, ok := strings.Cut(s.RFrameRate, "/"); ok {
				nf, _ := strconv.ParseFloat(n, 64)
				df, _ := strconv.ParseFloat(d, 64)
				if df > 0 {
					v.FPS = round(nf/df, 2)
				}
			}
			if v.DurationSec == 0 {
				v.DurationSec, _ = strconv.ParseFloat(s.Duration, 64)
			}
		case "audio":
			if v.AudioCodec == "" {
				v.AudioCodec = s.CodecName
			}
		}
	}
	if !seenVideo {
		return nil, errors.New("analyze: no video stream")
	}
	return v, nil
}

// Set flattens to MetricSet names (audio figures included when present).
func (m *VideoMetrics) Set() MetricSet {
	s := MetricSet{"durationSec": round(m.DurationSec, 2), "width": float64(m.Width), "height": float64(m.Height),
		"fps": m.FPS, "bitrateKbps": m.BitrateKbps, "blackShare": m.BlackShare}
	if len(m.SceneLuma) > 0 {
		var sum, lo, hi float64 = 0, 1, 0
		for _, l := range m.SceneLuma {
			sum += l
			lo, hi = min(lo, l), max(hi, l)
		}
		s["meanLuma"], s["minSceneLuma"], s["maxSceneLuma"] = round(sum/float64(len(m.SceneLuma)), 4), lo, hi
	}
	if m.Audio != nil {
		for k, v := range m.Audio.Set() {
			s[k] = v
		}
	}
	return s
}
