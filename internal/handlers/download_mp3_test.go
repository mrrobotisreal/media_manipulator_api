package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/middleware"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
	"github.com/mrrobotisreal/media_manipulator_api/internal/retention"
	"github.com/mrrobotisreal/media_manipulator_api/internal/services"
	"github.com/mrrobotisreal/media_manipulator_api/internal/tiers"
)

// wavStemFixture is a finished remove_vocals job whose WAV result sits on disk.
func wavStemFixture(t *testing.T, format string) (*gin.Engine, *models.ConversionJob, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	resetMP3StatesForTests()
	t.Cleanup(resetMP3StatesForTests)
	dir := t.TempDir()
	jm := services.NewJobManager()
	job := jm.CreateJob(models.OriginalFileInfo{Name: "My Song.mp3", Type: "audio/mpeg"}, map[string]interface{}{
		"format": format, "ai": map[string]interface{}{"enabled": true, "operation": "remove_vocals"},
	})
	h := &ConversionHandler{cfg: &config.Config{OutputDir: dir}, jobManager: jm}
	out := h.outputPath(job, filepath.Join(dir, job.ID))
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(out, []byte("RIFF....WAVEfmt "+strings.Repeat("x", 4000)), 0o600); err != nil {
		t.Fatal(err)
	}
	jm.SetJobOwner(job.ID, services.JobOwner{SessionID: "sess-owner"})
	_ = jm.UpdateJobStatus(job.ID, models.StatusCompleted)
	r := gin.New()
	r.Use(middleware.TierContext(middleware.TierConfig{Store: &jobRecordStore{}, Registry: tiers.New(&config.Config{})}, true))
	RegisterConversionRoutes(r, h)
	return r, job, out
}

// fakeTranscode replaces ffmpeg: it writes a small "MP3" and counts calls.
func fakeTranscode(t *testing.T, fail bool) *int32 {
	t.Helper()
	var calls int32
	prev := transcodeToMP3
	transcodeToMP3 = func(_ context.Context, in, out string) error {
		atomic.AddInt32(&calls, 1)
		if fail {
			return errors.New("boom")
		}
		return os.WriteFile(out, []byte("ID3mp3-bytes"), 0o600)
	}
	t.Cleanup(func() { transcodeToMP3 = prev })
	return &calls
}

func waitMP3Idle(t *testing.T, jobID string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		mp3Mu.Lock()
		st := mp3States[jobID]
		running := st != nil && st.running
		mp3Mu.Unlock()
		if !running {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("transcode never finished")
}

type mp3Resp struct {
	Status    string `json:"status"`
	URL       string `json:"url"`
	SizeBytes int64  `json:"sizeBytes"`
	FileName  string `json:"fileName"`
	Error     string `json:"error"`
}

func decodeMP3(t *testing.T, body []byte) mp3Resp {
	t.Helper()
	var r mp3Resp
	if err := json.Unmarshal(body, &r); err != nil {
		t.Fatalf("body %s: %v", body, err)
	}
	return r
}

// MM81 "Download as MP3": the first POST starts one transcode (202), a repeat
// click gets the cached file as a signed link (no second encode), and that
// link serves derived.mp3 as "<name>.mp3" while the WAV link is unchanged.
func TestResultMP3TranscodeCachedAndServedBySignedLink(t *testing.T) {
	SetResultOwnershipEnforce(true)
	calls := fakeTranscode(t, false)
	ledger := installFakeLedger(t, nil)
	r, job, out := wavStemFixture(t, "wav")
	owner := map[string]string{"X-MM-Session-ID": "sess-owner"}

	if w := doLinkReq(r, http.MethodGet, "/download/"+job.ID+"/mp3", "", owner); w.Code != 200 || decodeMP3(t, w.Body.Bytes()).Status != "none" {
		t.Fatalf("status before start: %d %s", w.Code, w.Body.String())
	}
	w := doLinkReq(r, http.MethodPost, "/download/"+job.ID+"/mp3", "", owner)
	if w.Code != http.StatusAccepted || decodeMP3(t, w.Body.Bytes()).Status != "processing" {
		t.Fatalf("start: %d %s", w.Code, w.Body.String())
	}
	waitMP3Idle(t, job.ID)

	for i := 0; i < 2; i++ {
		w = doLinkReq(r, http.MethodPost, "/download/"+job.ID+"/mp3", "", owner)
		got := decodeMP3(t, w.Body.Bytes())
		if w.Code != 200 || got.Status != "ready" || got.FileName != "My Song_converted.mp3" || got.SizeBytes != int64(len("ID3mp3-bytes")) {
			t.Fatalf("ready #%d: %d %s", i, w.Code, w.Body.String())
		}
		if !strings.HasPrefix(got.URL, "/api/download/"+job.ID+"?token=") || strings.Contains(got.URL, "sess-owner") {
			t.Fatalf("url %q", got.URL)
		}
		dl := doGet(r, strings.TrimPrefix(got.URL, "/api"), nil)
		if dl.Code != 200 || dl.Body.String() != "ID3mp3-bytes" {
			t.Fatalf("download: %d %q", dl.Code, dl.Body.String())
		}
		if cd := dl.Header().Get("Content-Disposition"); !strings.Contains(cd, `"My Song_converted.mp3"`) {
			t.Errorf("disposition %q", cd)
		}
	}
	if n := atomic.LoadInt32(calls); n != 1 {
		t.Fatalf("transcode ran %d times, want 1 (cache)", n)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(out), derivedMP3Temp)); !os.IsNotExist(err) {
		t.Error("the temp file must not be left behind")
	}

	// The plain link still serves the WAV result.
	_, plain := mintLink(t, r, job.ID, "", owner)
	if dl := doGet(r, plain, nil); dl.Code != 200 || !strings.HasPrefix(dl.Body.String(), "RIFF") {
		t.Fatalf("wav link: %d", dl.Code)
	}

	// Retention: one derived_file row, parent = the job's output_dir locator,
	// created on the job's clock so it comes due with the result.
	var found int
	for _, row := range ledger.rows {
		if row.Kind != retention.KindDerivedFile {
			continue
		}
		found++
		if row.Locator != job.ID+"/derived.mp3" || row.ParentLocator != job.ID || row.JobID != job.ID || row.OwnerSession != "sess-owner" {
			t.Errorf("ledger row %+v", row)
		}
		if !row.CreatedAt.Equal(job.CreatedAt.UTC()) {
			t.Errorf("created %v want the job's %v", row.CreatedAt, job.CreatedAt)
		}
	}
	if found != 1 {
		t.Fatalf("derived_file rows = %d", found)
	}
}

func TestResultMP3Refusals(t *testing.T) {
	SetResultOwnershipEnforce(true)
	calls := fakeTranscode(t, false)
	r, job, _ := wavStemFixture(t, "wav")
	for _, tc := range []struct {
		name    string
		method  string
		headers map[string]string
		want    int
	}{
		{"stranger post", http.MethodPost, map[string]string{"X-MM-Session-ID": "stranger"}, http.StatusNotFound},
		{"no credentials", http.MethodPost, nil, http.StatusNotFound},
		{"stranger status", http.MethodGet, map[string]string{"X-MM-Session-ID": "stranger"}, http.StatusNotFound},
	} {
		if w := doLinkReq(r, tc.method, "/download/"+job.ID+"/mp3", "", tc.headers); w.Code != tc.want {
			t.Errorf("%s: %d want %d", tc.name, w.Code, tc.want)
		}
	}
	// A variant token cannot be forged from a plain one: a link minted for the
	// WAV never reaches derived.mp3, and a variant link to a missing file 404s.
	if w := doLinkReq(r, http.MethodGet, "/download/"+job.ID+"?token=garbage", "", nil); w.Code != http.StatusNotFound {
		t.Errorf("garbage token %d", w.Code)
	}

	mp3Job, mp3Router := func() (*models.ConversionJob, *gin.Engine) {
		rr, j, _ := wavStemFixture(t, "mp3")
		return j, rr
	}()
	if w := doLinkReq(mp3Router, http.MethodPost, "/download/"+mp3Job.ID+"/mp3", "", map[string]string{"X-MM-Session-ID": "sess-owner"}); w.Code != http.StatusBadRequest {
		t.Errorf("non-WAV result: %d %s", w.Code, w.Body.String())
	}
	if atomic.LoadInt32(calls) != 0 {
		t.Error("no refusal may start a transcode")
	}
}

func TestResultMP3FailureIsReportedAndRetryable(t *testing.T) {
	SetResultOwnershipEnforce(true)
	fakeTranscode(t, true)
	r, job, out := wavStemFixture(t, "wav")
	owner := map[string]string{"X-MM-Session-ID": "sess-owner"}
	if w := doLinkReq(r, http.MethodPost, "/download/"+job.ID+"/mp3", "", owner); w.Code != http.StatusAccepted {
		t.Fatalf("start %d", w.Code)
	}
	waitMP3Idle(t, job.ID)
	w := doLinkReq(r, http.MethodGet, "/download/"+job.ID+"/mp3", "", owner)
	if got := decodeMP3(t, w.Body.Bytes()); got.Status != "failed" || got.Error == "" {
		t.Fatalf("status after failure: %s", w.Body.String())
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(out), derivedMP3File)); !os.IsNotExist(err) {
		t.Error("a failed encode must leave no cached file")
	}
	calls := fakeTranscode(t, false)
	if w := doLinkReq(r, http.MethodPost, "/download/"+job.ID+"/mp3", "", owner); w.Code != http.StatusAccepted {
		t.Fatalf("retry %d", w.Code)
	}
	waitMP3Idle(t, job.ID)
	if atomic.LoadInt32(calls) != 1 {
		t.Fatal("retry must encode again")
	}
}

func TestSwapExtAndVariantFile(t *testing.T) {
	if swapExt("My Song_converted.wav", ".mp3") != "My Song_converted.mp3" || swapExt(".wav", ".mp3") != "converted.mp3" {
		t.Error("swapExt")
	}
	if _, _, ok := variantFile("/nope/converted.wav", "flac"); ok {
		t.Error("unknown variant must be refused")
	}
	// derived.mp3 must never be mistaken for a job's result after a restart.
	for _, p := range resultFilePrefixes {
		if strings.HasPrefix(derivedMP3File, p+".") {
			t.Fatalf("derived file name collides with result prefix %q", p)
		}
	}
}
