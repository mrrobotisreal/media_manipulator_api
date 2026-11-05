package handlers

import (
	"bytes"
	"context"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/metadata"
	"github.com/mrrobotisreal/media_manipulator_api/internal/services"
)

// 1x1 PNG.
var onePixelPNG = []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0x00, 0x00, 0x0d, 0x49, 0x48, 0x44, 0x52, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01, 0x08, 0x06, 0x00, 0x00, 0x00, 0x1f, 0x15, 0xc4, 0x89, 0x00, 0x00, 0x00, 0x0d, 0x49, 0x44, 0x41, 0x54, 0x78, 0x9c, 0x63, 0xf8, 0xcf, 0xc0, 0xf0, 0x1f, 0x00, 0x05, 0x00, 0x01, 0xff, 0x89, 0x99, 0x3d, 0x1d, 0x00, 0x00, 0x00, 0x00, 0x49, 0x45, 0x4e, 0x44, 0xae, 0x42, 0x60, 0x82}

func newTestFiles(t *testing.T) (*AssistantFiles, *[]string) {
	t.Helper()
	cfg := &config.Config{UploadDir: t.TempDir(), TempDir: t.TempDir(), MaxFileSize: 1 << 20, CommandTimeout: 5e9}
	var ledgered []string
	f := &AssistantFiles{cfg: cfg, inspector: services.NewMediaInspector(cfg.CommandTimeout),
		inspect: func(_ context.Context, path, kind string) *metadata.MetadataDocument {
			return &metadata.MetadataDocument{File: metadata.FileSummary{Kind: kind}, Entries: []metadata.MetadataEntry{{Group: "File", Tag: "x", Value: 1}}}
		},
		ledger: func(_ *gin.Context, id string) { ledgered = append(ledgered, id) }}
	return f, &ledgered
}

func uploadTo(t *testing.T, f *AssistantFiles, session, name string, body []byte, user string) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", name)
	_, _ = fw.Write(body)
	_ = mw.Close()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, AssistantFilesPath, &buf)
	c.Request.Header.Set("Content-Type", mw.FormDataContentType())
	if session != "" {
		c.Request.Header.Set("X-MM-Session-ID", session)
	}
	f.Upload(c, assistantCaller{})
	_ = user
	return w
}

// MM116a decision 3: the upload is stored like a job upload and ledgered, then resolvable only by its owner.
func TestAssistantFileUploadLedgerAndOwnership(t *testing.T) {
	f, ledgered := newTestFiles(t)
	w := uploadTo(t, f, "sess-a", "../../evil name.png", onePixelPNG, "")
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `"kind":"image"`) || !strings.Contains(body, `"name":"evil name.png"`) {
		t.Errorf("body = %s", body)
	}
	if len(*ledgered) != 1 {
		t.Fatalf("ledgered = %v", *ledgered)
	}
	id := (*ledgered)[0]
	// stored inside uploads/<id>/, nowhere else
	entries, _ := os.ReadDir(filepath.Join(f.cfg.UploadDir, id))
	if len(entries) != 2 {
		t.Errorf("job dir has %d entries", len(entries))
	}
	if !f.Owns("s:sess-a", id) {
		t.Error("the owner cannot resolve its file")
	}
	for _, other := range []string{"s:sess-b", "u:someone", ""} {
		if f.Owns(other, id) {
			t.Errorf("owner %q resolved another visitor's file", other)
		}
	}
	p, _, err := f.ResolveFile(context.Background(), "s:sess-a", id)
	if err != nil || !strings.HasPrefix(p, f.cfg.UploadDir) {
		t.Errorf("resolve = %q %v", p, err)
	}
	if _, _, err := f.ResolveFile(context.Background(), "s:sess-a", "../../etc/passwd"); err == nil {
		t.Error("resolved a traversal ref")
	}
	kind, mime, doc := f.InspectFile(context.Background(), p)
	if kind != "image" || !strings.HasPrefix(mime, "image/") || doc == nil {
		t.Errorf("inspect = %q %q %v", kind, mime, doc)
	}
}

func TestAssistantFileUploadRefusals(t *testing.T) {
	f, ledgered := newTestFiles(t)
	if w := uploadTo(t, f, "", "a.png", onePixelPNG, ""); w.Code != 400 {
		t.Errorf("no session: %d", w.Code)
	}
	if w := uploadTo(t, f, "s", "notes.txt", []byte("hello there, plain text"), ""); w.Code != 400 || !strings.Contains(w.Body.String(), "unsupported_file") {
		t.Errorf("text file: %d %s", w.Code, w.Body.String())
	}
	if w := uploadTo(t, f, "s", "big.png", append(append([]byte{}, onePixelPNG...), make([]byte, 2<<20)...), ""); w.Code != 413 {
		t.Errorf("oversized: %d", w.Code)
	}
	if len(*ledgered) != 0 {
		t.Errorf("a refused upload was ledgered: %v", *ledgered)
	}
	// nothing is left behind
	entries, _ := os.ReadDir(f.cfg.UploadDir)
	if len(entries) != 0 {
		t.Errorf("leftover dirs: %d", len(entries))
	}
}
