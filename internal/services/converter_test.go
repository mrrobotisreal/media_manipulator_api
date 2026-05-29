package services

import (
	"strings"
	"testing"
)

// countFlag returns how many times an exact flag token appears in args.
func countFlag(args []string, flag string) int {
	n := 0
	for _, a := range args {
		if a == flag {
			n++
		}
	}
	return n
}

// valueAfter returns the token immediately following the given flag, or "".
func valueAfter(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func TestVideoCRF(t *testing.T) {
	cases := map[string]string{
		"low":    "30",
		"medium": "23",
		"high":   "18",
		"":       "23", // unexpected -> medium default
		"bogus":  "23",
	}
	for quality, want := range cases {
		if got := videoCRF(quality); got != want {
			t.Errorf("videoCRF(%q) = %q, want %q", quality, got, want)
		}
	}
}

// TestVideoOutputCodecArgsNoDuplicateFlags is the key regression guard: the old
// code emitted "-c:v" / "-c:a" twice for some formats. Every format must set
// each codec exactly once.
func TestVideoOutputCodecArgsNoDuplicateFlags(t *testing.T) {
	formats := []string{"mp4", "mov", "mkv", "flv", "avi", "wmv", "webm", "prores", "dnxhd"}
	for _, f := range formats {
		args := videoOutputCodecArgs(f, "medium", true)
		if c := countFlag(args, "-c:v"); c != 1 {
			t.Errorf("format %q: expected exactly one -c:v, got %d (%v)", f, c, args)
		}
		if c := countFlag(args, "-c:a"); c != 1 {
			t.Errorf("format %q: expected exactly one -c:a, got %d (%v)", f, c, args)
		}
	}
}

func TestVideoOutputCodecArgsMP4(t *testing.T) {
	args := videoOutputCodecArgs("mp4", "high", false)
	joined := strings.Join(args, " ")
	if valueAfter(args, "-c:v") != "libx264" {
		t.Errorf("mp4 video codec = %q, want libx264 (%v)", valueAfter(args, "-c:v"), args)
	}
	if valueAfter(args, "-c:a") != "aac" {
		t.Errorf("mp4 audio codec = %q, want aac", valueAfter(args, "-c:a"))
	}
	if valueAfter(args, "-pix_fmt") != "yuv420p" {
		t.Errorf("mp4 must set -pix_fmt yuv420p, got %q (%v)", valueAfter(args, "-pix_fmt"), args)
	}
	if !strings.Contains(joined, "-movflags +faststart") {
		t.Errorf("mp4 must include -movflags +faststart, got %v", args)
	}
	if valueAfter(args, "-crf") != "18" {
		t.Errorf("mp4 high quality CRF = %q, want 18", valueAfter(args, "-crf"))
	}
}

func TestVideoOutputCodecArgsMOV(t *testing.T) {
	args := videoOutputCodecArgs("mov", "medium", false)
	if valueAfter(args, "-c:v") != "libx264" {
		t.Errorf("mov should default to H.264, got %q", valueAfter(args, "-c:v"))
	}
	if valueAfter(args, "-c:a") != "aac" {
		t.Errorf("mov audio = %q, want aac", valueAfter(args, "-c:a"))
	}
	if !strings.Contains(strings.Join(args, " "), "+faststart") {
		t.Errorf("mov should include +faststart, got %v", args)
	}
}

func TestVideoOutputCodecArgsWebM(t *testing.T) {
	// VP9 + Opus when available.
	vp9 := videoOutputCodecArgs("webm", "medium", true)
	if valueAfter(vp9, "-c:v") != "libvpx-vp9" {
		t.Errorf("webm (vp9 available) video codec = %q, want libvpx-vp9", valueAfter(vp9, "-c:v"))
	}
	if valueAfter(vp9, "-c:a") != "libopus" {
		t.Errorf("webm (vp9 available) audio codec = %q, want libopus", valueAfter(vp9, "-c:a"))
	}

	// VP8 + Vorbis fallback when VP9/Opus is unavailable.
	vp8 := videoOutputCodecArgs("webm", "medium", false)
	if valueAfter(vp8, "-c:v") != "libvpx" {
		t.Errorf("webm fallback video codec = %q, want libvpx", valueAfter(vp8, "-c:v"))
	}
	if valueAfter(vp8, "-c:a") != "libvorbis" {
		t.Errorf("webm fallback audio codec = %q, want libvorbis", valueAfter(vp8, "-c:a"))
	}
}

func TestVideoOutputCodecArgsAVI(t *testing.T) {
	// AVI predates AAC; MP3 audio keeps it broadly playable.
	args := videoOutputCodecArgs("avi", "medium", false)
	if valueAfter(args, "-c:a") != "libmp3lame" {
		t.Errorf("avi audio = %q, want libmp3lame", valueAfter(args, "-c:a"))
	}
	if valueAfter(args, "-c:v") != "libx264" {
		t.Errorf("avi video = %q, want libx264", valueAfter(args, "-c:v"))
	}
}

func TestVideoOutputCodecArgsWMV(t *testing.T) {
	args := videoOutputCodecArgs("wmv", "medium", false)
	if valueAfter(args, "-c:v") != "wmv2" {
		t.Errorf("wmv video = %q, want wmv2", valueAfter(args, "-c:v"))
	}
	if valueAfter(args, "-c:a") != "wmav2" {
		t.Errorf("wmv audio = %q, want wmav2", valueAfter(args, "-c:a"))
	}
	// wmv2 must NOT use CRF.
	if countFlag(args, "-crf") != 0 {
		t.Errorf("wmv must not use -crf, got %v", args)
	}
}

func TestVideoOutputCodecArgsProResAndDNxHD(t *testing.T) {
	pr := videoOutputCodecArgs("prores", "high", false)
	if valueAfter(pr, "-c:v") != "prores_ks" {
		t.Errorf("prores video = %q, want prores_ks", valueAfter(pr, "-c:v"))
	}
	dn := videoOutputCodecArgs("dnxhd", "high", false)
	if valueAfter(dn, "-c:v") != "dnxhd" {
		t.Errorf("dnxhd video = %q, want dnxhd", valueAfter(dn, "-c:v"))
	}
	if valueAfter(dn, "-profile:v") != "dnxhr_hq" {
		t.Errorf("dnxhd profile = %q, want dnxhr_hq", valueAfter(dn, "-profile:v"))
	}
}
