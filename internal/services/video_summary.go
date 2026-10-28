package services

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mrrobotisreal/media_manipulator_api/internal/gpu"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
	"github.com/mrrobotisreal/media_manipulator_api/internal/validation"
)

// VideoSummaryService runs the staged video summary job (MM74, design
// docs/design/VIDEO_SUMMARY.md §3, ADR-0014):
//
//	probe → audio (Whisper, TaskWhisper lease) ∥ frames (scene-aware sampling)
//	      → vision (VLM over batches of 4 frames + their transcript window, TaskVLM lease)
//	      → synthesis (one text-model call, schema-validated with one repair retry)
//	      → render (summary.json + summary.md in the job output dir)
//
// Synthesis starts strictly after both audio and frames have finished (and
// after vision, which needs the transcript), so the text model never competes
// with Whisper or the VLM for VRAM. Jobs are serialised through a
// SUMMARY_WORKERS queue (default 1) exactly like ANALYSIS_WORKERS, so two
// summaries never load models at the same time.
type VideoSummaryService struct {
	settings   VideoSummarySettings
	jobManager *JobManager
	deps       videoSummaryDeps

	queue   chan VideoSummaryRequest
	startMu sync.Once
	waitMu  sync.Mutex
	waiting []string
	running atomic.Int32
}

// VideoSummarySettings is the service's slice of configuration
// (config.Config → handlers wiring).
type VideoSummarySettings struct {
	// OllamaURL serves the VLM and the standard summary model (OLLAMA_URL).
	OllamaURL    string
	VLMModel     string
	SummaryModel string
	// DeepModel/DeepURL: deep synthesis (OLLAMA_SUMMARY_DEEP_MODEL/_URL). Both
	// must be set for deep to exist.
	DeepModel      string
	DeepURL        string
	MaxInputTokens int
	Workers        int
	// StageTimeout bounds each heavy stage (frames, audio, vision, synthesis).
	StageTimeout time.Duration
	// JobTimeout bounds the whole job once it leaves the queue.
	JobTimeout time.Duration
	// OllamaTimeout is the HTTP client timeout for one Ollama call.
	OllamaTimeout time.Duration
	// FrameWidth is the width frames are scaled to for the VLM (768).
	FrameWidth int
}

// DeepAvailable reports whether deep synthesis is configured.
func (s VideoSummarySettings) DeepAvailable() bool {
	return strings.TrimSpace(s.DeepModel) != "" && strings.TrimSpace(s.DeepURL) != ""
}

// VideoSummaryRequest is one accepted job.
type VideoSummaryRequest struct {
	JobID     string
	InputPath string
	OutputDir string
	// OutputLanguage is "auto" or a registered locale code.
	OutputLanguage string
	// Depth is the EFFECTIVE depth (already downgraded by the handler).
	Depth string
	// MaxFrames is the caller's tier frame budget (> 0).
	MaxFrames   int
	RequestID   string
	GPUPriority int
	// Probe is the handler's ffprobe report (it already probed to refuse an
	// unusable file before the job existed). nil makes the probe stage run it.
	Probe *models.VideoProbeResponse
	// UploadDir, when set, is the job's staged-upload directory, removed when
	// the job ends (the frames, transcript and summary stay in OutputDir).
	UploadDir string
}

// summaryLease / summaryLeaser abstract gpu.Manager so the stage-ordering test
// can observe leases.
type summaryLease interface {
	Release(ctx context.Context, err error)
}

type summaryLeaser interface {
	Acquire(ctx context.Context, task gpu.TaskType, tool, jobID, requestID string) (summaryLease, error)
}

type gpuManagerLeaser struct{ m *gpu.Manager }

func (g gpuManagerLeaser) Acquire(ctx context.Context, task gpu.TaskType, tool, jobID, requestID string) (summaryLease, error) {
	l, err := g.m.Acquire(ctx, task, tool, jobID, requestID)
	if err != nil || l == nil {
		return nil, err
	}
	return l, nil
}

// videoSummaryDeps are the side-effecting steps, injectable for tests (the
// MacBook has no ffmpeg/Whisper/Ollama).
type videoSummaryDeps struct {
	probe         func(ctx context.Context, path string) (*models.VideoProbeResponse, error)
	detectScenes  func(ctx context.Context, path string, threshold float64) ([]float64, error)
	extractFrames func(ctx context.Context, path, framesDir string, times []float64, width int) ([]Frame, error)
	// transcribe extracts the audio and runs Whisper (device chosen by the
	// shared GPU scheduler); the TaskWhisper lease is taken by the pipeline.
	transcribe func(ctx context.Context, req VideoSummaryRequest, workDir string) (models.VideoSummaryTranscript, error)
	leaser     summaryLeaser
	httpClient *http.Client
}

// NewVideoSummaryService wires the production dependencies. gpuMgr may be nil.
func NewVideoSummaryService(settings VideoSummarySettings, jm *JobManager, gpuMgr *gpu.Manager) *VideoSummaryService {
	settings = settings.withDefaults()
	s := &VideoSummaryService{
		settings:   settings,
		jobManager: jm,
		queue:      make(chan VideoSummaryRequest, settings.Workers*8),
	}
	s.deps = videoSummaryDeps{
		probe:         ProbeVideoReport,
		detectScenes:  detectSceneTimes,
		extractFrames: extractFramesAt,
		transcribe:    s.transcribeWithWhisper,
		leaser:        gpuManagerLeaser{m: gpuMgr},
		httpClient:    &http.Client{Timeout: settings.OllamaTimeout},
	}
	return s
}

func (s VideoSummarySettings) withDefaults() VideoSummarySettings {
	if strings.TrimSpace(s.OllamaURL) == "" {
		s.OllamaURL = "http://localhost:11434"
	}
	if strings.TrimSpace(s.VLMModel) == "" {
		s.VLMModel = defaultVLMModel
	}
	if strings.TrimSpace(s.SummaryModel) == "" {
		s.SummaryModel = DefaultSummaryModel
	}
	if s.MaxInputTokens <= 0 {
		s.MaxInputTokens = DefaultSummaryMaxInputTokens
	}
	if s.Workers <= 0 {
		s.Workers = 1
	}
	if s.StageTimeout <= 0 {
		s.StageTimeout = 30 * time.Minute
	}
	if s.JobTimeout <= 0 {
		s.JobTimeout = 2 * time.Hour
	}
	if s.OllamaTimeout <= 0 {
		s.OllamaTimeout = 300 * time.Second
	}
	if s.FrameWidth <= 0 {
		s.FrameWidth = 768
	}
	return s
}

// Defaults shared with config (design §1).
const (
	DefaultSummaryModel          = "qwen3.5:9b-q8_0"
	DefaultSummaryMaxInputTokens = 12000
)

// Settings returns the effective settings (for capabilities).
func (s *VideoSummaryService) Settings() VideoSummarySettings { return s.settings }

// ---------------------------------------------------------------------------
// Queue (SUMMARY_WORKERS)
// ---------------------------------------------------------------------------

// Start launches the workers. Safe to call more than once.
func (s *VideoSummaryService) Start() {
	s.startMu.Do(func() {
		for i := 0; i < s.settings.Workers; i++ {
			go s.worker()
		}
	})
}

// Enqueue hands a job to the serial queue. The job shows "queued" until a
// worker takes it; the probe stage's message says how many are ahead.
func (s *VideoSummaryService) Enqueue(req VideoSummaryRequest) {
	s.waitMu.Lock()
	s.waiting = append(s.waiting, req.JobID)
	s.waitMu.Unlock()
	s.refreshQueueMessages()
	select {
	case s.queue <- req:
	default:
		go func() { s.queue <- req }()
	}
}

func (s *VideoSummaryService) worker() {
	for req := range s.queue {
		s.waitMu.Lock()
		for i, id := range s.waiting {
			if id == req.JobID {
				s.waiting = append(s.waiting[:i], s.waiting[i+1:]...)
				break
			}
		}
		s.waitMu.Unlock()
		s.running.Add(1)
		s.refreshQueueMessages()
		s.Process(context.Background(), req)
		s.running.Add(-1)
		s.refreshQueueMessages()
	}
}

// refreshQueueMessages updates the probe-stage message of every waiting job.
// A waiting job has not started, so its stage list is rebuilt from the initial
// one rather than read from the live job (no shared-pointer reads).
func (s *VideoSummaryService) refreshQueueMessages() {
	s.waitMu.Lock()
	waiting := append([]string(nil), s.waiting...)
	s.waitMu.Unlock()
	running := int(s.running.Load())
	for i, id := range waiting {
		ahead := i + running
		msg := "Waiting in the queue"
		if ahead == 1 {
			msg = "Waiting in the queue: 1 summary ahead"
		} else if ahead > 1 {
			msg = fmt.Sprintf("Waiting in the queue: %d summaries ahead", ahead)
		}
		stages := updateStage(BuildVideoSummaryStages(), SummaryStageProbe, models.StageStatusPending, 0, msg)
		_ = s.jobManager.ReplaceStages(id, stages, "queued")
	}
}

// ---------------------------------------------------------------------------
// Stages
// ---------------------------------------------------------------------------

// Video summary stage keys, in report order.
const (
	SummaryStageProbe     = "probe"
	SummaryStageAudio     = "audio"
	SummaryStageFrames    = "frames"
	SummaryStageVision    = "vision"
	SummaryStageSynthesis = "synthesis"
	SummaryStageRender    = "render"
)

type summaryStageDef struct {
	key    string
	label  string
	weight int
}

var summaryStageDefs = []summaryStageDef{
	{SummaryStageProbe, "Reading the file", 5},
	{SummaryStageAudio, "Transcribing speech", 20},
	{SummaryStageFrames, "Choosing frames", 15},
	{SummaryStageVision, "Describing frames", 30},
	{SummaryStageSynthesis, "Writing the summary", 25},
	{SummaryStageRender, "Formatting the result", 5},
}

// BuildVideoSummaryStages is the initial stage list: six pending stages whose
// Progress is the cumulative checkpoint the job reaches when that stage ends.
func BuildVideoSummaryStages() []models.TranscodeJobStage {
	out := make([]models.TranscodeJobStage, 0, len(summaryStageDefs))
	cum := 0
	for _, d := range summaryStageDefs {
		cum += d.weight
		out = append(out, models.TranscodeJobStage{Key: d.key, Label: d.label, Status: models.StageStatusPending, Progress: cum})
	}
	return out
}

// summaryStages serialises stage updates from the parallel audio/frames
// goroutines and publishes them to the job.
type summaryStages struct {
	mu      sync.Mutex
	jobID   string
	jm      *JobManager
	stages  []models.TranscodeJobStage
	partial map[string]float64 // fraction of a processing stage done
	// trace, when set, receives "start:<key>" / "done:<key>" / "skip:<key>" /
	// "fail:<key>" (tests).
	trace func(string)
}

func newSummaryStages(jobID string, jm *JobManager, trace func(string)) *summaryStages {
	return &summaryStages{jobID: jobID, jm: jm, stages: BuildVideoSummaryStages(), partial: map[string]float64{}, trace: trace}
}

func (st *summaryStages) set(key string, status models.TranscodeStageStatus, message string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.stages = updateStage(st.stages, key, status, 0, message)
	if status != models.StageStatusProcessing {
		delete(st.partial, key)
	}
	if st.trace != nil {
		verb := map[models.TranscodeStageStatus]string{
			models.StageStatusProcessing: "start", models.StageStatusCompleted: "done",
			models.StageStatusSkipped: "skip", models.StageStatusFailed: "fail",
		}[status]
		if verb != "" {
			st.trace(verb + ":" + key)
		}
	}
	st.publishLocked()
}

func (st *summaryStages) progress(key string, fraction float64, message string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if fraction < 0 {
		fraction = 0
	}
	if fraction > 1 {
		fraction = 1
	}
	st.partial[key] = fraction
	if message != "" {
		st.stages = updateStage(st.stages, key, models.StageStatusProcessing, 0, message)
	}
	st.publishLocked()
}

func (st *summaryStages) publishLocked() {
	if st.jm == nil {
		return
	}
	current := ""
	done := 0.0
	for i, s := range st.stages {
		w := float64(summaryStageDefs[i].weight)
		switch s.Status {
		case models.StageStatusCompleted, models.StageStatusSkipped:
			done += w
		case models.StageStatusProcessing:
			done += w * st.partial[s.Key]
			if current == "" {
				current = s.Key
			}
		}
	}
	if current == "" {
		for _, s := range st.stages {
			if s.Status == models.StageStatusPending {
				current = s.Key
				break
			}
		}
	}
	snapshot := append([]models.TranscodeJobStage(nil), st.stages...)
	_ = st.jm.ReplaceStages(st.jobID, snapshot, current)
	_ = st.jm.UpdateJobProgress(st.jobID, int(done))
}

// failProcessing marks every processing stage failed with userMsg.
func (st *summaryStages) failProcessing(userMsg string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	for _, s := range st.stages {
		if s.Status == models.StageStatusProcessing && st.trace != nil {
			st.trace("fail:" + s.Key)
		}
	}
	st.stages = updateStageStatus(st.stages, "", models.StageStatusFailed, userMsg)
	snapshot := append([]models.TranscodeJobStage(nil), st.stages...)
	if st.jm != nil {
		_ = st.jm.ReplaceStages(st.jobID, snapshot, "failed")
	}
}

// ---------------------------------------------------------------------------
// Pipeline
// ---------------------------------------------------------------------------

// Caveats the pipeline adds (the model may add its own).
const (
	CaveatAudioOnly         = "Audio only: there is no picture, so the summary is written from the speech."
	CaveatNoAudioTrack      = "No audio track: the summary is written from the picture only."
	CaveatNoSpeech          = "No speech detected: the summary is written from the picture only."
	CaveatMachineTranscript = "The transcript is machine-generated and may contain mistakes."
	CaveatFramesUnavailable = "The picture could not be sampled, so the summary is written from the speech."
	CaveatSomeFramesSkipped = "Some frames could not be described and were left out."
)

// summaryArtifacts are the files a finished job leaves in its output dir.
const (
	SummaryMarkdownFile   = "summary.md"
	SummaryJSONFile       = "summary.json"
	SummaryTranscriptFile = "transcript.json"
	SummaryVisionFile     = "vision_notes.json"
	SummaryFramesDir      = "frames"
)

// errSummaryUser is an error whose message is safe to show the user.
type errSummaryUser struct{ msg string }

func (e errSummaryUser) Error() string { return e.msg }

func userErr(format string, a ...any) error { return errSummaryUser{msg: fmt.Sprintf(format, a...)} }

// summaryRun is the mutable state of one job.
type summaryRun struct {
	req        VideoSummaryRequest
	st         *summaryStages
	duration   float64
	hasAudio   bool
	hasVideo   bool
	transcript models.VideoSummaryTranscript
	frames     []Frame
	notes      []models.VideoSummaryBatchNote
	caveats    []string
	trace      func(string)
}

func (r *summaryRun) addCaveat(c string) {
	for _, have := range r.caveats {
		if have == c {
			return
		}
	}
	r.caveats = append(r.caveats, c)
}

// Process runs one job end to end. On any failure the job is marked failed
// with a user-safe message, which refunds its ai_ops unit (MM73).
func (s *VideoSummaryService) Process(ctx context.Context, req VideoSummaryRequest) {
	s.process(ctx, req, nil)
}

func (s *VideoSummaryService) process(ctx context.Context, req VideoSummaryRequest, trace func(string)) {
	ctx, cancel := context.WithTimeout(ctx, s.settings.JobTimeout)
	defer cancel()
	if req.UploadDir != "" {
		defer func() { _ = os.RemoveAll(req.UploadDir) }()
	}
	run := &summaryRun{req: req, st: newSummaryStages(req.JobID, s.jobManager, trace), trace: trace}
	if err := s.jobManager.UpdateJobStatus(req.JobID, models.StatusProcessing); err != nil {
		return
	}
	if err := os.MkdirAll(req.OutputDir, 0o755); err != nil {
		s.fail(run, "Failed to prepare the job workspace", err)
		return
	}
	for _, step := range []func(context.Context, *summaryRun) error{
		s.stageProbe,
		s.stagesAudioAndFrames,
		s.stageVision,
		s.stageSynthesisAndRender,
	} {
		if err := step(ctx, run); err != nil {
			msg := "The summary could not be produced"
			var ue errSummaryUser
			if errors.As(err, &ue) {
				msg = ue.msg
			} else if ctx.Err() != nil {
				msg = "The summary took too long and was stopped"
			}
			s.fail(run, msg, err)
			return
		}
	}
}

func (s *VideoSummaryService) fail(run *summaryRun, userMsg string, err error) {
	if err != nil {
		log.Printf("video-summary job %s: %s: %v", run.req.JobID, userMsg, err)
	} else {
		log.Printf("video-summary job %s: %s", run.req.JobID, userMsg)
	}
	run.st.failProcessing(userMsg)
	_ = s.jobManager.UpdateJobError(run.req.JobID, userMsg)
}

// UsableVideoStream reports whether a probe found a real moving picture:
// cover art (attached_pic, already skipped by the probe) and still-image codecs
// do not count (MM73's rule).
func UsableVideoStream(report *models.VideoProbeResponse) bool {
	if report == nil || report.Width <= 0 || report.Height <= 0 {
		return false
	}
	return strings.TrimSpace(report.VideoCodec) != "" && !validation.IsStillImageCodec(report.VideoCodec)
}

func (s *VideoSummaryService) stageProbe(ctx context.Context, run *summaryRun) error {
	run.st.set(SummaryStageProbe, models.StageStatusProcessing, "")
	report := run.req.Probe
	if report == nil {
		var err error
		if report, err = s.deps.probe(ctx, run.req.InputPath); err != nil {
			return userErrWrap("This file could not be read", err)
		}
	}
	run.hasAudio = report.HasAudio
	run.hasVideo = UsableVideoStream(report)
	run.duration = report.DurationSeconds
	if !run.hasAudio && !run.hasVideo {
		return userErr("This file has no audio or video to summarize")
	}
	if run.hasVideo && run.duration <= 0 {
		return userErr("The length of this video could not be read")
	}
	_ = s.jobManager.SetTranscodeReport(run.req.JobID, report)
	if !run.hasVideo {
		run.addCaveat(CaveatAudioOnly)
	}
	run.st.set(SummaryStageProbe, models.StageStatusCompleted, "")
	return nil
}

type wrappedUserErr struct {
	errSummaryUser
	cause error
}

func (w wrappedUserErr) Unwrap() []error { return []error{w.errSummaryUser, w.cause} }

func userErrWrap(msg string, cause error) error {
	return wrappedUserErr{errSummaryUser: errSummaryUser{msg: msg}, cause: cause}
}

// stagesAudioAndFrames runs audio and frames in parallel and returns when both
// have finished. Synthesis cannot start before this returns.
func (s *VideoSummaryService) stagesAudioAndFrames(ctx context.Context, run *summaryRun) error {
	var wg sync.WaitGroup
	var audioErr error
	if run.hasAudio {
		wg.Add(1)
		go func() {
			defer wg.Done()
			audioErr = s.stageAudio(ctx, run)
		}()
	} else {
		run.st.set(SummaryStageAudio, models.StageStatusSkipped, "No audio track")
		run.addCaveat(CaveatNoAudioTrack)
	}

	var framesErr error
	if run.hasVideo {
		framesErr = s.stageFrames(ctx, run)
	} else {
		run.st.set(SummaryStageFrames, models.StageStatusSkipped, "Audio only")
		run.st.set(SummaryStageVision, models.StageStatusSkipped, "Audio only")
	}
	wg.Wait()
	if audioErr != nil {
		return audioErr
	}
	if framesErr != nil {
		return framesErr
	}

	hasSpeech := run.transcript.HasSpeech()
	if run.hasAudio && !hasSpeech {
		run.addCaveat(CaveatNoSpeech)
	}
	if hasSpeech {
		run.addCaveat(CaveatMachineTranscript)
	}
	if !hasSpeech && len(run.frames) == 0 {
		return userErr("Nothing to summarize: no speech was found and there is no picture")
	}
	return nil
}

func (s *VideoSummaryService) stageAudio(ctx context.Context, run *summaryRun) error {
	run.st.set(SummaryStageAudio, models.StageStatusProcessing, "")
	stageCtx, cancel := context.WithTimeout(ctx, s.settings.StageTimeout)
	defer cancel()
	lease, _ := s.deps.leaser.Acquire(stageCtx, gpu.TaskWhisper, models.VideoSummaryTool, run.req.JobID, run.req.RequestID)
	transcript, err := s.deps.transcribe(stageCtx, run.req, run.req.OutputDir)
	if lease != nil {
		lease.Release(context.Background(), err)
	}
	if err != nil {
		if stageCtx.Err() != nil {
			return userErrWrap("Transcribing the speech took too long", err)
		}
		return userErrWrap("The speech could not be transcribed", err)
	}
	run.transcript = transcript
	if err := writeJSON(filepath.Join(run.req.OutputDir, SummaryTranscriptFile), transcript); err != nil {
		return userErrWrap("Failed to save the transcript", err)
	}
	msg := fmt.Sprintf("%d segments", len(transcript.Segments))
	if !transcript.HasSpeech() {
		msg = "No speech detected"
	}
	run.st.set(SummaryStageAudio, models.StageStatusCompleted, msg)
	return nil
}

// stageFrames detects cuts, plans the frames and extracts them. A failed
// scene pass falls back to the even interval; if no frame can be extracted at
// all the job continues from the speech alone (with a caveat) when there is
// audio, and fails otherwise.
func (s *VideoSummaryService) stageFrames(ctx context.Context, run *summaryRun) error {
	run.st.set(SummaryStageFrames, models.StageStatusProcessing, "")
	stageCtx, cancel := context.WithTimeout(ctx, s.settings.StageTimeout)
	defer cancel()

	scenes, err := s.deps.detectScenes(stageCtx, run.req.InputPath, summarySceneThreshold)
	if err != nil {
		if stageCtx.Err() != nil {
			return userErrWrap("Choosing frames took too long", err)
		}
		log.Printf("video-summary job %s: scene detection failed, using the even interval: %v", run.req.JobID, err)
		scenes = nil
	}
	run.st.progress(SummaryStageFrames, 0.5, "")
	times := PlanFrames(run.duration, scenes, run.req.MaxFrames)
	frames, err := s.deps.extractFrames(stageCtx, run.req.InputPath, filepath.Join(run.req.OutputDir, SummaryFramesDir), times, s.settings.FrameWidth)
	if err != nil && stageCtx.Err() != nil {
		return userErrWrap("Choosing frames took too long", err)
	}
	if len(frames) == 0 {
		if !run.hasAudio {
			return userErrWrap("No frames could be read from this video", err)
		}
		log.Printf("video-summary job %s: no frames extracted, continuing from the speech: %v", run.req.JobID, err)
		run.addCaveat(CaveatFramesUnavailable)
		run.st.set(SummaryStageFrames, models.StageStatusFailed, "Frames unavailable")
		run.st.set(SummaryStageVision, models.StageStatusSkipped, "No frames")
		return nil
	}
	run.frames = frames
	run.st.set(SummaryStageFrames, models.StageStatusCompleted, fmt.Sprintf("%d frames", len(frames)))
	return nil
}

// visionSystemPrompt asks for one JSON note per batch.
const visionSystemPrompt = `You describe sampled frames of a video for someone who will write a summary of it. Reply with ONE JSON object and nothing else, with exactly these keys:
{"scene": string, "onScreenText": [string], "people": [string], "objects": [string], "actions": [string], "cameraNotes": string}
- scene: one or two sentences on the setting and what happens across these frames.
- onScreenText: every legible piece of on-screen text (titles, captions, slides, signs, labels), copied exactly; [] if none.
- people: short descriptions of the people visible; never guess who they are unless a name is written on screen.
- objects: notable objects.
- actions: what people or things are doing.
- cameraNotes: framing, camera movement, cuts or visual style; "" if unremarkable.
Describe only what is visible. The speech lines are context for what is happening; do not repeat them. Write in English.`

func visionUserPrompt(batch FrameBatch, window []models.VideoSummaryTranscriptSegment) string {
	var b strings.Builder
	times := make([]string, 0, len(batch.Frames))
	for _, f := range batch.Frames {
		times = append(times, FormatClock(f.TimeSec))
	}
	fmt.Fprintf(&b, "These %d frames are from %s, in this order.\n", len(batch.Frames), strings.Join(times, ", "))
	if len(window) == 0 {
		fmt.Fprintf(&b, "No speech between %s and %s.\n", FormatClock(batch.Start), FormatClock(batch.End))
	} else {
		fmt.Fprintf(&b, "Speech between %s and %s (machine transcript):\n", FormatClock(batch.Start), FormatClock(batch.End))
		b.WriteString(formatTranscriptLines(window))
	}
	return b.String()
}

// stageVision describes the frames in batches of four. A batch whose reply is
// still invalid after the repair retry is left out (with a caveat); the stage
// fails only when no batch could be described.
func (s *VideoSummaryService) stageVision(ctx context.Context, run *summaryRun) error {
	if len(run.frames) == 0 {
		return nil
	}
	run.st.set(SummaryStageVision, models.StageStatusProcessing, "")
	stageCtx, cancel := context.WithTimeout(ctx, s.settings.StageTimeout)
	defer cancel()
	lease, _ := s.deps.leaser.Acquire(stageCtx, gpu.TaskVLM, models.VideoSummaryTool, run.req.JobID, run.req.RequestID)
	var visionErr error
	defer func() {
		if lease != nil {
			lease.Release(context.Background(), visionErr)
		}
	}()

	batches := BatchFrames(run.frames, summaryVisionBatchSize, run.duration)
	skipped := 0
	for i, batch := range batches {
		run.st.progress(SummaryStageVision, float64(i)/float64(len(batches)), fmt.Sprintf("Batch %d of %d", i+1, len(batches)))
		images, err := encodeImagesBase64(FramePaths(batch.Frames))
		if err != nil {
			visionErr = err
			return userErrWrap("Failed to read the extracted frames", err)
		}
		// Keep the VLM loaded between batches (the daemon runs with
		// OLLAMA_KEEP_ALIVE=0); the last call lets it unload so synthesis has
		// the card.
		keepAlive := "2m"
		if i == len(batches)-1 {
			keepAlive = "0s"
		}
		res, err := ChatJSON(stageCtx, JSONChatRequest{
			Client:    s.deps.httpClient,
			BaseURL:   s.settings.OllamaURL,
			Model:     s.settings.VLMModel,
			System:    visionSystemPrompt,
			User:      visionUserPrompt(batch, TranscriptWindow(run.transcript.Segments, batch.Start, batch.End)),
			Images:    images,
			Options:   map[string]any{"num_ctx": 8192, "temperature": 0.2},
			KeepAlive: keepAlive,
		}, func(raw []byte) error { _, err := ParseBatchNote(raw, batch); return err })
		if err != nil {
			if stageCtx.Err() != nil || !isRepairFailure(err) {
				visionErr = err
				if stageCtx.Err() != nil {
					return userErrWrap("Describing the frames took too long", err)
				}
				return userErrWrap("The vision model is not available right now", err)
			}
			log.Printf("video-summary job %s: vision batch %d skipped: %v", run.req.JobID, i+1, err)
			skipped++
			continue
		}
		note, _ := ParseBatchNote(res.Content, batch)
		run.notes = append(run.notes, note)
	}
	if len(run.notes) == 0 {
		visionErr = errors.New("no batch described")
		return userErr("The vision model returned no usable description")
	}
	if skipped > 0 {
		run.addCaveat(CaveatSomeFramesSkipped)
	}
	if err := writeJSON(filepath.Join(run.req.OutputDir, SummaryVisionFile), run.notes); err != nil {
		visionErr = err
		return userErrWrap("Failed to save the frame notes", err)
	}
	run.st.set(SummaryStageVision, models.StageStatusCompleted, fmt.Sprintf("%d batches", len(run.notes)))
	return nil
}

func isRepairFailure(err error) bool {
	return err != nil && strings.Contains(err.Error(), "after one repair retry")
}

// synthesisModel picks the model and daemon for this job's effective depth.
func (s *VideoSummaryService) synthesisModel(depth string) (model, baseURL string) {
	if depth == models.VideoSummaryDepthDeep && s.settings.DeepAvailable() {
		return s.settings.DeepModel, s.settings.DeepURL
	}
	return s.settings.SummaryModel, s.settings.OllamaURL
}

// BuildSummarySystemPrompt fixes the JSON schema, the chapter rules for this
// duration and the output language.
func BuildSummarySystemPrompt(durationSec float64, outputLanguage, speechLanguage string, hasSpeech, hasVideo bool) string {
	minCh := models.MinChaptersFor(durationSec)
	maxCh := models.VideoSummaryMaxChapters
	if minCh < models.VideoSummaryMinChapters {
		maxCh = 3
	}
	var b strings.Builder
	b.WriteString(`You write a summary of a video or audio recording from timestamped visual notes and a machine transcript. Reply with ONE JSON object and nothing else, with exactly this shape:
{"title": string, "summary": string, "chapters": [{"start": number, "end": number, "title": string, "description": string}], "topics": [string], "onScreenText": [{"text": string, "firstSeen": number}], "visual": string, "speech": string, "language": string, "caveats": [string]}
Rules:
- title: at most 80 characters, specific and plain.
`)
	if minCh < models.VideoSummaryMinChapters {
		b.WriteString("- summary: 1 to 3 sentences on what the clip shows.\n")
	} else {
		b.WriteString("- summary: 3 to 6 sentences on what the recording is about and what happens.\n")
	}
	if durationSec > 0 {
		fmt.Fprintf(&b, "- chapters: %d to %d, in time order; start and end are SECONDS as numbers between 0 and %.1f; each chapter starts where the previous one ends; the first starts at 0 and the last ends at %.1f; each has a short title and a one-sentence description.\n", minCh, maxCh, durationSec, durationSec)
	} else {
		fmt.Fprintf(&b, "- chapters: %d to %d, in time order; start and end are SECONDS as numbers taken from the transcript timestamps; each has a short title and a one-sentence description.\n", minCh, maxCh)
	}
	b.WriteString("- topics: up to 8 short topic labels.\n")
	if hasVideo {
		b.WriteString("- onScreenText: notable on-screen text from the visual notes with the time first seen in seconds; [] if none.\n")
		b.WriteString("- visual: one paragraph on the setting, people, objects and camera style.\n")
	} else {
		b.WriteString("- onScreenText: [] (there is no picture).\n- visual: \"\" (there is no picture).\n")
	}
	if hasSpeech {
		b.WriteString("- speech: one paragraph on who speaks and the tone.\n")
	} else {
		b.WriteString("- speech: \"\" (there is no speech).\n")
	}
	b.WriteString("- language: the BCP-47 code of the language you wrote in.\n")
	b.WriteString("- caveats: anything a reader should know about the limits of this summary; [] if none.\n")
	b.WriteString("- Use only what the notes and the transcript say. Do not invent names, numbers, quotes or events.\n")
	switch {
	case outputLanguage != "" && outputLanguage != models.VideoSummaryLanguageAuto:
		name := models.VideoSummaryLanguageName(outputLanguage)
		if name == "" {
			name = outputLanguage
		}
		fmt.Fprintf(&b, "Write every text field in %s (%s), whatever language the speech is in. Keep on-screen text exactly as written.\n", name, outputLanguage)
	case hasSpeech && strings.TrimSpace(speechLanguage) != "":
		fmt.Fprintf(&b, "Write every text field in the language of the speech (%s). Keep on-screen text exactly as written.\n", speechLanguage)
	default:
		b.WriteString("Write every text field in English. Keep on-screen text exactly as written.\n")
	}
	return b.String()
}

func (s *VideoSummaryService) stageSynthesisAndRender(ctx context.Context, run *summaryRun) error {
	run.st.set(SummaryStageSynthesis, models.StageStatusProcessing, "")
	stageCtx, cancel := context.WithTimeout(ctx, s.settings.StageTimeout)
	defer cancel()

	duration := run.duration
	if duration <= 0 && len(run.transcript.Segments) > 0 {
		duration = run.transcript.Segments[len(run.transcript.Segments)-1].End
	}
	in := BuildSynthesisInput(duration, run.notes, run.transcript.Segments, s.settings.MaxInputTokens)
	for _, c := range in.Caveats {
		run.addCaveat(c)
	}
	model, baseURL := s.synthesisModel(run.req.Depth)
	hasSpeech := run.transcript.HasSpeech()
	system := BuildSummarySystemPrompt(duration, run.req.OutputLanguage, run.transcript.Language, hasSpeech, len(run.notes) > 0)

	lease, _ := s.deps.leaser.Acquire(stageCtx, gpu.TaskOllama, models.VideoSummaryTool, run.req.JobID, run.req.RequestID)
	res, err := ChatJSON(stageCtx, JSONChatRequest{
		Client:    s.deps.httpClient,
		BaseURL:   baseURL,
		Model:     model,
		System:    system,
		User:      in.Text,
		Options:   map[string]any{"num_ctx": s.settings.MaxInputTokens + 4096, "temperature": 0.3},
		KeepAlive: "0s",
	}, SummaryValidator(duration))
	if lease != nil {
		lease.Release(context.Background(), err)
	}
	if err != nil {
		switch {
		case stageCtx.Err() != nil:
			return userErrWrap("Writing the summary took too long", err)
		case isRepairFailure(err):
			return userErrWrap("The summary model returned an invalid summary twice", err)
		default:
			return userErrWrap("The summary model is not available right now", err)
		}
	}
	summary, err := ParseVideoSummary(res.Content, duration)
	if err != nil {
		return userErrWrap("The summary model returned an invalid summary", err)
	}
	finishSummary(summary, run, duration)
	msg := ""
	if res.Attempts > 1 {
		msg = "Repaired once"
	}
	run.st.set(SummaryStageSynthesis, models.StageStatusCompleted, msg)

	run.st.set(SummaryStageRender, models.StageStatusProcessing, "")
	md := RenderSummaryMarkdown(summary)
	jsonPath := filepath.Join(run.req.OutputDir, SummaryJSONFile)
	mdPath := filepath.Join(run.req.OutputDir, SummaryMarkdownFile)
	if err := writeJSON(jsonPath, summary); err != nil {
		return userErrWrap("Failed to save the summary", err)
	}
	if err := os.WriteFile(mdPath, []byte(md), 0o644); err != nil {
		return userErrWrap("Failed to save the summary", err)
	}
	var size int64
	for _, p := range []string{jsonPath, mdPath} {
		if info, err := os.Stat(p); err == nil {
			size += info.Size()
		}
	}
	jm := s.jobManager
	_ = jm.SetResultSize(run.req.JobID, size)
	_ = jm.SetResultMime(run.req.JobID, "text/markdown")
	_ = jm.SetResultMetadata(run.req.JobID, "", SummaryMarkdownFile, time.Time{})
	if err := jm.UpdateJobResult(run.req.JobID, "/api/summarize/"+run.req.JobID+"/result?format=md"); err != nil {
		return userErrWrap("Failed to finalize the summary", err)
	}
	run.st.set(SummaryStageRender, models.StageStatusCompleted, "")
	_ = jm.UpdateJobStatus(run.req.JobID, models.StatusCompleted)
	return nil
}

// finishSummary normalises the model's summary and merges what the pipeline
// knows for certain: the caveats, the on-screen text the VLM read (with real
// frame times) and the output language.
func finishSummary(summary *models.VideoSummary, run *summaryRun, duration float64) {
	models.NormalizeVideoSummary(summary, duration)
	fromNotes := OnScreenTextFromNotes(run.notes)
	merged := append(fromNotes, summary.OnScreenText...)
	if len(run.notes) == 0 {
		merged = nil // no picture: anything here was invented
	}
	for i := range merged {
		if duration > 0 && float64(merged[i].FirstSeen) > duration {
			merged[i].FirstSeen = models.Seconds(duration)
		}
	}
	summary.OnScreenText = models.DedupeOnScreenText(merged, models.VideoSummaryMaxOnScreenText)
	if !run.transcript.HasSpeech() {
		summary.Speech = ""
	}
	if len(run.notes) == 0 {
		summary.Visual = ""
	}
	switch {
	case run.req.OutputLanguage != "" && run.req.OutputLanguage != models.VideoSummaryLanguageAuto:
		summary.Language = run.req.OutputLanguage
	case summary.Language == "" && run.transcript.Language != "":
		summary.Language = run.transcript.Language
	}
	caveats := append([]string{}, run.caveats...)
	caveats = append(caveats, summary.Caveats...)
	summary.Caveats = models.FlexStrings(caveats)
	models.NormalizeVideoSummary(summary, duration)
}

// RenderSummaryMarkdown is summary.md. The format is a contract with the UI's
// "Copy as Markdown" (MM75), which must produce the same text from summary.json:
//
//	# <title>
//
//	<summary>
//
//	## Chapters
//
//	- **<m:ss>–<m:ss> · <chapter title>**: <description>   (": <description>" omitted when empty)
//
//	## Topics
//
//	<topic> · <topic> · ...
//
//	## On-screen text
//
//	- <m:ss>: <text>
//
//	## Visual
//
//	<visual>
//
//	## Speech
//
//	<speech>
//
//	## Notes
//
//	- <caveat>
//
// Empty sections are omitted; sections are separated by one blank line; the
// file ends with a single newline. Timestamps use FormatClock (m:ss, or
// h:mm:ss from one hour). The dash between times is U+2013, the separator
// U+00B7.
func RenderSummaryMarkdown(s *models.VideoSummary) string {
	var sections []string
	title := strings.TrimSpace(s.Title)
	if title == "" {
		title = "Summary"
	}
	sections = append(sections, "# "+title)
	if v := strings.TrimSpace(s.Summary); v != "" {
		sections = append(sections, v)
	}
	if len(s.Chapters) > 0 {
		lines := make([]string, 0, len(s.Chapters))
		for _, ch := range s.Chapters {
			line := fmt.Sprintf("- **%s–%s · %s**", FormatClock(float64(ch.Start)), FormatClock(float64(ch.End)), strings.TrimSpace(ch.Title))
			if d := strings.TrimSpace(ch.Description); d != "" {
				line += ": " + d
			}
			lines = append(lines, line)
		}
		sections = append(sections, "## Chapters\n\n"+strings.Join(lines, "\n"))
	}
	if len(s.Topics) > 0 {
		sections = append(sections, "## Topics\n\n"+strings.Join(s.Topics, " · "))
	}
	if len(s.OnScreenText) > 0 {
		lines := make([]string, 0, len(s.OnScreenText))
		for _, o := range s.OnScreenText {
			lines = append(lines, fmt.Sprintf("- %s: %s", FormatClock(float64(o.FirstSeen)), o.Text))
		}
		sections = append(sections, "## On-screen text\n\n"+strings.Join(lines, "\n"))
	}
	if v := strings.TrimSpace(s.Visual); v != "" {
		sections = append(sections, "## Visual\n\n"+v)
	}
	if v := strings.TrimSpace(s.Speech); v != "" {
		sections = append(sections, "## Speech\n\n"+v)
	}
	if len(s.Caveats) > 0 {
		lines := make([]string, 0, len(s.Caveats))
		for _, c := range s.Caveats {
			lines = append(lines, "- "+c)
		}
		sections = append(sections, "## Notes\n\n"+strings.Join(lines, "\n"))
	}
	return strings.Join(sections, "\n\n") + "\n"
}

// ---------------------------------------------------------------------------
// Production steps
// ---------------------------------------------------------------------------

// transcribeWithWhisper reuses the user-facing transcription plumbing:
// 16 kHz mono WAV, the shared GPU scheduler for the device, the whisper
// permit, WHISPER_CT2_* config.
func (s *VideoSummaryService) transcribeWithWhisper(ctx context.Context, req VideoSummaryRequest, workDir string) (models.VideoSummaryTranscript, error) {
	audioPath, cleanup, err := extractAudioForTranscribe(ctx, req.InputPath, workDir)
	if err != nil {
		return models.VideoSummaryTranscript{}, err
	}
	defer cleanup()
	whisperCfg := loadWhisperCT2ConfigFromEnv()
	gpuReq := ResolveWhisperGPURequest(whisperCfg)
	gpuReq.Kind = "whisper-ct2[summary]"
	gpuReq.Priority = req.GPUPriority
	gpuIdx, releaseGPU, err := acquireGPUForJob(ctx, s.jobManager, req.JobID, gpuReq)
	if err != nil {
		return models.VideoSummaryTranscript{}, fmt.Errorf("acquire GPU for whisper: %w", err)
	}
	defer releaseGPU()
	if gpuIdx >= 0 {
		whisperCfg.DeviceIndex = &gpuIdx
	}
	release, err := acquireWhisperPermit(ctx)
	if err != nil {
		return models.VideoSummaryTranscript{}, fmt.Errorf("acquire whisper permit: %w", err)
	}
	defer release()
	resp, err := callWhisperCT2(ctx, whisperCfg, audioPath, workDir)
	if err != nil {
		return models.VideoSummaryTranscript{}, err
	}
	out := models.VideoSummaryTranscript{
		Language: normalizeLanguageCode(resp.Language),
		Text:     strings.TrimSpace(resp.Text),
		Segments: make([]models.VideoSummaryTranscriptSegment, 0, len(resp.Segments)),
	}
	for _, seg := range resp.Segments {
		if strings.TrimSpace(seg.Text) == "" {
			continue
		}
		out.Segments = append(out.Segments, models.VideoSummaryTranscriptSegment{
			Start: roundMilli(seg.Start), End: roundMilli(seg.End), Text: strings.TrimSpace(seg.Text),
		})
	}
	return out, nil
}

// OllamaBaseReachable is a cheap GET <base>/api/tags (the same probe document
// scan uses for its own daemon).
func OllamaBaseReachable(ctx context.Context, baseURL string) bool {
	return documentScanOllamaReachable(ctx, baseURL)
}
