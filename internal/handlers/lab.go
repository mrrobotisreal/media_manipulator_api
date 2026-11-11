package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/mrrobotisreal/media_manipulator_api/internal/burst"
	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/lab"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
	"github.com/mrrobotisreal/media_manipulator_api/internal/services"
)

// LabNotice is the permanent personal-use notice (design §2).
const LabNotice = "Non-commercial and research-licence models: personal use only. Don't use these outputs in Media Manipulator or CreaTV marketing, products or client work."

// LabRunner is what the Lab routes need from lab.Runner.
type LabRunner interface {
	Run(ctx context.Context, req lab.RunRequest)
	Sessions() []lab.SessionInfo
	EndSession(ctx context.Context, model string) bool
	BurstAvailable() bool
}

// LabHandler serves /api/lab/*. Every route is mounted behind the Lab gate
// (RegisterLabRoutes), which answers 404 to everyone else.
type LabHandler struct {
	cfg       *config.Config
	jobs      *services.JobManager
	inspector *services.MediaInspector
	runner    LabRunner
	// ledger is the Lab sub-cap ledger (nil = no sub-cap, reads as zero spend).
	ledger *burst.Ledger
	now    func() time.Time
}

// NewLabHandler wires the Lab routes.
func NewLabHandler(cfg *config.Config, jobs *services.JobManager, inspector *services.MediaInspector, runner LabRunner, ledger *burst.Ledger) *LabHandler {
	return &LabHandler{cfg: cfg, jobs: jobs, inspector: inspector, runner: runner, ledger: ledger, now: time.Now}
}

// RegisterLabRoutes mounts the Lab routes on r behind the gate (ticket decision 2).
func RegisterLabRoutes(r gin.IRouter, h *LabHandler, gate *lab.Gate) {
	g := r.Group("/lab")
	g.Use(gate.RequireLab())
	g.GET("/models", h.Models)
	g.POST("/jobs", h.CreateJob)
	g.GET("/spend", h.Spend)
	g.POST("/sessions/:model/end", h.EndSession)
}

// labModelView is a registry row as the UI sees it.
type labModelView struct {
	Key               string          `json:"key"`
	Name              string          `json:"name"`
	Area              string          `json:"area"`
	Licence           string          `json:"licence"`
	LicenceURL        string          `json:"licenceUrl"`
	SourceURL         string          `json:"sourceUrl"`
	Family            string          `json:"family"`
	Instance          string          `json:"instance"`
	FitsHome          bool            `json:"fitsHome"`
	RunsAt            string          `json:"runsAt"`
	EstSeconds        int             `json:"estSeconds"`
	EstCostUSD        float64         `json:"estCostUsd"`
	Available         bool            `json:"available"`
	UnavailableReason string          `json:"unavailableReason,omitempty"`
	ParamsSchema      json.RawMessage `json:"paramsSchema"`
	Inputs            []lab.Input     `json:"inputs"`
	Outputs           []lab.Output    `json:"outputs"`
}

func (h *LabHandler) view(m lab.LabModel) labModelView {
	place := lab.PlacementOf(m, h.cfg.Lab)
	v := labModelView{
		Key: m.Key, Name: m.Name, Area: m.Area, Licence: m.Licence, LicenceURL: m.LicenceURL, SourceURL: m.SourceURL,
		Family: m.Family, FitsHome: m.FitsHome, RunsAt: string(place), EstSeconds: m.EstSeconds,
		EstCostUSD: float64(lab.EstimateMicros(m, h.cfg)) / 1e6, Available: true,
		ParamsSchema: m.ParamsSchema, Inputs: m.Inputs, Outputs: m.Outputs,
	}
	if place == lab.PlaceHome {
		v.Instance = "home GPU"
		return v
	}
	v.Instance = lab.Instance(m, h.cfg.Burst)
	switch {
	case !h.runner.BurstAvailable():
		v.Available, v.UnavailableReason = false, "needs a burst GPU and burst is not configured"
	case h.cfg.Burst.AMIs[m.Family+":x86_64"] == "":
		v.Available, v.UnavailableReason = false, "no worker image for "+m.Family+" in BURST_AMIS"
	}
	return v
}

// Models handles GET /api/lab/models.
func (h *LabHandler) Models(c *gin.Context) {
	var out []labModelView
	for _, m := range lab.Models() {
		out = append(out, h.view(m))
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, gin.H{"notice": LabNotice, "burstConfigured": h.runner.BurstAvailable(), "models": out})
}

type labSpendWindow struct {
	SpentUSD float64 `json:"spentUsd"`
	CapUSD   float64 `json:"capUsd"`
}

// Spend handles GET /api/lab/spend: today and this month against the Lab caps.
func (h *LabHandler) Spend(c *gin.Context) {
	var s burst.Spend
	if h.ledger != nil {
		s = h.ledger.Spent(c.Request.Context())
	} else {
		s = burst.Spend{DayCapMicros: burst.USDToMicros(h.cfg.Lab.DailySpendCapUSD), MonthCapMicros: burst.USDToMicros(h.cfg.Lab.MonthlySpendCapUSD)}
	}
	sessions := []lab.SessionInfo{}
	if h.runner != nil {
		sessions = append(sessions, h.runner.Sessions()...)
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, gin.H{
		"day":      labSpendWindow{SpentUSD: float64(s.DayMicros) / 1e6, CapUSD: float64(s.DayCapMicros) / 1e6},
		"month":    labSpendWindow{SpentUSD: float64(s.MonthMicros) / 1e6, CapUSD: float64(s.MonthCapMicros) / 1e6},
		"sessions": sessions,
	})
}

// EndSession handles POST /api/lab/sessions/:model/end.
func (h *LabHandler) EndSession(c *gin.Context) {
	key := strings.TrimSpace(c.Param("model"))
	if _, ok := lab.Get(key); !ok {
		lab.NotFound(c)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ended": h.runner.EndSession(c.Request.Context(), key)})
}

type labJobRequest struct {
	Model  string                        `json:"model"`
	Params map[string]any                `json:"params"`
	Inputs map[string]labInputFromResult `json:"inputs"`
}

type labInputFromResult struct {
	FromJob string `json:"fromJob"`
}

func labErr(c *gin.Context, status int, code, msg, field string) {
	body := gin.H{"error": code, "message": msg}
	if field != "" {
		body["field"] = field
	}
	c.JSON(status, body)
}

// CreateJob handles POST /api/lab/jobs (multipart: `request` JSON plus one file
// part per upload input). It validates the params against the model's schema,
// checks the spend caps and availability, creates an ordinary job with
// tool = lab_<model> and runs it in the background.
func (h *LabHandler) CreateJob(c *gin.Context) {
	uploadCap := maxUploadBytes(c, h.cfg)
	if uploadCap > 0 {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, uploadCap)
	}
	if err := c.Request.ParseMultipartForm(32 << 20); err != nil {
		labErr(c, http.StatusRequestEntityTooLarge, "bad_request", "The upload is too large or the form is malformed", "")
		return
	}
	var req labJobRequest
	if err := json.Unmarshal([]byte(c.Request.FormValue("request")), &req); err != nil {
		labErr(c, http.StatusBadRequest, "bad_request", "The request field is missing or not JSON", "request")
		return
	}
	m, ok := lab.Get(strings.TrimSpace(req.Model))
	if !ok {
		labErr(c, http.StatusBadRequest, "bad_request", "Unknown model", "model")
		return
	}
	params, err := lab.ValidateParams(m, req.Params)
	if err != nil {
		labErr(c, http.StatusBadRequest, "bad_request", err.Error(), "params")
		return
	}

	// MM139 decision 5: any reference voice needs the consent confirmation, stored on the job.
	if m.RequiresConsent() {
		if ok, _ := params["consent"].(bool); !ok {
			labErr(c, http.StatusBadRequest, "consent_required", "Confirm that this is your voice, or that you have the explicit permission of the person whose voice it is", "consent")
			return
		}
	}

	// Availability and caps come before any file is written.
	v := h.view(m)
	if !v.Available {
		labErr(c, http.StatusServiceUnavailable, "lab_unavailable", "This model is not available: "+v.UnavailableReason, "")
		return
	}
	if scope, msg := h.capCheck(c.Request.Context(), lab.EstimateMicros(m, h.cfg)); scope != "" {
		c.JSON(http.StatusConflict, gin.H{"error": "lab_cap", "message": msg, "scope": scope})
		return
	}

	jobID := newJobID()
	jobUploadDir := filepath.Join(h.cfg.UploadDir, jobID)
	if err := os.MkdirAll(jobUploadDir, 0o755); err != nil {
		labErr(c, http.StatusInternalServerError, "server_error", "Failed to prepare the upload", "")
		return
	}
	fail := func(status int, code, msg, field string) {
		_ = os.RemoveAll(jobUploadDir)
		labErr(c, status, code, msg, field)
	}
	inputs := map[string]string{}
	firstName, firstSize, firstType := "", int64(0), ""
	for _, in := range m.Inputs {
		var path string
		if from, has := req.Inputs[in.Name]; has && strings.TrimSpace(from.FromJob) != "" {
			p, status, msg := h.resolveFromJob(c, strings.TrimSpace(from.FromJob))
			if p == "" {
				fail(status, "bad_request", msg, in.Name)
				return
			}
			dst := filepath.Join(jobUploadDir, in.Name+"_"+safeFilename(filepath.Base(p)))
			if err := copyFile(p, dst); err != nil {
				fail(http.StatusInternalServerError, "server_error", "Failed to copy the previous result", in.Name)
				return
			}
			path = dst
		} else if file, hdr, ferr := c.Request.FormFile(in.Name); ferr == nil {
			dst := filepath.Join(jobUploadDir, in.Name+"_"+safeFilename(hdr.Filename))
			err := saveMultipartTo(file, dst)
			_ = file.Close()
			if err != nil {
				fail(http.StatusInternalServerError, "server_error", "Failed to save the upload", in.Name)
				return
			}
			path = dst
			if firstName == "" {
				firstName, firstSize = hdr.Filename, hdr.Size
			}
		} else if in.Required {
			fail(http.StatusBadRequest, "bad_request", "Missing input: "+in.Label, in.Name)
			return
		} else {
			continue
		}
		ctx, cancel := context.WithTimeout(c.Request.Context(), h.cfg.CommandTimeout)
		ft, mime := h.inspector.DetectFile(ctx, path, "")
		cancel()
		if string(ft) != in.Kind {
			fail(http.StatusBadRequest, "bad_request", fmt.Sprintf("%s must be %s", in.Label, in.Kind), in.Name)
			return
		}
		if firstType == "" {
			firstType = mime
		}
		inputs[in.Name] = path
	}
	if firstName == "" {
		firstName = filepath.Base(inputs[m.Inputs[0].Name])
	}

	options := map[string]interface{}{"mode": m.Tool(), "tool": m.Tool(), "lab": true, "model": m.Key}
	if m.RequiresConsent() {
		options["consent"] = true
		options["consentAt"] = h.now().UTC().Format(time.RFC3339)
		options["consentTextVersion"] = lab.VoiceConsentVersion
	}
	job := h.jobs.CreateJobWithID(jobID, models.OriginalFileInfo{Name: safeFilename(firstName), Size: firstSize, Type: firstType}, options)
	_ = h.jobs.SetMode(job.ID, m.Tool())
	recordJobCreated(c, h.jobs, job, services.JobFacts{
		Tool: m.Tool(), Mode: m.Tool(), MediaKind: mediaKindOfLab(m), SourceFormat: formatOf(firstName), TargetFormat: strings.TrimPrefix(filepath.Ext(m.ResultFile), "."),
	})
	registerJobDirs(c, job.ID, true, true)

	go h.runner.Run(context.Background(), lab.RunRequest{
		JobID: job.ID, RequestID: c.Writer.Header().Get("X-MM-Request-ID"), Model: m, Params: params, Inputs: inputs,
	})
	c.JSON(http.StatusAccepted, gin.H{"jobId": job.ID})
}

func mediaKindOfLab(m lab.LabModel) string {
	if len(m.Inputs) > 0 {
		return m.Inputs[0].Kind
	}
	return "unknown"
}

// capCheck reports the first Lab cap the estimate would cross ("day"/"month"),
// from the ledger's settled spend plus live reservations. The ledger's Reserve at
// launch is the authoritative check; this one gives the UI a clear answer first.
func (h *LabHandler) capCheck(ctx context.Context, estMicros int64) (scope, msg string) {
	if h.ledger == nil || estMicros <= 0 {
		return "", ""
	}
	s := h.ledger.Spent(ctx)
	if s.DayCapMicros > 0 && s.DayMicros+estMicros > s.DayCapMicros {
		return "day", fmt.Sprintf("This run (about $%.2f) would pass today's Lab cap of $%.2f ($%.2f spent)", float64(estMicros)/1e6, float64(s.DayCapMicros)/1e6, float64(s.DayMicros)/1e6)
	}
	if s.MonthCapMicros > 0 && s.MonthMicros+estMicros > s.MonthCapMicros {
		return "month", fmt.Sprintf("This run (about $%.2f) would pass this month's Lab cap of $%.2f ($%.2f spent)", float64(estMicros)/1e6, float64(s.MonthCapMicros)/1e6, float64(s.MonthMicros)/1e6)
	}
	return "", ""
}

// resolveFromJob returns the result file of a finished job the caller may read.
func (h *LabHandler) resolveFromJob(c *gin.Context, jobID string) (path string, status int, msg string) {
	job, err := h.jobs.GetJob(jobID)
	if err != nil || !authorizeJobAccess(c, h.jobs, job) {
		return "", http.StatusBadRequest, "Unknown previous job"
	}
	if job.Status != models.StatusCompleted {
		return "", http.StatusBadRequest, "That job has not finished"
	}
	p := findResultFile(h.cfg.OutputDir, jobID)
	if name, ok := labResultFile(job); ok {
		p = filepath.Join(h.cfg.OutputDir, jobID, name)
	}
	if st, err := os.Stat(p); p == "" || err != nil || st.IsDir() {
		return "", http.StatusBadRequest, "That job's result is no longer available"
	}
	return p, 0, ""
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	return saveMultipartTo(in, dst)
}
