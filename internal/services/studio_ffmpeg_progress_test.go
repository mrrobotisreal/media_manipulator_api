package services

import (
	"bufio"
	"strings"
	"testing"
)

// ffmpeg writes its stats line with '\r' so terminals redraw it; each update
// must come out as its own token, or progress only arrives when ffmpeg exits.
func TestScanFFmpegLinesSplitsCarriageReturns(t *testing.T) {
	in := "  Duration: 00:00:10.00, start: 0\n" +
		"frame=  10 time=00:00:01.00 speed=1x\r" +
		"frame=  50 time=00:00:05.00 speed=1x\r" +
		"frame= 100 time=00:00:10.00 speed=1x\r\n" +
		"tail"
	sc := bufio.NewScanner(strings.NewReader(in))
	sc.Split(scanFFmpegLines)
	var times []string
	var all []string
	for sc.Scan() {
		line := sc.Text()
		all = append(all, line)
		if m := studioTimeRegex.FindStringSubmatch(line); m != nil {
			times = append(times, m[3])
		}
	}
	if got := strings.Join(times, ","); got != "01,05,10" {
		t.Fatalf("time= updates = %q, want 01,05,10 (one per stats line)", got)
	}
	if all[len(all)-1] != "tail" {
		t.Fatalf("last token = %q, want the unterminated tail", all[len(all)-1])
	}
}
