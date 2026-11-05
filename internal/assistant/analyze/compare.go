package analyze

import (
	"context"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
)

// Delta is one metric's change.
type Delta struct {
	Name          string
	Before, After float64
	Change        float64 // After - Before
}

// Compare diffs two metric sets: every name present in both, sorted by name.
// Names in only one set are returned in only (marked by name, for the prompt).
func Compare(before, after MetricSet) (deltas []Delta, only []string) {
	for _, k := range before.Keys() {
		a, ok := after[k]
		if !ok {
			only = append(only, k)
			continue
		}
		deltas = append(deltas, Delta{Name: k, Before: before[k], After: a, Change: round(a-before[k], 4)})
	}
	for _, k := range after.Keys() {
		if _, ok := before[k]; !ok {
			only = append(only, k)
		}
	}
	return deltas, only
}

// FormatCompare renders the deltas as the tool result Medi reads: one line
// per metric, "name: before → after (change)", changed metrics first.
func FormatCompare(before, after MetricSet) string {
	deltas, only := Compare(before, after)
	if len(deltas) == 0 {
		return "No metrics in common between the before and after measurements."
	}
	var b strings.Builder
	b.WriteString("Measured on the same preview, before → after:\n")
	for _, d := range deltas {
		fmt.Fprintf(&b, "- %s: %s → %s (%s)\n", d.Name, trimFloat(d.Before), trimFloat(d.After), signed(d.Change))
	}
	if len(only) > 0 {
		fmt.Fprintf(&b, "Measured only once: %s.\n", strings.Join(only, ", "))
	}
	return strings.TrimSpace(b.String())
}

// Brief is the one-line "name a→b" form of the changed metrics, biggest
// relative change first, at most n names (used inline in an edit result).
func Brief(before, after MetricSet, n int) string {
	deltas, _ := Compare(before, after)
	type scored struct {
		d Delta
		s float64
	}
	var sc []scored
	for _, d := range deltas {
		if d.Change == 0 {
			continue
		}
		sc = append(sc, scored{d, math.Abs(d.Change) / math.Max(math.Abs(d.Before), 0.05)})
	}
	for i := 1; i < len(sc); i++ {
		for j := i; j > 0 && sc[j].s > sc[j-1].s; j-- {
			sc[j], sc[j-1] = sc[j-1], sc[j]
		}
	}
	if len(sc) > n {
		sc = sc[:n]
	}
	parts := make([]string, len(sc))
	for i, s := range sc {
		parts[i] = fmt.Sprintf("%s %s→%s", s.d.Name, trimFloat(s.d.Before), trimFloat(s.d.After))
	}
	return strings.Join(parts, ", ")
}

func signed(v float64) string {
	if v > 0 {
		return "+" + trimFloat(v)
	}
	return trimFloat(v)
}

// Similarity is the full-reference comparison of two media files.
type Similarity struct {
	// PSNR is the average peak signal-to-noise ratio in dB (capped at 100 for
	// identical inputs); SSIM is the mean structural similarity, 0..1.
	PSNR, SSIM float64
}

var (
	rePSNR = regexp.MustCompile(`PSNR[^\n]*average:(inf|-?[0-9.]+)`)
	reSSIM = regexp.MustCompile(`SSIM[^\n]*All:(-?[0-9.]+)`)
)

// CompareFiles measures PSNR and SSIM between the first video (or image)
// stream of two files; the second is scaled to the first's size.
func CompareFiles(ctx context.Context, a, b string) (*Similarity, error) {
	_, stderr, err := run(ctx, analyzeTimeout, "ffmpeg", "-nostdin", "-nostats", "-hide_banner", "-i", a, "-i", b,
		"-lavfi", "[1:v][0:v]scale2ref=flags=bicubic[b][a];[a]split[a1][a2];[b]split[b1][b2];[a1][b1]psnr;[a2][b2]ssim",
		"-f", "null", "-")
	if err != nil {
		return nil, err
	}
	s := &Similarity{}
	if m := rePSNR.FindStringSubmatch(string(stderr)); m != nil {
		if m[1] == "inf" {
			s.PSNR = 100
		} else {
			s.PSNR, _ = strconv.ParseFloat(m[1], 64)
		}
	} else {
		return nil, fmt.Errorf("analyze: no PSNR in the ffmpeg report")
	}
	if m := reSSIM.FindStringSubmatch(string(stderr)); m != nil {
		s.SSIM, _ = strconv.ParseFloat(m[1], 64)
	}
	s.PSNR, s.SSIM = round(s.PSNR, 2), round(s.SSIM, 4)
	return s, nil
}
