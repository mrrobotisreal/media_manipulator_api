package handlers

import (
	"strings"
	"testing"
)

// These tests cover the PURE helpers behind the DR "Create Doc" endpoints:
// allowlisting + size caps, slug derivation + uniqueness suffixing, structural
// content validation, and dr-asset:// reference extraction. They mirror the
// style of dr_auth_test.go and run with NO database, network, or AWS/Firebase
// access (pure functions over in-memory inputs only).

func TestDocAssetExt(t *testing.T) {
	tests := []struct {
		name        string
		kind        string
		contentType string
		wantExt     string
		wantMax     int64
		wantOK      bool
	}{
		{"image png", "image", "image/png", "png", drMaxImageAssetBytes, true},
		{"image jpeg", "image", "image/jpeg", "jpg", drMaxImageAssetBytes, true},
		{"image webp", "image", "image/webp", "webp", drMaxImageAssetBytes, true},
		{"image gif", "image", "image/gif", "gif", drMaxImageAssetBytes, true},
		{"image mixed case content type", "image", "IMAGE/PNG", "png", drMaxImageAssetBytes, true},
		{"image with charset trimmed? no", "image", "image/png; charset=utf-8", "", 0, false},
		{"video mp4", "video", "video/mp4", "mp4", drMaxVideoAssetBytes, true},
		{"video webm", "video", "video/webm", "webm", drMaxVideoAssetBytes, true},
		{"video quicktime", "video", "video/quicktime", "mov", drMaxVideoAssetBytes, true},
		{"file pdf", "file", "application/pdf", "pdf", drMaxFileAssetBytes, true},
		{"file zip", "file", "application/zip", "zip", drMaxFileAssetBytes, true},
		{"file txt", "file", "text/plain", "txt", drMaxFileAssetBytes, true},
		{"file csv", "file", "text/csv", "csv", drMaxFileAssetBytes, true},
		{"file json", "file", "application/json", "json", drMaxFileAssetBytes, true},
		{"file md", "file", "text/markdown", "md", drMaxFileAssetBytes, true},
		{"file docx", "file", "application/vnd.openxmlformats-officedocument.wordprocessingml.document", "docx", drMaxFileAssetBytes, true},
		{"file xlsx", "file", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", "xlsx", drMaxFileAssetBytes, true},
		// Cross-kind mismatches and unknowns are rejected.
		{"kind/content mismatch", "image", "video/mp4", "", 0, false},
		{"unknown image type", "image", "image/tiff", "", 0, false},
		{"unknown file type", "file", "application/x-msdownload", "", 0, false},
		{"unknown kind", "audio", "audio/mpeg", "", 0, false},
		{"empty", "", "", "", 0, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ext, max, ok := docAssetExt(tc.kind, tc.contentType)
			if ok != tc.wantOK || ext != tc.wantExt || max != tc.wantMax {
				t.Fatalf("docAssetExt(%q,%q) = (%q,%d,%v), want (%q,%d,%v)",
					tc.kind, tc.contentType, ext, max, ok, tc.wantExt, tc.wantMax, tc.wantOK)
			}
		})
	}
}

func TestSlugifyDrTitle(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"Backend & AI Infrastructure", "backend-ai-infrastructure"},
		{"  Hello, World!  ", "hello-world"},
		{"It's Great", "it-s-great"},
		{"Multiple   spaces --- and dashes", "multiple-spaces-and-dashes"},
		{"UPPER lower 123", "upper-lower-123"},
		{"", "doc"},
		{"!!!", "doc"},
		{"café ☕", "caf"},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			if got := slugifyDrTitle(tc.in); got != tc.want {
				t.Fatalf("slugifyDrTitle(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if got := slugifyDrTitle(tc.in); !drSlugPattern.MatchString(got) {
				t.Fatalf("slugifyDrTitle(%q) = %q does not match drSlugPattern", tc.in, got)
			}
		})
	}
}

func TestSlugifyDrTitleCapsLengthWithoutTrailingDash(t *testing.T) {
	// 119 'a' + " bbb" would slugify to 123 chars; capped at 120 then any
	// trailing dash trimmed → no dash at the boundary.
	in := strings.Repeat("a", 119) + " bbb"
	got := slugifyDrTitle(in)
	if len(got) > 120 {
		t.Fatalf("slug length %d exceeds 120", len(got))
	}
	if strings.HasSuffix(got, "-") {
		t.Fatalf("slug has a trailing dash: %q", got)
	}
	if !drSlugPattern.MatchString(got) {
		t.Fatalf("capped slug %q does not match drSlugPattern", got)
	}
}

func TestNextAvailableSlug(t *testing.T) {
	tests := []struct {
		name  string
		base  string
		taken map[string]bool
		want  string
	}{
		{"free", "backend", map[string]bool{}, "backend"},
		{"one collision", "backend", map[string]bool{"backend": true}, "backend-2"},
		{"two collisions", "backend", map[string]bool{"backend": true, "backend-2": true}, "backend-3"},
		{"gap reused", "backend", map[string]bool{"backend": true, "backend-2": true, "backend-3": true}, "backend-4"},
		{"empty base defaults to doc", "", map[string]bool{}, "doc"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := nextAvailableSlug(tc.base, func(s string) bool { return tc.taken[s] })
			if got != tc.want {
				t.Fatalf("nextAvailableSlug(%q) = %q, want %q", tc.base, got, tc.want)
			}
		})
	}
}

func TestValidateDrBlocksJSON(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		wantErr bool
	}{
		{"empty blocks ok", `{"format":"dr-blocks/v1","blocks":[]}`, false},
		{"valid paragraph", `{"format":"dr-blocks/v1","blocks":[{"type":"paragraph","spans":[]}]}`, false},
		{"all known types", `{"format":"dr-blocks/v1","blocks":[{"type":"heading"},{"type":"divider"},{"type":"table"},{"type":"code"},{"type":"callout"},{"type":"list"},{"type":"blockquote"},{"type":"image"},{"type":"video"},{"type":"file"}]}`, false},
		{"empty string", ``, true},
		{"not an object", `[1,2,3]`, true},
		{"scalar", `"nope"`, true},
		{"wrong format", `{"format":"nope","blocks":[]}`, true},
		{"missing format", `{"blocks":[]}`, true},
		{"missing blocks", `{"format":"dr-blocks/v1"}`, true},
		{"null blocks", `{"format":"dr-blocks/v1","blocks":null}`, true},
		{"blocks not array", `{"format":"dr-blocks/v1","blocks":{}}`, true},
		{"unknown block type", `{"format":"dr-blocks/v1","blocks":[{"type":"widget"}]}`, true},
		{"block missing type", `{"format":"dr-blocks/v1","blocks":[{"foo":1}]}`, true},
		{"block is scalar", `{"format":"dr-blocks/v1","blocks":[123]}`, true},
		{"invalid json", `{"format":"dr-blocks/v1","blocks":[`, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateDrBlocksJSON([]byte(tc.raw))
			if (err != nil) != tc.wantErr {
				t.Fatalf("validateDrBlocksJSON(%q) error = %v, wantErr = %v", tc.raw, err, tc.wantErr)
			}
		})
	}
}

func TestValidateDrBlocksJSONRejectsOversize(t *testing.T) {
	// The size check runs before parsing, so an oversize buffer is rejected
	// regardless of JSON validity.
	raw := make([]byte, drMaxContentBytes+1)
	if err := validateDrBlocksJSON(raw); err == nil {
		t.Fatal("expected oversize content to be rejected")
	}
}

func TestExtractDrAssetRefs(t *testing.T) {
	content := `{
	  "format": "dr-blocks/v1",
	  "blocks": [
	    {"type":"paragraph","spans":[{"text":"hi"}]},
	    {"type":"image","src":"dr-asset://11111111-1111-1111-1111-111111111111","alt":"a"},
	    {"type":"video","src":"https://example.com/v.mp4"},
	    {"type":"file","src":"dr-asset://22222222-2222-2222-2222-222222222222","name":"x.pdf"},
	    {"type":"image","src":""}
	  ]
	}`
	refs, err := extractDrAssetRefs([]byte(content))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"11111111-1111-1111-1111-111111111111", "22222222-2222-2222-2222-222222222222"}
	if len(refs) != len(want) {
		t.Fatalf("got %v, want %v", refs, want)
	}
	for i := range want {
		if refs[i] != want[i] {
			t.Fatalf("ref[%d] = %q, want %q", i, refs[i], want[i])
		}
	}

	if _, err := extractDrAssetRefs([]byte(`not json`)); err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestParseDrAssetRef(t *testing.T) {
	tests := []struct {
		src     string
		wantRef string
		wantOK  bool
	}{
		{"dr-asset://abc", "abc", true},
		{"https://example.com/x.png", "", false},
		{"dr-asset://", "", false},
		{"", "", false},
		{"/uploads/x.png", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.src, func(t *testing.T) {
			ref, ok := parseDrAssetRef(tc.src)
			if ref != tc.wantRef || ok != tc.wantOK {
				t.Fatalf("parseDrAssetRef(%q) = (%q,%v), want (%q,%v)", tc.src, ref, ok, tc.wantRef, tc.wantOK)
			}
		})
	}
}

func TestSanitizeDrFileName(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"report.pdf", "report.pdf"},
		{"../../etc/passwd", "passwd"},
		{"a/b/c.txt", "c.txt"},
		{`C:\Users\me\notes.md`, "notes.md"},
		{"  spaced.csv  ", "spaced.csv"},
		{"", "file"},
		{"   ", "file"},
		{"..", "file"},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			if got := sanitizeDrFileName(tc.in); got != tc.want {
				t.Fatalf("sanitizeDrFileName(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
	// Control characters are stripped.
	if got := sanitizeDrFileName("a\x00b\x1fc.txt"); got != "abc.txt" {
		t.Fatalf("control-char sanitize = %q, want %q", got, "abc.txt")
	}
	// Length is bounded.
	long := strings.Repeat("x", 400) + ".pdf"
	if got := sanitizeDrFileName(long); len([]rune(got)) > drMaxFileNameChars {
		t.Fatalf("sanitized name length %d exceeds cap %d", len([]rune(got)), drMaxFileNameChars)
	}
}

func TestDeriveDrSummary(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{
			"first paragraph",
			`{"format":"dr-blocks/v1","blocks":[{"type":"heading","text":"T"},{"type":"paragraph","spans":[{"text":"Hello "},{"text":"world"}]}]}`,
			"Hello world",
		},
		{
			"skips empty paragraph",
			`{"format":"dr-blocks/v1","blocks":[{"type":"paragraph","spans":[]},{"type":"paragraph","spans":[{"text":"Second"}]}]}`,
			"Second",
		},
		{
			"no paragraph",
			`{"format":"dr-blocks/v1","blocks":[{"type":"heading","text":"T"},{"type":"divider"}]}`,
			"",
		},
		{
			"invalid json",
			`nope`,
			"",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := deriveDrSummary([]byte(tc.raw)); got != tc.want {
				t.Fatalf("deriveDrSummary = %q, want %q", got, tc.want)
			}
		})
	}
	// Long paragraph is truncated to the derived-summary cap.
	long := strings.Repeat("a", 500)
	raw := `{"format":"dr-blocks/v1","blocks":[{"type":"paragraph","spans":[{"text":"` + long + `"}]}]}`
	got := deriveDrSummary([]byte(raw))
	if len([]rune(got)) != drDerivedSummaryChars {
		t.Fatalf("truncated summary length = %d, want %d", len([]rune(got)), drDerivedSummaryChars)
	}
}
