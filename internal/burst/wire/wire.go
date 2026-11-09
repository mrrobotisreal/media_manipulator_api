// Package wire is the burst worker protocol shared by the API (remote
// executor) and cmd/burst-worker (design §10, MM125). The signature scheme:
//
//	X-MM-Burst-Ts:  unix seconds
//	X-MM-Burst-Sig: hex(HMAC-SHA256(runToken, METHOD + REQUEST_URI + BODY + TS))
//
// A request more than 60 s from the worker's clock, or with a bad signature,
// is 401.
package wire

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strconv"
	"time"
)

const (
	HeaderSig = "X-MM-Burst-Sig"
	HeaderTs  = "X-MM-Burst-Ts"
	// MaxSkew is the allowed clock difference.
	MaxSkew = 60 * time.Second
	// ProtocolVersion is bumped when the wire format changes; the worker is
	// fetched by hash with the API's own build, so they always match.
	ProtocolVersion = 1
)

// Sign returns the signature for a request.
func Sign(token, method, requestURI string, body []byte, ts int64) string {
	m := hmac.New(sha256.New, []byte(token))
	m.Write([]byte(method))
	m.Write([]byte(requestURI))
	m.Write(body)
	m.Write([]byte(strconv.FormatInt(ts, 10)))
	return hex.EncodeToString(m.Sum(nil))
}

// SignRequest sets both headers on req.
func SignRequest(req *http.Request, token string, body []byte, now time.Time) {
	ts := now.Unix()
	req.Header.Set(HeaderTs, strconv.FormatInt(ts, 10))
	req.Header.Set(HeaderSig, Sign(token, req.Method, req.URL.RequestURI(), body, ts))
}

// Verify checks the headers of r against token at now.
func Verify(token string, r *http.Request, body []byte, now time.Time) bool {
	ts, err := strconv.ParseInt(r.Header.Get(HeaderTs), 10, 64)
	if err != nil {
		return false
	}
	d := now.Sub(time.Unix(ts, 0))
	if d < 0 {
		d = -d
	}
	if d > MaxSkew {
		return false
	}
	want := Sign(token, r.Method, r.URL.RequestURI(), body, ts)
	return hmac.Equal([]byte(want), []byte(r.Header.Get(HeaderSig)))
}

// Cmd is one remapped command.
type Cmd struct {
	Label      string   `json:"label,omitempty"`
	Exe        string   `json:"exe"`
	Args       []string `json:"args,omitempty"`
	Env        []string `json:"env,omitempty"`
	Dir        string   `json:"dir,omitempty"`
	TimeoutSec int      `json:"timeoutSec,omitempty"`
}

// PathSpec is one declared path at the worker.
type PathSpec struct {
	N        int    `json:"n"`
	Remote   string `json:"remote"`
	Dir      bool   `json:"dir,omitempty"`
	Optional bool   `json:"optional,omitempty"`
}

// RunRequest is POST /v1/runs. Prefix is the run's S3 prefix without a
// trailing slash ("burst/<runId>").
type RunRequest struct {
	RunID      string     `json:"runId"`
	Prefix     string     `json:"prefix"`
	Steps      []Cmd      `json:"steps"`
	Inputs     []PathSpec `json:"inputs,omitempty"`
	Outputs    []PathSpec `json:"outputs,omitempty"`
	Scratch    []PathSpec `json:"scratch,omitempty"`
	InputBytes int64      `json:"inputBytes"`
}

// Run states reported by GET /v1/runs/{id}.
const (
	StateStagingIn  = "staging_in"
	StateRunning    = "running"
	StateStagingOut = "staging_out"
	StateDone       = "done"
	StateFailed     = "failed"
	StateCancelled  = "cancelled"
)

// Failure phases (Status.Phase).
const (
	PhaseStagingIn  = "staging_in"
	PhaseRunning    = "running"
	PhaseStagingOut = "staging_out"
)

// Status is GET /v1/runs/{id}?cursor=N.
type Status struct {
	RunID  string   `json:"runId"`
	State  string   `json:"state"`
	Step   int      `json:"step"`
	Lines  []string `json:"lines,omitempty"`
	Cursor int      `json:"cursor"`
	// ExitCode of the failing step (when State is failed in phase running).
	ExitCode   int    `json:"exitCode,omitempty"`
	TimedOut   bool   `json:"timedOut,omitempty"`
	Phase      string `json:"phase,omitempty"`
	Error      string `json:"error,omitempty"`
	StderrTail string `json:"stderrTail,omitempty"`
	// OutputBytes and OutputFiles are set once the outputs are uploaded; the
	// manifest itself is at <prefix>/out/manifest.json.
	OutputBytes int64 `json:"outputBytes,omitempty"`
	OutputFiles int   `json:"outputFiles,omitempty"`
}

// Terminal reports whether no further change is coming.
func (s Status) Terminal() bool {
	return s.State == StateDone || s.State == StateFailed || s.State == StateCancelled
}

// Health is GET /v1/health.
type Health struct {
	Ready         bool      `json:"ready"`
	GPU           GPUInfo   `json:"gpu"`
	Disk          DiskInfo  `json:"disk"`
	Image         ImageInfo `json:"image"`
	WorkerVersion string    `json:"workerVersion"`
	Protocol      int       `json:"protocol"`
	Busy          bool      `json:"busy"`
}

type GPUInfo struct {
	Name   string `json:"name"`
	MemMiB int    `json:"memMiB"`
	Driver string `json:"driver"`
	CUDA   string `json:"cuda"`
}

type DiskInfo struct {
	ScratchFreeGiB float64 `json:"scratchFreeGiB"`
}

type ImageInfo struct {
	Family       string `json:"family"`
	Arch         string `json:"arch"`
	ManifestHash string `json:"manifestHash"`
}

// ErrorBody is every non-2xx JSON body.
type ErrorBody struct {
	Error string `json:"error"`
}
