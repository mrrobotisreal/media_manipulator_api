package services

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
)

// countingRefund is a QuotaRefunder that counts real refunds.
type countingRefund struct{ n atomic.Int32 }

func (c *countingRefund) Refund(string) bool { c.n.Add(1); return true }

// fakeSynth stands in for the mm-voices client: it writes outWav unless told
// to fail, and records which endpoint ran.
type fakeSynth struct {
	err    error
	noFile bool
	calls  []string
	refs   []string
}

func (f *fakeSynth) write(endpoint, out string) (*VoicesResult, error) {
	f.calls = append(f.calls, endpoint)
	if f.err != nil {
		return nil, f.err
	}
	if f.noFile {
		return nil, ErrVoicesBadOutput // what the real client returns for a missing wav
	}
	return &VoicesResult{OutWav: out, Engine: "chatterbox"}, os.WriteFile(out, fakeWav, 0o644)
}
func (f *fakeSynth) TTS(_ context.Context, r VoicesTTSRequest) (*VoicesResult, error) {
	f.refs = append(f.refs, r.ReferenceWav)
	return f.write("tts", r.OutWav)
}
func (f *fakeSynth) TTSPreset(_ context.Context, r VoicesPresetRequest) (*VoicesResult, error) {
	return f.write("tts-preset", r.OutWav)
}
func (f *fakeSynth) VC(_ context.Context, r VoicesVCRequest) (*VoicesResult, error) {
	f.refs = append(f.refs, r.ReferenceWav)
	return f.write("vc", r.OutWav)
}
func (f *fakeSynth) Design(_ context.Context, r VoicesDesignRequest) (*VoicesResult, error) {
	return f.write("design", r.OutWav)
}

// fakeFFmpeg writes its last argument (the output path) and records the args.
type fakeFFmpeg struct{ calls [][]string }

func (f *fakeFFmpeg) run(_ context.Context, name string, args ...string) (string, string, error) {
	f.calls = append(f.calls, append([]string{name}, args...))
	return "", "", os.WriteFile(args[len(args)-1], fakeWav, 0o644)
}

type voiceJobEnv struct {
	svc    *VoicesService
	jm     *JobManager
	synth  *fakeSynth
	ff     *fakeFFmpeg
	refund *countingRefund
	leases atomic.Int32
	out    string
	ref    string
}

func newVoiceJobEnv(t *testing.T) *voiceJobEnv {
	t.Helper()
	cfg := &config.Config{Voices: config.VoicesConfig{Enabled: true, Timeout: time.Second, ScratchDir: t.TempDir(), VRAMMiB: 6000}}
	e := &voiceJobEnv{jm: NewJobManager(), synth: &fakeSynth{}, ff: &fakeFFmpeg{}, refund: &countingRefund{}, out: t.TempDir()}
	e.svc = NewVoicesService(cfg, e.jm, nil, e.synth)
	e.svc.SetRunner(e.ff.run)
	e.svc.SetGPUAcquire(func(context.Context, *JobManager, string, GPURequest) (int, func(), error) {
		e.leases.Add(1)
		return 0, func() {}, nil
	})
	e.ref = filepath.Join(t.TempDir(), "reference.wav")
	if err := os.WriteFile(e.ref, fakeWav, 0o644); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *voiceJobEnv) job(t *testing.T, tool string) VoiceJobRequest {
	t.Helper()
	j := e.jm.CreateJob(models.OriginalFileInfo{Name: "speech", Type: "audio/mpeg"}, map[string]any{"mode": tool})
	e.jm.SetJobQuotaCharge(j.ID, e.refund)
	return VoiceJobRequest{JobID: j.ID, Tool: tool, OutputDir: filepath.Join(e.out, j.ID), Language: "en", Text: "hello"}
}

func (e *voiceJobEnv) status(t *testing.T, id string) *models.ConversionJob {
	t.Helper()
	j, err := e.jm.GetJob(id)
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func TestVoiceTTSJobWritesWavAndMP3(t *testing.T) {
	e := newVoiceJobEnv(t)
	req := e.job(t, VoiceToolTTS)
	req.ReferencePath = e.ref
	e.svc.Process(context.Background(), req)
	j := e.status(t, req.JobID)
	if j.Status != models.StatusCompleted || j.ResultMime != "audio/mpeg" || j.ResultURL != "/api/download/"+req.JobID {
		t.Fatalf("job: %+v", j)
	}
	for _, f := range []string{VoiceResultWav, VoiceResultMP3} {
		if _, err := os.Stat(filepath.Join(req.OutputDir, f)); err != nil {
			t.Errorf("%s missing: %v", f, err)
		}
	}
	if e.refund.n.Load() != 0 {
		t.Error("a completed job keeps its unit")
	}
	if e.leases.Load() != 1 {
		t.Errorf("a TTS job holds one TaskVoices lease, got %d", e.leases.Load())
	}
	if len(e.synth.refs) != 1 || !strings.HasPrefix(e.synth.refs[0], e.svc.cfg.Voices.ScratchDir) {
		t.Errorf("the reference must be staged inside the scratch dir: %v", e.synth.refs)
	}
	last := e.ff.calls[len(e.ff.calls)-1]
	if !strings.Contains(strings.Join(last, " "), "libmp3lame") {
		t.Errorf("mp3 encode: %v", last)
	}
}

func TestVoicePresetTTSJob(t *testing.T) {
	e := newVoiceJobEnv(t)
	req := e.job(t, VoiceToolTTS)
	req.Preset = "Vivian"
	e.svc.Process(context.Background(), req)
	if j := e.status(t, req.JobID); j.Status != models.StatusCompleted || e.synth.calls[0] != "tts-preset" {
		t.Fatalf("preset job: %+v %v", j, e.synth.calls)
	}
}

// MM77 acceptance 5: service down (503) → job failed → unit refunded once;
// bad output (missing wav) → job failed → refunded; timeout likewise.
func TestVoiceJobFailuresRefundOnce(t *testing.T) {
	for name, err := range map[string]error{
		"down":       ErrVoicesUnavailable,
		"timeout":    ErrVoicesTimeout,
		"bad output": nil, // noFile
		"4xx":        &VoicesServiceError{Status: 400, Code: "language_not_supported"},
	} {
		t.Run(name, func(t *testing.T) {
			e := newVoiceJobEnv(t)
			e.synth.err = err
			e.synth.noFile = err == nil
			req := e.job(t, VoiceToolTTS)
			req.ReferencePath = e.ref
			e.svc.Process(context.Background(), req)
			j := e.status(t, req.JobID)
			if j.Status != models.StatusFailed {
				t.Fatalf("status %s", j.Status)
			}
			if e.refund.n.Load() != 1 {
				t.Fatalf("refunds = %d, want exactly 1", e.refund.n.Load())
			}
			if strings.Contains(j.Error, e.ref) || strings.Contains(j.Error, "/") {
				t.Errorf("job error must be user-safe: %q", j.Error)
			}
			// A second failure path for the same job must not refund again.
			_ = e.jm.UpdateJobStatus(req.JobID, models.StatusFailed)
			if e.refund.n.Load() != 1 {
				t.Fatalf("refund must be exactly once, got %d", e.refund.n.Load())
			}
		})
	}
}

func TestVoiceConvertJob(t *testing.T) {
	e := newVoiceJobEnv(t)
	req := e.job(t, VoiceToolConvert)
	req.InputPath = e.ref
	req.ReferencePath = e.ref
	e.svc.Process(context.Background(), req)
	if j := e.status(t, req.JobID); j.Status != models.StatusCompleted {
		t.Fatalf("convert: %+v", j)
	}
	if strings.Join(e.synth.calls, ",") != "vc" {
		t.Fatalf("calls %v", e.synth.calls)
	}
	// Convert to a preset renders the preset's reference first.
	e2 := newVoiceJobEnv(t)
	req2 := e2.job(t, VoiceToolConvert)
	req2.InputPath, req2.Preset = e2.ref, "Vivian"
	e2.svc.Process(context.Background(), req2)
	if strings.Join(e2.synth.calls, ",") != "tts-preset,vc" {
		t.Fatalf("preset convert calls %v", e2.synth.calls)
	}
}

func TestVoiceShiftJobUsesNoGPUOrService(t *testing.T) {
	e := newVoiceJobEnv(t)
	req := e.job(t, VoiceToolShift)
	req.InputPath = e.ref
	s, _ := ResolveVoiceShift("", 3, "shifted")
	req.Shift = s
	e.svc.Process(context.Background(), req)
	if j := e.status(t, req.JobID); j.Status != models.StatusCompleted {
		t.Fatalf("shift: %+v", j)
	}
	if e.leases.Load() != 0 || len(e.synth.calls) != 0 {
		t.Fatalf("the shifter must not touch the GPU or the service: leases %d calls %v", e.leases.Load(), e.synth.calls)
	}
	if !strings.Contains(strings.Join(e.ff.calls[0], " "), "rubberband=pitch=1.189207:formant=shifted") {
		t.Fatalf("shift args %v", e.ff.calls[0])
	}
}

func TestMeanNoSpeechProb(t *testing.T) {
	for name, tc := range map[string]struct {
		json string
		want float64
	}{
		"speech":      {`{"segments":[{"no_speech_prob":0.1},{"no_speech_prob":0.3}]}`, 0.2},
		"silence":     {`{"segments":[{"no_speech_prob":0.9},{"no_speech_prob":0.7}]}`, 0.8},
		"no segments": {`{"segments":[]}`, 1},
		"no field":    {`{"segments":[{"text":"hi"}]}`, 0},
	} {
		got, err := MeanNoSpeechProb([]byte(tc.json))
		if err != nil || got < tc.want-1e-9 || got > tc.want+1e-9 {
			t.Errorf("%s: %v %v", name, got, err)
		}
	}
	if _, err := MeanNoSpeechProb([]byte("x")); err == nil {
		t.Error("bad json")
	}
}

func TestVoiceUserMessage(t *testing.T) {
	if !strings.Contains(VoiceUserMessage(errors.Join(ErrVoicesUnavailable)), "unavailable") {
		t.Error("unavailable message")
	}
	if !strings.Contains(VoiceUserMessage(&VoicesServiceError{Code: "preset_not_found"}), "preset") {
		t.Error("preset message")
	}
}

// MM79 acceptance 7 (server truth at the emission seam): a voice job created
// through RecordJobCreated with tool voice_tts and failed by the service
// emits server_job_created then server_job_failed with tool_slug voice_tts in
// the context and a coarse error_type (never the message); the convert and
// shift tools flow the same way.
func TestVoiceJobServerTruthEvents(t *testing.T) {
	for tool, wantType := range map[string]string{
		VoiceToolTTS:     "upstream",
		VoiceToolConvert: "upstream",
		VoiceToolShift:   "tool_failure",
	} {
		t.Run(tool, func(t *testing.T) {
			e := newVoiceJobEnv(t)
			an := &stubAnalytics{}
			e.jm.SetAnalyticsEmitter(an)
			e.synth.err = ErrVoicesUnavailable
			req := e.job(t, tool)
			req.ReferencePath, req.InputPath = e.ref, e.ref
			e.jm.RecordJobCreated(context.Background(), req.JobID, testOwner(),
				JobFacts{Tool: tool, Mode: "Vivian", MediaKind: "audio", SourceFormat: "text", TargetFormat: "mp3"})
			if tool == VoiceToolShift {
				e.svc.SetRunner(func(context.Context, string, ...string) (string, string, error) {
					return "", "rubberband: /srv/uploads/x/secret name.wav", errors.New("exit status 1")
				})
			}
			e.svc.Process(context.Background(), req)
			created := an.named("server_job_created")
			failed := an.named("server_job_failed")
			if len(created) != 1 || len(failed) != 1 {
				t.Fatalf("created %d failed %d", len(created), len(failed))
			}
			ev := failed[0]
			if ev.Context["tool_slug"] != tool || ev.Context["job_id"] != req.JobID || ev.Context["media_kind"] != "audio" {
				t.Errorf("context %v", ev.Context)
			}
			if ev.Properties["error_type"] != wantType {
				t.Errorf("error_type %v want %s", ev.Properties["error_type"], wantType)
			}
			if _, ok := ev.Properties["duration_ms"]; !ok {
				t.Error("duration_ms missing")
			}
			for _, v := range ev.Properties {
				if s, ok := v.(string); ok && (strings.Contains(s, "/") || strings.Contains(s, " ")) {
					t.Errorf("a property carries message text: %q", s)
				}
			}
		})
	}
}

// MM79: Finish runs after the MP3 exists; its error fails the job (refunded
// once) with the user-safe message.
func TestVoiceJobFinishHook(t *testing.T) {
	e := newVoiceJobEnv(t)
	req := e.job(t, VoiceToolTTS)
	req.ReferencePath = e.ref
	var sawWav string
	req.Finish = func(_ context.Context, wav string) error {
		sawWav = wav
		if _, err := os.Stat(filepath.Join(filepath.Dir(wav), VoiceResultMP3)); err != nil {
			t.Errorf("Finish ran before the MP3 existed")
		}
		return errors.New("ingest: s3 down")
	}
	e.svc.Process(context.Background(), req)
	j := e.status(t, req.JobID)
	if filepath.Base(sawWav) != VoiceResultWav || j.Status != models.StatusFailed || j.Error != VoiceFinishFailedMessage {
		t.Fatalf("wav %q job %+v", sawWav, j)
	}
	if e.refund.n.Load() != 1 {
		t.Fatalf("refunds %d", e.refund.n.Load())
	}
}
