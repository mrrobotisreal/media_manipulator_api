package services

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/gpu"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
)

// Voice job tools (mm_conversion_jobs.tool). `mode` carries the voice id or
// the preset name.
const (
	VoiceToolTTS     = "voice_tts"
	VoiceToolConvert = "voice_convert"
	VoiceToolShift   = "voice_shift"
)

// Voice job result files in the job output dir. speech.mp3 is the download
// (/api/download/:jobId and /api/result/:jobId serve it); speech.wav sits
// beside it for lossless reuse.
const (
	VoiceResultWav = "speech.wav"
	VoiceResultMP3 = "speech.mp3"
)

// VoiceSampleText is the fixed sentence a designed voice's reference and every
// voice preview are rendered from (design §5: "a fixed sample sentence").
const VoiceSampleText = "Hello! This is my voice. I can read anything you write, in many languages, " +
	"with warmth and with feeling. Let's make something together."

// VoicesRunner runs ffmpeg for the voice jobs. Production uses the shared
// runCommand; tests inject a fake that writes the output file.
type VoicesRunner func(ctx context.Context, name string, args ...string) (stdout, stderr string, err error)

// VoicesSynth is the slice of *VoicesClient the jobs call. Tests pass a fake.
type VoicesSynth interface {
	TTS(ctx context.Context, req VoicesTTSRequest) (*VoicesResult, error)
	TTSPreset(ctx context.Context, req VoicesPresetRequest) (*VoicesResult, error)
	VC(ctx context.Context, req VoicesVCRequest) (*VoicesResult, error)
	Design(ctx context.Context, req VoicesDesignRequest) (*VoicesResult, error)
}

// VoicesService runs the voice jobs (voice_tts, voice_convert, voice_shift)
// and the synchronous steps of creating a voice. It is the voices sibling of
// DocumentScanService: the same JobManager lifecycle (UpdateJobError refunds
// the MM73 charge exactly once), a TaskVoices lease around every service call.
type VoicesService struct {
	cfg    *config.Config
	jobs   *JobManager
	gpuMgr *gpu.Manager
	synth  VoicesSynth
	run    VoicesRunner
	// acquire is the shared GPU scheduler; injectable so tests do not probe
	// nvidia-smi.
	acquire func(ctx context.Context, jm *JobManager, jobID string, req GPURequest) (int, func(), error)
	now     func() time.Time
}

// NewVoicesService wires the voice jobs. gpuMgr may be nil.
func NewVoicesService(cfg *config.Config, jm *JobManager, gpuMgr *gpu.Manager, synth VoicesSynth) *VoicesService {
	return &VoicesService{cfg: cfg, jobs: jm, gpuMgr: gpuMgr, synth: synth, run: runCommand, acquire: acquireGPUForJob, now: time.Now}
}

// SetRunner replaces the ffmpeg runner (tests).
func (s *VoicesService) SetRunner(r VoicesRunner) { s.run = r }

// SetGPUAcquire replaces the scheduler acquire (tests).
func (s *VoicesService) SetGPUAcquire(f func(ctx context.Context, jm *JobManager, jobID string, req GPURequest) (int, func(), error)) {
	s.acquire = f
}

// VoiceJobRequest is one validated voice job.
type VoiceJobRequest struct {
	JobID string
	Tool  string // VoiceToolTTS | VoiceToolConvert | VoiceToolShift
	// TTS
	Text         string
	Language     string
	Exaggeration *float64
	// Exactly one of ReferencePath (a saved voice's reference clip, already
	// fetched to local disk) and Preset is set for TTS and convert.
	ReferencePath string
	Preset        string
	// Convert and shift
	InputPath string
	Shift     VoiceShiftSettings
	// OutputDir is the job's output dir (OUTPUT_DIR/<jobId>).
	OutputDir   string
	RequestID   string
	GPUPriority int
	// Cleanup runs after the job ends either way (removes the fetched
	// reference and the scratch dir). Optional.
	Cleanup func()
	// Finish runs once the WAV and MP3 exist and before the job completes
	// (MM79: ingest the WAV as a Darkroom audio asset). An error fails the
	// job, which refunds its unit like any other failure. Optional.
	Finish func(ctx context.Context, wavPath string) error
}

// VoiceFinishFailedMessage is the job error when Finish fails (MM79).
const VoiceFinishFailedMessage = "Could not add the voiceover to the project. Your operation was not counted."

// Process runs one voice job to completion. Any failure goes through
// UpdateJobError, which marks the job failed and refunds its unit once.
func (s *VoicesService) Process(ctx context.Context, req VoiceJobRequest) {
	if req.Cleanup != nil {
		defer req.Cleanup()
	}
	timeout := s.cfg.Voices.Timeout + 2*time.Minute
	if timeout < 5*time.Minute {
		timeout = 5 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := s.jobs.UpdateJobStatus(req.JobID, models.StatusProcessing); err != nil {
		return
	}
	_ = s.jobs.UpdateJobProgress(req.JobID, 5)
	if err := os.MkdirAll(req.OutputDir, 0o755); err != nil {
		s.fail(req, "Could not prepare the result", err)
		return
	}
	wavPath := filepath.Join(req.OutputDir, VoiceResultWav)
	var err error
	switch req.Tool {
	case VoiceToolShift:
		err = s.shift(ctx, req, wavPath)
	case VoiceToolTTS, VoiceToolConvert:
		err = s.synthesize(ctx, req, wavPath)
	default:
		err = fmt.Errorf("unknown voice tool %q", req.Tool)
	}
	if err != nil {
		s.fail(req, VoiceUserMessage(err), err)
		return
	}
	_ = s.jobs.UpdateJobProgress(req.JobID, 85)
	mp3Path := filepath.Join(req.OutputDir, VoiceResultMP3)
	if _, stderr, err := s.run(ctx, "ffmpeg", VoiceMP3Args(wavPath, mp3Path)...); err != nil {
		s.fail(req, "Could not encode the MP3", fmt.Errorf("%w: %s", err, tail(stderr, 500)))
		return
	}
	st, err := os.Stat(mp3Path)
	if err != nil || st.Size() == 0 {
		s.fail(req, "Could not encode the MP3", ErrVoicesBadOutput)
		return
	}
	_ = s.jobs.SetResultSize(req.JobID, st.Size())
	_ = s.jobs.SetResultMime(req.JobID, "audio/mpeg")
	if secs := WavSeconds(wavPath); secs > 0 {
		_ = s.jobs.SetResultSeconds(req.JobID, secs)
	}
	if req.Finish != nil {
		if err := req.Finish(ctx, wavPath); err != nil {
			s.fail(req, VoiceFinishFailedMessage, err)
			return
		}
	}
	if err := s.jobs.UpdateJobResult(req.JobID, "/api/download/"+req.JobID); err != nil {
		s.fail(req, "Could not record the result", err)
		return
	}
	_ = s.jobs.UpdateJobStatus(req.JobID, models.StatusCompleted)
}

// shift is ffmpeg only: no GPU, no service, no watermark.
func (s *VoicesService) shift(ctx context.Context, req VoiceJobRequest, wavPath string) error {
	if _, stderr, err := s.run(ctx, "ffmpeg", VoiceShiftArgs(req.InputPath, wavPath, req.Shift)...); err != nil {
		return fmt.Errorf("rubberband shift: %w: %s", err, tail(stderr, 500))
	}
	if st, err := os.Stat(wavPath); err != nil || st.Size() <= 44 {
		return ErrVoicesBadOutput
	}
	return nil
}

// synthesize calls mm-voices under a TaskVoices lease. The service writes into
// the shared scratch dir; the result is then moved into the job output dir.
func (s *VoicesService) synthesize(ctx context.Context, req VoiceJobRequest, wavPath string) error {
	scratch, err := s.scratchDir("job-" + safeSegment(req.JobID) + "-")
	if err != nil {
		return fmt.Errorf("%w: scratch dir: %v", ErrVoicesUnavailable, err)
	}
	defer os.RemoveAll(scratch)
	out := filepath.Join(scratch, "out.wav")

	release, err := s.lease(ctx, req.JobID, req.RequestID, req.Tool, req.GPUPriority)
	if err != nil {
		return err
	}
	var callErr error
	defer func() { release(callErr) }()
	_ = s.jobs.UpdateJobProgress(req.JobID, 20)

	switch {
	case req.Tool == VoiceToolTTS && req.Preset != "":
		_, callErr = s.synth.TTSPreset(ctx, VoicesPresetRequest{Text: req.Text, Language: req.Language, Preset: req.Preset, OutWav: out})
	case req.Tool == VoiceToolTTS:
		ref, cerr := stageInScratch(req.ReferencePath, scratch, "reference.wav")
		if cerr != nil {
			callErr = cerr
			break
		}
		_, callErr = s.synth.TTS(ctx, VoicesTTSRequest{Text: req.Text, Language: req.Language, ReferenceWav: ref, Exaggeration: req.Exaggeration, OutWav: out})
	case req.Tool == VoiceToolConvert:
		in := filepath.Join(scratch, "input.wav")
		if _, stderr, rerr := s.run(ctx, "ffmpeg", VoiceWavArgs(req.InputPath, in, 0)...); rerr != nil {
			callErr = fmt.Errorf("decode input: %w: %s", rerr, tail(stderr, 500))
			break
		}
		ref, cerr := s.convertReference(ctx, req, scratch)
		if cerr != nil {
			callErr = cerr
			break
		}
		_, callErr = s.synth.VC(ctx, VoicesVCRequest{InputWav: in, ReferenceWav: ref, OutWav: out})
	}
	if callErr != nil {
		return callErr
	}
	return moveFile(out, wavPath)
}

// convertReference is the voice a recording is converted to: a saved voice's
// reference clip, or (for a preset) a short clip of the preset rendered first.
func (s *VoicesService) convertReference(ctx context.Context, req VoiceJobRequest, scratch string) (string, error) {
	if req.Preset == "" {
		return stageInScratch(req.ReferencePath, scratch, "reference.wav")
	}
	ref := filepath.Join(scratch, "preset-reference.wav")
	lang := req.Language
	if lang == "" {
		lang = "en"
	}
	if _, err := s.synth.TTSPreset(ctx, VoicesPresetRequest{Text: VoiceSampleText, Language: lang, Preset: req.Preset, OutWav: ref}); err != nil {
		return "", err
	}
	return ref, nil
}

// lease takes the shared scheduler slot on the voices card (VOICES_VRAM_MIB)
// and the TaskVoices telemetry lease. release(err) frees both.
func (s *VoicesService) lease(ctx context.Context, jobID, requestID, tool string, priority int) (func(error), error) {
	force := s.cfg.Voices.CUDAIndex
	_, releaseGPU, err := s.acquire(ctx, s.jobs, jobID, GPURequest{
		Kind:            "voices-" + tool,
		VRAMRequiredMiB: s.cfg.Voices.VRAMMiB,
		ForceIndex:      &force,
		Priority:        priority,
	})
	if err != nil {
		return nil, fmt.Errorf("%w: waiting for the GPU: %v", ErrVoicesTimeout, err)
	}
	var lease *gpu.Lease
	if s.gpuMgr != nil {
		lease, _ = s.gpuMgr.Acquire(ctx, gpu.TaskVoices, tool, jobID, requestID)
	}
	return func(err error) {
		if lease != nil {
			lease.Release(context.Background(), err)
		}
		releaseGPU()
	}, nil
}

// scratchDir makes a per-call directory under VOICES_SCRATCH_DIR, creating
// the root on first use (the service and the API both run as mwintrow).
func (s *VoicesService) scratchDir(pattern string) (string, error) {
	if err := os.MkdirAll(s.cfg.Voices.ScratchDir, 0o755); err != nil {
		return "", err
	}
	return os.MkdirTemp(s.cfg.Voices.ScratchDir, pattern)
}

// --- synchronous steps of creating a voice -----------------------------------

// RenderVoicePreview speaks the fixed sample sentence in a reference clip's
// voice and encodes it to MP3 at mp3Path. Used by POST /api/voices (the one
// counted ai_ops unit) and POST /api/voices/design.
func (s *VoicesService) RenderVoicePreview(ctx context.Context, jobID, requestID, referencePath, language, mp3Path string) (*VoicesResult, error) {
	scratch, err := s.scratchDir("preview-")
	if err != nil {
		return nil, fmt.Errorf("%w: scratch dir: %v", ErrVoicesUnavailable, err)
	}
	defer os.RemoveAll(scratch)
	ref, err := stageInScratch(referencePath, scratch, "reference.wav")
	if err != nil {
		return nil, err
	}
	out := filepath.Join(scratch, "preview.wav")
	release, err := s.lease(ctx, jobID, requestID, "voice_preview", GPUPriorityStandard)
	if err != nil {
		return nil, err
	}
	res, err := s.synth.TTS(ctx, VoicesTTSRequest{Text: VoiceSampleText, Language: language, ReferenceWav: ref, OutWav: out})
	release(err)
	if err != nil {
		return nil, err
	}
	if _, stderr, err := s.run(ctx, "ffmpeg", VoiceMP3Args(out, mp3Path)...); err != nil {
		return nil, fmt.Errorf("%w: encode preview: %v: %s", ErrVoicesBadOutput, err, tail(stderr, 300))
	}
	return res, nil
}

// RenderDesignedReference renders the 10 s reference clip of a voice designed
// from a description (Qwen3-TTS VoiceDesign) into refPath (WAV). From then on
// the voice is spoken by Chatterbox from that clip (design §2).
func (s *VoicesService) RenderDesignedReference(ctx context.Context, requestID, description, language string, seed int64, refPath string) (*VoicesResult, error) {
	scratch, err := s.scratchDir("design-")
	if err != nil {
		return nil, fmt.Errorf("%w: scratch dir: %v", ErrVoicesUnavailable, err)
	}
	defer os.RemoveAll(scratch)
	out := filepath.Join(scratch, "reference.wav")
	release, err := s.lease(ctx, "", requestID, "voice_design", GPUPriorityStandard)
	if err != nil {
		return nil, err
	}
	res, err := s.synth.Design(ctx, VoicesDesignRequest{Description: description, Language: language, Seed: seed, SampleText: VoiceSampleText, OutWav: out})
	release(err)
	if err != nil {
		return nil, err
	}
	if err := moveFile(out, refPath); err != nil {
		return nil, err
	}
	return res, nil
}

// EncodeMP3 encodes a WAV to MP3 (design previews).
func (s *VoicesService) EncodeMP3(ctx context.Context, wav, mp3 string) error {
	if _, stderr, err := s.run(ctx, "ffmpeg", VoiceMP3Args(wav, mp3)...); err != nil {
		return fmt.Errorf("encode mp3: %w: %s", err, tail(stderr, 300))
	}
	return nil
}

// NormalizeReference converts an uploaded clip to the mono 24 kHz WAV the
// engines read (and that is stored as reference.wav).
func (s *VoicesService) NormalizeReference(ctx context.Context, input, output string) error {
	if _, stderr, err := s.run(ctx, "ffmpeg", VoiceWavArgs(input, output, 0)...); err != nil {
		return fmt.Errorf("normalize reference: %w: %s", err, tail(stderr, 500))
	}
	return nil
}

// --- speech check -------------------------------------------------------------

// SpeechChecker reports the mean Whisper no_speech_prob of a clip.
type SpeechChecker interface {
	NoSpeechProbability(ctx context.Context, wavPath string) (float64, error)
}

// WhisperSpeechChecker is the create-voice speech check (ticket decision 10):
// a quick pass of the smallest configured Whisper model through the existing
// whisper-ctranslate2 path and GPU scheduler, on at most 30 s of audio.
type WhisperSpeechChecker struct {
	Model string
	Jobs  *JobManager
}

// voiceSpeechCheckSeconds caps the audio the speech check analyses.
const voiceSpeechCheckSeconds = 30

// NoSpeechProbability runs Whisper and averages segment no_speech_prob. A clip
// in which Whisper found no segment at all counts as 1.0 (no speech).
func (w WhisperSpeechChecker) NoSpeechProbability(ctx context.Context, wavPath string) (float64, error) {
	dir, err := os.MkdirTemp("", "mm-voice-speech-")
	if err != nil {
		return 0, err
	}
	defer os.RemoveAll(dir)
	clip := filepath.Join(dir, "clip.wav")
	if _, stderr, err := runCommand(ctx, "ffmpeg", "-hide_banner", "-nostdin", "-y", "-i", wavPath, "-t",
		fmt.Sprint(voiceSpeechCheckSeconds), "-ac", "1", "-ar", "16000", "-c:a", "pcm_s16le", clip); err != nil {
		return 0, fmt.Errorf("speech check: cut clip: %w: %s", err, tail(stderr, 300))
	}
	cfg := loadWhisperCT2ConfigFromEnv()
	if strings.TrimSpace(w.Model) != "" {
		cfg.Model = w.Model
	}
	cfg.VADFilter = false // VAD would drop exactly the silence we are measuring
	gpuIdx, release, err := acquireGPUForJob(ctx, w.Jobs, "", ResolveWhisperGPURequest(cfg))
	if err != nil {
		return 0, fmt.Errorf("speech check: GPU: %w", err)
	}
	defer release()
	if gpuIdx >= 0 {
		cfg.DeviceIndex = &gpuIdx
	}
	releasePermit, err := acquireWhisperPermit(ctx)
	if err != nil {
		return 0, err
	}
	defer releasePermit()
	payload, err := runWhisperCT2JSON(ctx, cfg, clip)
	if err != nil {
		return 0, err
	}
	return MeanNoSpeechProb(payload)
}

// MeanNoSpeechProb averages the per-segment no_speech_prob of a
// whisper-ctranslate2 JSON document. No segments means no speech (1.0).
func MeanNoSpeechProb(payload []byte) (float64, error) {
	var doc struct {
		Segments []struct {
			NoSpeechProb *float64 `json:"no_speech_prob"`
		} `json:"segments"`
	}
	if err := json.Unmarshal(payload, &doc); err != nil {
		return 0, fmt.Errorf("parse whisper json: %w", err)
	}
	var sum float64
	n := 0
	for _, seg := range doc.Segments {
		if seg.NoSpeechProb != nil {
			sum += *seg.NoSpeechProb
			n++
		}
	}
	if len(doc.Segments) == 0 {
		return 1, nil
	}
	if n == 0 {
		return 0, nil // segments without the field: Whisper heard words
	}
	return sum / float64(n), nil
}

// --- helpers --------------------------------------------------------------------

// VoiceUserMessage is the user-safe job error for a voices failure.
func VoiceUserMessage(err error) string {
	var se *VoicesServiceError
	switch {
	case errors.Is(err, ErrVoicesUnavailable):
		return "The voice engine is unavailable right now. Your operation was not counted; try again shortly."
	case errors.Is(err, ErrVoicesTimeout):
		return "The voice engine took too long. Your operation was not counted; try a shorter text."
	case errors.Is(err, ErrVoicesBadOutput):
		return "The voice engine returned no audio. Your operation was not counted."
	case errors.As(err, &se):
		switch se.Code {
		case "language_not_supported":
			return "That language is not supported for this voice."
		case "preset_not_found":
			return "That preset voice does not exist."
		}
		return "The voice engine refused the request."
	}
	return "Voice processing failed. Your operation was not counted."
}

func (s *VoicesService) fail(req VoiceJobRequest, userMsg string, err error) {
	// Never log text, audio paths with user ids, or reference contents.
	log.Printf("voices job %s (%s) failed: %v", req.JobID, req.Tool, scrubVoiceErr(err))
	_ = s.jobs.UpdateJobError(req.JobID, userMsg)
}

// scrubVoiceErr keeps the error class but drops anything after the first
// line, where ffmpeg stderr (which echoes paths) would start.
func scrubVoiceErr(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	if i := strings.IndexByte(msg, '\n'); i >= 0 {
		msg = msg[:i]
	}
	if len(msg) > 300 {
		msg = msg[:300]
	}
	return msg
}

// stageInScratch hard-links (or copies) src into the scratch dir so the
// service, which refuses paths outside it, can read it.
func stageInScratch(src, scratch, name string) (string, error) {
	if strings.TrimSpace(src) == "" {
		return "", fmt.Errorf("%w: no reference clip", ErrVoicesBadOutput)
	}
	dst := filepath.Join(scratch, name)
	if err := os.Link(src, dst); err == nil {
		return dst, nil
	}
	if err := copyFile(src, dst); err != nil {
		return "", fmt.Errorf("stage %s: %w", name, err)
	}
	return dst, nil
}

func copyFile(src, dst string) error {
	raw, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, raw, 0o644)
}

// moveFile renames, falling back to copy+remove across filesystems (the
// scratch dir and OUTPUT_DIR may be on different volumes).
func moveFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	if err := copyFile(src, dst); err != nil {
		return err
	}
	_ = os.Remove(src)
	return nil
}

func safeSegment(s string) string {
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "x"
	}
	return b.String()
}

// WavSeconds reads a PCM WAV's duration from its header (fmt byte rate and
// data chunk size), 0 when the file is not a readable RIFF/WAVE. Used for the
// job's resultSeconds (MM79); it never fails a job.
func WavSeconds(path string) float64 {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()
	head := make([]byte, 12)
	if _, err := io.ReadFull(f, head); err != nil || string(head[0:4]) != "RIFF" || string(head[8:12]) != "WAVE" {
		return 0
	}
	var byteRate uint32
	chunk := make([]byte, 8)
	for i := 0; i < 64; i++ {
		if _, err := io.ReadFull(f, chunk); err != nil {
			return 0
		}
		id, size := string(chunk[0:4]), binary.LittleEndian.Uint32(chunk[4:8])
		switch id {
		case "fmt ":
			body := make([]byte, size)
			if _, err := io.ReadFull(f, body); err != nil || size < 12 {
				return 0
			}
			byteRate = binary.LittleEndian.Uint32(body[8:12])
			if size%2 == 1 {
				_, _ = f.Seek(1, io.SeekCurrent)
			}
		case "data":
			if byteRate == 0 {
				return 0
			}
			if size == 0 || size == 0xFFFFFFFF { // streamed header: use the file size
				if st, err := f.Stat(); err == nil {
					pos, _ := f.Seek(0, io.SeekCurrent)
					size = uint32(st.Size() - pos)
				}
			}
			return float64(size) / float64(byteRate)
		default:
			if _, err := f.Seek(int64(size)+int64(size%2), io.SeekCurrent); err != nil {
				return 0
			}
		}
	}
	return 0
}
