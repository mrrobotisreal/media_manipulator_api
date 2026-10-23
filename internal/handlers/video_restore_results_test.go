package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/middleware"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
	"github.com/mrrobotisreal/media_manipulator_api/internal/services"
	"github.com/mrrobotisreal/media_manipulator_api/internal/tiers"
)

// restoreRouter builds a completed restore job with kept results on disk.
func restoreRouter(t *testing.T, owner *services.JobOwner, complete bool) (*gin.Engine, *models.ConversionJob, []byte) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	out := t.TempDir()
	jm := services.NewJobManager()
	job := jm.CreateJob(models.OriginalFileInfo{Name: "old.mp4", Type: "video/mp4"}, map[string]interface{}{"mode": "restore"})
	_ = jm.SetMode(job.ID, "restore")
	kept := filepath.Join(out, job.ID, "results")
	body := bytes.Repeat([]byte{9}, 500)
	write := func(rel string, b []byte) {
		p := filepath.Join(kept, filepath.FromSlash(rel))
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("original/clip.mp4", []byte("orig"))
	write("realesrgan/realesrgan_x4.mp4", body)
	manifest := map[string]any{
		"jobId":  job.ID,
		"source": map[string]any{"width": 640, "height": 480},
		"models": []map[string]any{
			{"id": "realesrgan", "group": "frame", "status": "completed", "durationSeconds": 10, "outputWidth": 2560, "outputHeight": 1920, "outputFile": "restoration_results/realesrgan/realesrgan_x4.mp4", "note": "n"},
			{"id": "vrt", "group": "video", "status": "failed", "error": "Model timed out", "outputFile": "restoration_results/vrt/vrt_x4.mp4"},
		},
	}
	raw, _ := json.Marshal(manifest)
	write("manifest.json", raw)
	if owner != nil {
		jm.SetJobOwner(job.ID, *owner)
	}
	if complete {
		_ = jm.UpdateJobStatus(job.ID, models.StatusCompleted)
	}
	h := &VideoRestoreHandler{jobManager: jm, cfg: &config.Config{OutputDir: out}}
	r := gin.New()
	r.Use(middleware.TierContext(middleware.TierConfig{Store: &jobRecordStore{}, Registry: tiers.New(&config.Config{})}, true))
	RegisterVideoRestoreRoutes(r, h)
	return r, job, body
}

func TestVideoRestoreResultsManifest(t *testing.T) {
	SetResultOwnershipEnforce(true)
	r, job, _ := restoreRouter(t, nil, true)
	w := doGet(r, "/video-restore/"+job.ID+"/results", nil)
	if w.Code != 200 {
		t.Fatalf("code %d %s", w.Code, w.Body.String())
	}
	var resp models.RestoreResultsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.JobID != job.ID || resp.Original.ID != "original" || resp.Original.Mime != "video/mp4" || resp.Original.Width != 640 {
		t.Fatalf("original/jobId: %+v", resp)
	}
	if len(resp.Models) != 2 {
		t.Fatalf("models: %+v", resp.Models)
	}
	done, failed := resp.Models[0], resp.Models[1]
	if done.FileName != "realesrgan_x4.mp4" || done.SizeBytes != 500 || done.Mime != "video/mp4" || done.Width != 2560 || done.Height != 1920 {
		t.Errorf("completed: %+v", done)
	}
	if failed.FileName != "" || failed.Status != "failed" || failed.Error == "" {
		t.Errorf("failed model must omit fileName: %+v", failed)
	}
	var generic map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &generic)
	fm := generic["models"].([]any)[1].(map[string]any)
	if _, has := fm["fileName"]; has {
		t.Error("failed model JSON must not contain fileName")
	}
}

func TestVideoRestoreResultFileRangeAndNames(t *testing.T) {
	SetResultOwnershipEnforce(true)
	r, job, body := restoreRouter(t, nil, true)
	base := "/video-restore/" + job.ID + "/result/"

	w := doGet(r, base+"realesrgan", map[string]string{"Range": "bytes=0-99"})
	if w.Code != http.StatusPartialContent || w.Header().Get("Content-Range") != "bytes 0-99/500" || !bytes.Equal(w.Body.Bytes(), body[:100]) {
		t.Fatalf("range: %d %q", w.Code, w.Header().Get("Content-Range"))
	}
	if w.Header().Get("Content-Type") != "video/mp4" || w.Header().Get("Accept-Ranges") != "bytes" {
		t.Errorf("headers: %v", w.Header())
	}
	if w := doGet(r, base+"original", nil); w.Code != 200 || w.Body.String() != "orig" {
		t.Errorf("original: %d", w.Code)
	}
	if w := doGet(r, base+"realesrgan", map[string]string{"Range": "bytes=9000-9999"}); w.Code != http.StatusRequestedRangeNotSatisfiable {
		t.Errorf("unsatisfiable: %d", w.Code)
	}
	for _, name := range []string{"vrt", "hat", "..%2Fx", "../x", "manifest.json", "realesrgan_x4.mp4"} {
		if w := doGet(r, base+name, nil); w.Code != 404 {
			t.Errorf("%q: got %d want 404", name, w.Code)
		}
	}
}

func TestVideoRestoreResultsIncompleteAndWrongMode(t *testing.T) {
	r, job, _ := restoreRouter(t, nil, false)
	if w := doGet(r, "/video-restore/"+job.ID+"/results", nil); w.Code != 400 {
		t.Errorf("results incomplete: %d", w.Code)
	}
	if w := doGet(r, "/video-restore/"+job.ID+"/result/original", nil); w.Code != 400 {
		t.Errorf("result incomplete: %d", w.Code)
	}
	if w := doGet(r, "/video-restore/nope/results", nil); w.Code != 404 {
		t.Errorf("missing: %d", w.Code)
	}
}

func TestVideoRestoreResultsOwnership(t *testing.T) {
	SetResultOwnershipEnforce(true)
	r, job, _ := restoreRouter(t, &services.JobOwner{SessionID: "sess-owner"}, true)
	missing := doGet(r, "/video-restore/nope/results", nil)
	for _, p := range []string{"/results", "/result/original"} {
		u := "/video-restore/" + job.ID + p
		if w := doGet(r, u, map[string]string{"X-MM-Session-ID": "sess-owner"}); w.Code != 200 {
			t.Errorf("%s owner: %d", p, w.Code)
		}
		w := doGet(r, u, map[string]string{"X-MM-Session-ID": "stranger"})
		if w.Code != 404 || w.Body.String() != missing.Body.String() {
			t.Errorf("%s stranger: %d %q", p, w.Code, w.Body.String())
		}
	}
	// ?sid only on the file route.
	if w := doGet(r, "/video-restore/"+job.ID+"/result/original?sid=sess-owner", nil); w.Code != 200 {
		t.Errorf("file ?sid: %d", w.Code)
	}
	if w := doGet(r, "/video-restore/"+job.ID+"/results?sid=sess-owner", nil); w.Code != 404 {
		t.Errorf("manifest ?sid must be rejected: %d", w.Code)
	}
}
