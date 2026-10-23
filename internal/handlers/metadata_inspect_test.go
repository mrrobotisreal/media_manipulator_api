package handlers

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/metadata"
	"github.com/mrrobotisreal/media_manipulator_api/internal/services"
)

type stubRunner func(name string, args []string) (string, string, error)

func (f stubRunner) Run(_ context.Context, name string, args ...string) (string, string, error) {
	return f(name, args)
}

func metadataTestRouter(t *testing.T, r metadata.Runner) (*gin.Engine, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	tmp := t.TempDir()
	prev := inspectMetadata
	in := metadata.NewInspector(r)
	inspectMetadata = in.Inspect
	t.Cleanup(func() { inspectMetadata = prev })
	h := &ConversionHandler{
		cfg:       &config.Config{TempDir: tmp, MaxFileSize: 1 << 20, CommandTimeout: 5 * time.Second},
		inspector: services.NewMediaInspector(5 * time.Second),
	}
	engine := gin.New()
	RegisterConversionRoutes(engine.Group("/api"), h)
	return engine, tmp
}

func multipartBody(t *testing.T, field, name string, data []byte) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	if field != "" {
		fw, err := w.CreateFormFile(field, name)
		if err != nil {
			t.Fatal(err)
		}
		fw.Write(data)
	}
	w.Close()
	return &buf, w.FormDataContentType()
}

func post(engine *gin.Engine, path string, body *bytes.Buffer, ct string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, body)
	req.Header.Set("Content-Type", ct)
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)
	return rec
}

func TestInspectMetadataOK(t *testing.T) {
	engine, tmp := metadataTestRouter(t, stubRunner(func(name string, args []string) (string, string, error) {
		return `[{"SourceFile":"/x","File:FileType":"JPEG","IFD0:Make":"Apple"}]`, "", nil
	}))
	data := []byte("\xff\xd8\xff\xe0 fake jpeg bytes")
	body, ct := multipartBody(t, "file", "holiday.jpg", data)
	rec := post(engine, "/api/metadata/inspect", body, ct)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var doc metadata.MetadataDocument
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	if doc.SchemaVersion != 1 || doc.File.Name != "holiday.jpg" || doc.File.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("file summary %+v", doc.File)
	}
	if len(doc.Entries) != 2 || doc.Entries[1].ID != "IFD0:Make" || doc.Entries[1].Privacy != "device" {
		t.Fatalf("entries %+v", doc.Entries)
	}
	if left, _ := filepath.Glob(filepath.Join(tmp, "*")); len(left) != 0 {
		t.Fatalf("temp file not removed: %v", left)
	}
}

func TestInspectMetadataHidesServerTempFile(t *testing.T) {
	engine, _ := metadataTestRouter(t, stubRunner(func(name string, args []string) (string, string, error) {
		return `[{"SourceFile":"/srv/tmp/metadata_9_x.jpg","System:FileName":"metadata_9_x.jpg","System:Directory":"/srv/tmp","System:FilePermissions":"-rw-r--r--","System:FileAccessDate":"2026:10:23 09:55:50-06:00","File:FileType":"JPEG"}]`, "", nil
	}))
	body, ct := multipartBody(t, "file", "holiday.jpg", []byte("\xff\xd8\xff\xe0 fake jpeg bytes"))
	rec := post(engine, "/api/metadata/inspect", body, ct)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	out := rec.Body.String()
	for _, leak := range []string{"/srv/tmp", "metadata_9_x", "FilePermissions", "FileAccessDate", "Directory"} {
		if strings.Contains(out, leak) {
			t.Fatalf("response leaks %q: %s", leak, out)
		}
	}
	var doc metadata.MetadataDocument
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Entries) != 2 || doc.Entries[0].ID != "System:FileName" || doc.Entries[0].Value != "holiday.jpg" {
		t.Fatalf("entries %+v", doc.Entries)
	}
}

func TestInspectMetadataToolFailureIs200WithWarning(t *testing.T) {
	engine, _ := metadataTestRouter(t, stubRunner(func(string, []string) (string, string, error) {
		return "", "", errors.New("boom")
	}))
	body, ct := multipartBody(t, "file", "a.jpg", []byte("x"))
	rec := post(engine, "/api/metadata/inspect", body, ct)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"warnings":["exiftool failed: boom"]`) || !strings.Contains(rec.Body.String(), `"entries":[]`) {
		t.Fatalf("body %s", rec.Body.String())
	}
}

func TestInspectMetadataNoFileIs400(t *testing.T) {
	engine, _ := metadataTestRouter(t, stubRunner(func(string, []string) (string, string, error) { return "[]", "", nil }))
	body, ct := multipartBody(t, "other", "a.jpg", []byte("x"))
	if rec := post(engine, "/api/metadata/inspect", body, ct); rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d", rec.Code)
	}
	body, ct = multipartBody(t, "", "", nil)
	if rec := post(engine, "/api/metadata/inspect", body, ct); rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d", rec.Code)
	}
}

func TestInspectMetadataOversizedIs400(t *testing.T) {
	engine, _ := metadataTestRouter(t, stubRunner(func(string, []string) (string, string, error) { return "[]", "", nil }))
	body, ct := multipartBody(t, "file", "big.bin", bytes.Repeat([]byte("a"), 2<<20))
	if rec := post(engine, "/api/metadata/inspect", body, ct); rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d", rec.Code)
	}
}

func TestDetailsRouteStillRegistered(t *testing.T) {
	engine, _ := metadataTestRouter(t, stubRunner(func(string, []string) (string, string, error) { return "[]", "", nil }))
	have := map[string]bool{}
	for _, r := range engine.Routes() {
		have[r.Method+" "+r.Path] = true
	}
	for _, want := range []string{"POST /api/details", "POST /api/metadata/inspect"} {
		if !have[want] {
			t.Errorf("route %s missing", want)
		}
	}
	// /details without a file keeps its old 400 contract.
	body, ct := multipartBody(t, "other", "a.jpg", []byte("x"))
	if rec := post(engine, "/api/details", body, ct); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "no file provided") {
		t.Fatalf("details: %d %s", rec.Code, rec.Body.String())
	}
}
