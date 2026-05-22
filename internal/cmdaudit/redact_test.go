package cmdaudit

import (
	"strings"
	"testing"
)

func TestRedactPath_KnownDirs(t *testing.T) {
	s := NewPathSanitizer("/srv/upload", "/srv/output", "/srv/tmp")
	got := s.RedactArg("/srv/upload/abc/original.mp4")
	if !strings.Contains(got, "<UPLOAD_DIR>/abc/original.mp4") {
		t.Errorf("upload path not redacted: %q", got)
	}
	got = s.RedactArg("/srv/output/abc/converted.mp4")
	if !strings.Contains(got, "<OUTPUT_DIR>/abc/converted.mp4") {
		t.Errorf("output path not redacted: %q", got)
	}
	got = s.RedactArg("/srv/tmp/working.tmp")
	if !strings.Contains(got, "<TEMP_DIR>/working.tmp") {
		t.Errorf("temp path not redacted: %q", got)
	}
}

func TestRedactPath_HomeDir(t *testing.T) {
	s := NewPathSanitizer("", "", "")
	got := s.RedactArg("/Users/mitch/secret.txt")
	if !strings.Contains(got, "/Users/<USER>") {
		t.Errorf("home dir not redacted: %q", got)
	}
	got = s.RedactArg("/home/alice/.config")
	if !strings.Contains(got, "/home/<USER>") {
		t.Errorf("linux home dir not redacted: %q", got)
	}
}

func TestRedactPresignedURL(t *testing.T) {
	s := NewPathSanitizer("", "", "")
	input := "https://s3.example/bucket/key?X-Amz-Signature=deadbeefcafe&X-Amz-Credential=ABCDEFG&foo=bar"
	got := s.RedactArg(input)
	if strings.Contains(got, "deadbeefcafe") {
		t.Errorf("signature leaked: %q", got)
	}
	if !strings.Contains(got, "X-Amz-Signature=") {
		t.Errorf("expected X-Amz-Signature param still present (masked): %q", got)
	}
	if !strings.Contains(got, "foo=bar") {
		t.Errorf("non-sensitive params should remain: %q", got)
	}
}

func TestRedactBearer(t *testing.T) {
	s := NewPathSanitizer("", "", "")
	got := s.RedactArg("Authorization: Bearer abcdef1234567890")
	if strings.Contains(got, "abcdef1234567890") {
		t.Errorf("bearer leaked: %q", got)
	}
	if !strings.Contains(got, "Bearer ***") {
		t.Errorf("expected redacted Bearer ***: %q", got)
	}
}

func TestRedactEnv(t *testing.T) {
	s := NewPathSanitizer("", "", "")
	env := []string{
		"AWS_SECRET_ACCESS_KEY=verylongsecretvalue",
		"OPENAI_API_KEY=sk-abcdef",
		"PATH=/usr/bin:/bin",
		"HOME=/Users/mitch",
	}
	got := s.RedactEnv(env)
	if v := got["AWS_SECRET_ACCESS_KEY"]; v == "verylongsecretvalue" {
		t.Errorf("AWS secret should be masked, got %q", v)
	}
	if v := got["OPENAI_API_KEY"]; v == "sk-abcdef" {
		t.Errorf("OPENAI key should be masked, got %q", v)
	}
	if v := got["PATH"]; v != "/usr/bin:/bin" {
		t.Errorf("PATH should be left alone, got %q", v)
	}
	if v := got["HOME"]; !strings.Contains(v, "<USER>") {
		t.Errorf("HOME should have user redacted, got %q", v)
	}
}

func TestRedactAWSKey(t *testing.T) {
	s := NewPathSanitizer("", "", "")
	got := s.RedactArg("AKIAIOSFODNN7EXAMPLE foo")
	if strings.Contains(got, "AKIAIOSFODNN7EXAMPLE") {
		t.Errorf("AWS key leaked: %q", got)
	}
}

func TestTailString(t *testing.T) {
	in := strings.Repeat("a", 100)
	out := TailString(in, 10)
	if len(out) != 10 {
		t.Errorf("expected 10 bytes, got %d", len(out))
	}
	if got := TailString("short", 100); got != "short" {
		t.Errorf("short string should pass through, got %q", got)
	}
}
