package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/png"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/mrrobotisreal/media_manipulator_api/internal/burst"
	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/lab"
	"github.com/mrrobotisreal/media_manipulator_api/internal/services"
)

type recordingRunner struct {
	labFakeRunner
	mu   sync.Mutex
	reqs []lab.RunRequest
	done chan struct{}
}

func (r *recordingRunner) Run(_ context.Context, req lab.RunRequest) {
	r.mu.Lock()
	r.reqs = append(r.reqs, req)
	r.mu.Unlock()
	r.done <- struct{}{}
}

func pngBytes() []byte {
	var b bytes.Buffer
	_ = png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 8, 8)))
	return b.Bytes()
}

func labMultipart(t *testing.T, requestJSON string, files map[string][]byte) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("request", requestJSON)
	for name, data := range files {
		ext := ".png"
		if !bytes.HasPrefix(data, []byte("\x89PNG")) {
			ext = ".txt" // the type check sniffs content, then falls back to the extension
		}
		fw, _ := mw.CreateFormFile(name, name+ext)
		_, _ = fw.Write(data)
	}
	_ = mw.Close()
	return &buf, mw.FormDataContentType()
}

func labPost(r *gin.Engine, body *bytes.Buffer, ctype, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "/api/lab/jobs", body)
	req.Header.Set("Content-Type", ctype)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// POST /api/lab/jobs: schema validation, the lab job record (tool lab_<model>,
// lab flag), the runner call with defaults applied, fail-closed availability and
// the cap check.
func TestLabCreateJob(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{UploadDir: dir + "/up", OutputDir: dir + "/out", CommandTimeout: 10 * time.Second,
		Lab: config.LabConfig{AllowLocal: true, DailySpendCapUSD: 10, MonthlySpendCapUSD: 100},
		Burst: config.BurstConfig{BootTimeoutSeconds: 600, PriceUSDPerHour: map[string]float64{"g6e.xlarge": 1.86},
			AMIs: map[string]string{"lab-image:x86_64": "ami-1"}}}
	jm := services.NewJobManager()
	runner := &recordingRunner{done: make(chan struct{}, 4)}
	h := NewLabHandler(cfg, jm, services.NewMediaInspector(5*time.Second), runner, nil)
	r, _ := labEngine(t, []string{"mwintrow@creatv.io"}, func(api gin.IRouter, gate *lab.Gate) { RegisterLabRoutes(api, h, gate) })

	// a bad param is a 400 with the field named; nothing runs, nothing is created.
	body, ct := labMultipart(t, `{"model":"codeformer","params":{"fidelity":9}}`, map[string][]byte{"image": pngBytes()})
	if w := labPost(r, body, ct, "mitch"); w.Code != 400 || !strings.Contains(w.Body.String(), `"field":"params"`) {
		t.Fatalf("bad param: %d %s", w.Code, w.Body)
	}
	body, ct = labMultipart(t, `{"model":"nope","params":{}}`, nil)
	if w := labPost(r, body, ct, "mitch"); w.Code != 400 {
		t.Fatalf("unknown model: %d", w.Code)
	}
	body, ct = labMultipart(t, `{"model":"codeformer"}`, nil)
	if w := labPost(r, body, ct, "mitch"); w.Code != 400 || !strings.Contains(w.Body.String(), "Missing input") {
		t.Fatalf("missing input: %d %s", w.Code, w.Body)
	}
	body, ct = labMultipart(t, `{"model":"codeformer"}`, map[string][]byte{"image": []byte("not an image at all")})
	if w := labPost(r, body, ct, "mitch"); w.Code != 400 {
		t.Fatalf("wrong input kind: %d %s", w.Code, w.Body)
	}
	if len(runner.reqs) != 0 {
		t.Fatal("a refused request must not run")
	}

	// the happy path: 202, a lab job, defaults applied, the input on disk.
	body, ct = labMultipart(t, `{"model":"codeformer","params":{"upscale":4}}`, map[string][]byte{"image": pngBytes()})
	w := labPost(r, body, ct, "mitch")
	if w.Code != 202 {
		t.Fatalf("create: %d %s", w.Code, w.Body)
	}
	var out struct{ JobID string }
	_ = json.Unmarshal([]byte(strings.Replace(w.Body.String(), "jobId", "JobID", 1)), &out)
	<-runner.done
	job, err := jm.GetJob(out.JobID)
	if err != nil {
		t.Fatal(err)
	}
	if job.Mode != "lab_codeformer" || job.Options["tool"] != "lab_codeformer" || job.Options["lab"] != true {
		t.Fatalf("job: %+v", job)
	}
	req := runner.reqs[0]
	if req.Params["upscale"] != float64(4) || req.Params["fidelity"] != 0.7 || req.Inputs["image"] == "" || req.Model.Key != "codeformer" {
		t.Fatalf("run request: %+v", req)
	}

	// the cap check refuses with 409 lab_cap before anything is written (burst-only model).
	cfg.Lab.AllowLocal = false
	runner.burst = true
	capped := burst.NewLedger(0.05, 100, nil, nil)
	h.ledger = capped
	body, ct = labMultipart(t, `{"model":"codeformer"}`, map[string][]byte{"image": pngBytes()})
	if w := labPost(r, body, ct, "mitch"); w.Code != 409 || !strings.Contains(w.Body.String(), `"error":"lab_cap"`) || !strings.Contains(w.Body.String(), `"scope":"day"`) {
		t.Fatalf("cap: %d %s", w.Code, w.Body)
	}
	// burst not configured: 503 lab_unavailable (fail closed).
	h.ledger = nil
	runner.burst = false
	body, ct = labMultipart(t, `{"model":"codeformer"}`, map[string][]byte{"image": pngBytes()})
	if w := labPost(r, body, ct, "mitch"); w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "lab_unavailable") {
		t.Fatalf("unavailable: %d %s", w.Code, w.Body)
	}
	// and the spend route reads the Lab ledger.
	h.ledger = burst.NewLedger(10, 100, nil, nil)
	req2 := httptest.NewRequest("GET", "/api/lab/spend", nil)
	req2.Header.Set("Authorization", "Bearer mitch")
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, req2)
	if w2.Code != 200 || !strings.Contains(w2.Body.String(), `"capUsd":10`) || !strings.Contains(w2.Body.String(), `"capUsd":100`) {
		t.Fatalf("spend: %d %s", w2.Code, w2.Body)
	}
}
