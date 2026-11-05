package analyze

import (
	"context"
	"errors"
	"regexp"
	"strconv"
	"strings"
)

// AudioMetrics is the loudness and level measurement of one audio stream.
type AudioMetrics struct {
	// IntegratedLUFS is the EBU R128 programme loudness; ShortTermMaxLUFS the
	// loudest 3 s window; LRA the loudness range (LU); TruePeakDBFS the
	// inter-sample peak.
	IntegratedLUFS, ShortTermMaxLUFS, LRA, TruePeakDBFS float64
	// PeakDB/RMSDB are sample-domain levels; NoiseFloorDB is the quietest
	// sustained level (RMS trough, dB); ClippedShare is the share of samples
	// at full scale (0 when the peak stays below -0.1 dBFS).
	PeakDB, RMSDB, NoiseFloorDB, ClippedShare float64
	Silent                                    bool
}

var (
	reIntegrated = regexp.MustCompile(`(?m)^\s*I:\s+(-?[0-9.]+|-inf)\s+LUFS`)
	reLRA        = regexp.MustCompile(`(?m)^\s*LRA:\s+(-?[0-9.]+)\s+LU`)
	reTruePeak   = regexp.MustCompile(`(?m)^\s*Peak:\s+(-?[0-9.]+|-inf)\s+dBFS`)
	reShortTerm  = regexp.MustCompile(`\bS:\s*(-?[0-9.]+)\s`)
)

// AnalyzeAudio measures the first audio stream of a file with ebur128 and
// astats (one ffmpeg pass).
func AnalyzeAudio(ctx context.Context, path string) (*AudioMetrics, error) {
	_, stderr, err := run(ctx, analyzeTimeout, "ffmpeg", "-nostdin", "-nostats", "-hide_banner", "-i", path, "-vn", "-map", "0:a:0",
		"-af", "ebur128=peak=true,astats=measure_perchannel=none", "-f", "null", "-")
	if err != nil {
		return nil, err
	}
	return parseAudio(string(stderr))
}

func parseAudio(report string) (*AudioMetrics, error) {
	m := &AudioMetrics{}
	sum := report
	if i := strings.LastIndex(report, "Summary:"); i >= 0 {
		sum = report[i:]
	}
	i, ok := firstFloat(reIntegrated, sum)
	if !ok {
		return nil, errors.New("analyze: no loudness in the ffmpeg report (no audio?)")
	}
	m.IntegratedLUFS = i
	m.LRA, _ = firstFloat(reLRA, sum)
	m.TruePeakDBFS, _ = firstFloat(reTruePeak, sum)
	m.ShortTermMaxLUFS = -120
	for _, sm := range reShortTerm.FindAllStringSubmatch(report, -1) {
		if v, err := strconv.ParseFloat(sm[1], 64); err == nil && v > m.ShortTermMaxLUFS && v > -100 {
			m.ShortTermMaxLUFS = v
		}
	}
	// astats prints "Overall" last; read the figures after it.
	ov := report
	if j := strings.LastIndex(report, "] Overall"); j >= 0 {
		ov = report[j:]
	}
	m.PeakDB = astat(ov, "Peak level dB")
	m.RMSDB = astat(ov, "RMS level dB")
	m.NoiseFloorDB = astat(ov, "RMS trough dB")
	if m.NoiseFloorDB == 0 {
		m.NoiseFloorDB = astat(ov, "RMS through dB") // older ffmpeg spelling
	}
	if samples := astat(ov, "Number of samples"); samples > 0 && m.PeakDB >= -0.1 {
		m.ClippedShare = round(astat(ov, "Peak count")/samples, 5)
	}
	m.Silent = m.IntegratedLUFS <= -70
	return m, nil
}

func firstFloat(re *regexp.Regexp, s string) (float64, bool) {
	m := re.FindStringSubmatch(s)
	if m == nil {
		return 0, false
	}
	if m[1] == "-inf" {
		return -120, true
	}
	v, err := strconv.ParseFloat(m[1], 64)
	return v, err == nil
}

func astat(s, label string) float64 {
	i := strings.Index(s, label+":")
	if i < 0 {
		return 0
	}
	rest := strings.TrimSpace(s[i+len(label)+1:])
	end := strings.IndexAny(rest, "\r\n")
	if end >= 0 {
		rest = rest[:end]
	}
	rest = strings.TrimSpace(rest)
	if strings.HasPrefix(rest, "-inf") {
		return -120
	}
	v, err := strconv.ParseFloat(strings.Fields(rest + " ")[0], 64)
	if err != nil {
		return 0
	}
	return v
}

// Set flattens to MetricSet names.
func (m *AudioMetrics) Set() MetricSet {
	return MetricSet{
		"integratedLUFS": round(m.IntegratedLUFS, 1), "shortTermMaxLUFS": round(m.ShortTermMaxLUFS, 1),
		"loudnessRange": round(m.LRA, 1), "truePeakDBFS": round(m.TruePeakDBFS, 1),
		"peakDB": round(m.PeakDB, 1), "rmsDB": round(m.RMSDB, 1), "noiseFloorDB": round(m.NoiseFloorDB, 1),
		"clippedShare": m.ClippedShare,
	}
}
