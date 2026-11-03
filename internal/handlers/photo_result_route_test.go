package handlers

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/middleware"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
	"github.com/mrrobotisreal/media_manipulator_api/internal/services"
	"github.com/mrrobotisreal/media_manipulator_api/internal/tiers"
)

// Regression (found in the 2026-11-03 live smoke test): a completed stateless
// heal reported resultUrl /api/download/<id> but both /api/result/<id> and
// /api/download/<id> answered 404, because outputPath looked for
// converted<ext> while the photo worker writes its recorded ResultFileName.
func TestPhotoJobResultsServedByResultAndDownload(t *testing.T) {
	cases := []struct {
		tool, mode, format, name, mime string
	}{
		{PhotoToolHeal, "heal", "png", "healed.png", "image/png"},
		{PhotoToolExport, "jpg", "jpg", "Beach day (retouched).jpg", "image/jpeg"},
	}
	for _, tc := range cases {
		t.Run(tc.tool, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			dir := t.TempDir()
			jm := services.NewJobManager()
			job := jm.CreateJob(models.OriginalFileInfo{Name: "photo.png", Type: "image/png"},
				map[string]interface{}{"tool": tc.tool, "mode": tc.mode, "format": tc.format})
			_ = jm.SetResultMetadata(job.ID, "", tc.name, time.Time{})
			_ = jm.SetResultMime(job.ID, tc.mime)
			if err := os.MkdirAll(filepath.Join(dir, job.ID), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, job.ID, tc.name), []byte("PIXELS"), 0o600); err != nil {
				t.Fatal(err)
			}
			jm.SetJobOwner(job.ID, services.JobOwner{SessionID: "s-photo"})
			_ = jm.UpdateJobStatus(job.ID, models.StatusCompleted)
			h := &ConversionHandler{cfg: &config.Config{OutputDir: dir}, jobManager: jm}
			r := gin.New()
			r.Use(middleware.TierContext(middleware.TierConfig{Store: &jobRecordStore{}, Registry: tiers.New(&config.Config{})}, true))
			RegisterConversionRoutes(r, h)
			hdr := map[string]string{"X-MM-Session-ID": "s-photo"}
			for _, route := range []string{"/result/", "/download/"} {
				w := doLinkReq(r, http.MethodGet, route+job.ID, "", hdr)
				if w.Code != http.StatusOK || w.Body.String() != "PIXELS" {
					t.Fatalf("%s = %d %q", route, w.Code, w.Body.String())
				}
			}
			cur, _ := jm.GetJob(job.ID)
			if got := h.getOutputFilename(cur); got != tc.name {
				t.Fatalf("download name %q, want %q", got, tc.name)
			}
		})
	}
	// A recorded name that is not one safe segment is ignored.
	for _, bad := range []string{"../x.png", "a/b.png", ""} {
		j := &models.ConversionJob{ResultFileName: bad, Options: map[string]interface{}{"tool": PhotoToolHeal}}
		if _, ok := photoJobResultName(j); ok {
			t.Fatalf("%q accepted", bad)
		}
	}
	if _, ok := photoJobResultName(&models.ConversionJob{ResultFileName: "x.png", Options: map[string]interface{}{"tool": "convert"}}); ok {
		t.Fatal("non-photo job accepted")
	}
}
