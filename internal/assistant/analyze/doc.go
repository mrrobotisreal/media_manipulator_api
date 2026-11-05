// Package analyze measures media for Medi's "numbers first" loop (MM115).
//
// Pure Go where possible (images: histograms, clipping, white balance,
// sharpness, noise, dominant hues), ffmpeg/ffprobe subprocesses with timeouts
// for audio (EBU R128 loudness, true peak, noise floor, clipping), video
// (ffprobe summary, scene luma, audio) and file comparison (PSNR, SSIM). No
// GPU and no model: every figure is a measurement a colourist or an audio
// engineer would read off a meter.
//
// Every analyzer returns a MetricSet: a flat name → number map in fixed units
// (luma and clip shares are 0..1, loudness is LUFS, peaks are dBFS). The same
// shape arrives from the editors, which measure their current frame in the
// browser (`context.metrics`, `editResult.metricsBefore/After`), so
// Compare works on either source.
//
// Face counting is omitted: the existing detector needs a GPU/Python stack, so
// it cannot run cheaply CPU-side inside the API process (MM115 decision 1).
package analyze

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
)

// MetricSet is a flat set of named measurements.
type MetricSet map[string]float64

// Body limits (MM115 decision 2).
const (
	// MaxMetricsBytes is the largest JSON a metric set may be on the wire.
	MaxMetricsBytes = 4096
	maxMetricKeys   = 128
	maxMetricValue  = 1e6
)

var metricKeyRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{0,31}$`)

// Validate checks a client-sent set: name shape, count, finite bounded values
// and the 4 KB wire limit. A nil or empty set is valid.
func (m MetricSet) Validate() error {
	if len(m) > maxMetricKeys {
		return fmt.Errorf("at most %d metrics", maxMetricKeys)
	}
	size := 2
	for k, v := range m {
		if !metricKeyRe.MatchString(k) {
			return fmt.Errorf("metric name %q is not a short identifier", k)
		}
		if math.IsNaN(v) || math.IsInf(v, 0) || math.Abs(v) > maxMetricValue {
			return fmt.Errorf("metric %s is not a bounded number", k)
		}
		size += len(k) + len(trimFloat(v)) + 4
	}
	if size > MaxMetricsBytes {
		return fmt.Errorf("metrics are larger than %d bytes", MaxMetricsBytes)
	}
	return nil
}

// Keys returns the metric names, sorted.
func (m MetricSet) Keys() []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// String renders "name 0.123, name2 -4.5" in name order.
func (m MetricSet) String() string {
	parts := make([]string, 0, len(m))
	for _, k := range m.Keys() {
		parts = append(parts, k+" "+trimFloat(m[k]))
	}
	return strings.Join(parts, ", ")
}

func round(v float64, places int) float64 {
	p := math.Pow(10, float64(places))
	return math.Round(v*p) / p
}

func trimFloat(v float64) string {
	s := fmt.Sprintf("%.4f", v)
	s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	if s == "" || s == "-0" {
		return "0"
	}
	return s
}
