package handlers

import (
	"strings"
	"testing"

	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
)

func TestAttachmentExt(t *testing.T) {
	cases := []struct {
		ct   string
		want string
		ok   bool
	}{
		{"image/png", "png", true},
		{"image/jpeg", "jpg", true},
		{"image/webp", "webp", true},
		{"image/gif", "gif", true},
		{"IMAGE/PNG", "png", true},     // case-insensitive
		{" image/jpeg ", "jpg", true},  // trimmed
		{"image/svg+xml", "", false},   // not allowlisted
		{"application/pdf", "", false}, // not an image
		{"", "", false},
	}
	for _, tc := range cases {
		got, ok := attachmentExt(tc.ct)
		if got != tc.want || ok != tc.ok {
			t.Errorf("attachmentExt(%q) = (%q,%v), want (%q,%v)", tc.ct, got, ok, tc.want, tc.ok)
		}
	}
}

func TestAttachmentKeyBuilding(t *testing.T) {
	if got := commentAttachmentKey("doc1", "com1", "img1", "png"); got != "documents/doc1/comments/com1/img1.png" {
		t.Errorf("commentAttachmentKey = %q", got)
	}
	if got := replyAttachmentKey("doc1", "com1", "rep1", "img1", "jpg"); got != "documents/doc1/comments/com1/replies/rep1/img1.jpg" {
		t.Errorf("replyAttachmentKey = %q", got)
	}
}

func TestCheckDraftMutation(t *testing.T) {
	cases := []struct {
		name           string
		status         string
		author, caller string
		want           error
	}{
		{"author + draft passes", "draft", "u1", "u1", nil},
		{"non-author + draft forbidden", "draft", "u1", "u2", errDrNotAuthor},
		{"author + published conflict", "published", "u1", "u1", errDrNotDraft},
		{"non-author + published forbidden (author checked first)", "published", "u1", "u2", errDrNotAuthor},
	}
	for _, tc := range cases {
		if got := checkDraftMutation(tc.status, tc.author, tc.caller); got != tc.want {
			t.Errorf("%s: checkDraftMutation = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestCheckAuthorOnly(t *testing.T) {
	if err := checkAuthorOnly("u1", "u1"); err != nil {
		t.Errorf("same author should pass, got %v", err)
	}
	if err := checkAuthorOnly("u1", "u2"); err != errDrNotAuthor {
		t.Errorf("different author should be errDrNotAuthor, got %v", err)
	}
}

func TestValidatePublishInput(t *testing.T) {
	cases := []struct {
		name              string
		body              string
		uploaded, pending int
		wantErr           bool
	}{
		{"text only ok", "hello", 0, 0, false},
		{"image only ok", "", 1, 0, false},
		{"whitespace + image ok", "   ", 1, 0, false},
		{"empty rejected", "", 0, 0, true},
		{"whitespace only rejected", "   \n ", 0, 0, true},
		{"pending upload blocks", "hello", 0, 1, true},
		{"at limit ok", strings.Repeat("a", models.DrCommentMaxBody), 0, 0, false},
		{"too long rejected", strings.Repeat("a", models.DrCommentMaxBody+1), 0, 0, true},
	}
	for _, tc := range cases {
		err := validatePublishInput(tc.body, tc.uploaded, tc.pending)
		if (err != nil) != tc.wantErr {
			t.Errorf("%s: validatePublishInput err=%v, wantErr=%v", tc.name, err, tc.wantErr)
		}
	}
}

func TestAbsInt64(t *testing.T) {
	for _, tc := range []struct{ in, want int64 }{{5, 5}, {-5, 5}, {0, 0}} {
		if got := absInt64(tc.in); got != tc.want {
			t.Errorf("absInt64(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}
