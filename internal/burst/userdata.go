package burst

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"text/template"
	"time"
)

// UserDataParams are the per-run values of the cloud-init script. The Tailscale
// key and RunToken are secrets: they appear only in the rendered script.
type UserDataParams struct {
	RunID         string
	Hostname      string
	Tag           string
	TailscaleKey  string
	RunToken      string
	Bucket        string
	Region        string
	Prefix        string // "burst/<runId>"
	WorkerHash    string // sha256 of the worker binary
	WorkerKey     string // burst/bin/<sha>/burst-worker
	Port          int
	MaxMinutes    int
	OrphanMinutes int
	Family        string
	Arch          string
	ManifestHash  string
	Prewarm       []string
	// Session lets the worker take another run after one finishes (MM138 Lab
	// sessions); false keeps the one-spec-per-instance rule.
	Session bool
}

var safeValue = regexp.MustCompile(`^[A-Za-z0-9._:/@=+-]+$`)

// ErrUserData: a value that cannot be placed in the script safely.
var ErrUserData = errors.New("burst: unsafe user-data value")

func (p UserDataParams) check() error {
	for name, v := range map[string]string{"RunID": p.RunID, "Hostname": p.Hostname, "Tag": p.Tag, "TailscaleKey": p.TailscaleKey,
		"RunToken": p.RunToken, "Bucket": p.Bucket, "Region": p.Region, "Prefix": p.Prefix, "WorkerHash": p.WorkerHash, "WorkerKey": p.WorkerKey} {
		if !safeValue.MatchString(v) {
			return fmt.Errorf("%w: %s", ErrUserData, name) // never the value
		}
	}
	for _, v := range []string{p.Family, p.Arch, p.ManifestHash} {
		if v != "" && !safeValue.MatchString(v) {
			return ErrUserData
		}
	}
	for _, v := range p.Prewarm {
		if !safeValue.MatchString(v) {
			return ErrUserData
		}
	}
	if p.Port <= 0 || p.MaxMinutes <= 0 {
		return ErrUserData
	}
	return nil
}

var userDataTmpl = template.Must(template.New("ud").Parse(`#!/bin/bash
# MM125 burst worker bootstrap. Contains two secrets (single-use Tailscale key,
# per-run token); never log or store this script.
set -euo pipefail
shutdown -h +{{.MaxMinutes}}

# Instance-store NVMe at /opt/dlami/nvme (the DLAMI mounts it; format it if not).
if ! mountpoint -q /opt/dlami/nvme; then
  dev="$(lsblk -dpno NAME,MODEL | awk '/Instance Storage/ {print $1; exit}')"
  if [ -n "$dev" ]; then
    mkdir -p /opt/dlami/nvme
    mkfs.xfs -f "$dev"
    mount "$dev" /opt/dlami/nvme
  fi
fi
mkdir -p /opt/dlami/nvme/burst

tailscale up --authkey={{.TailscaleKey}} --hostname={{.Hostname}} --advertise-tags={{.Tag}} --accept-dns=false

export AWS_DEFAULT_REGION={{.Region}}
aws s3 cp s3://{{.Bucket}}/{{.WorkerKey}} /usr/local/bin/burst-worker --only-show-errors
echo "{{.WorkerHash}}  /usr/local/bin/burst-worker" | sha256sum -c --status -
chmod 0755 /usr/local/bin/burst-worker

umask 077
mkdir -p /etc/mm-burst
cat > /etc/mm-burst/run.json <<'MMBURSTJSON'
{{.RunJSON}}
MMBURSTJSON
chmod 0600 /etc/mm-burst/run.json

systemctl start burst-worker

# Best effort: do not leave the secrets in cloud-init's copy of this script.
rm -f /var/lib/cloud/instance/scripts/* /var/lib/cloud/instance/user-data.txt* 2>/dev/null || true
`))

type runJSON struct {
	RunID    string   `json:"runId"`
	RunToken string   `json:"runToken"`
	Bucket   string   `json:"bucket"`
	Prefix   string   `json:"prefix"`
	Port     int      `json:"port"`
	Region   string   `json:"region"`
	Family   string   `json:"family,omitempty"`
	Arch     string   `json:"arch,omitempty"`
	Manifest string   `json:"manifestHash,omitempty"`
	Orphan   int      `json:"orphanMinutes"`
	Prewarm  []string `json:"prewarm,omitempty"`
	Session  bool     `json:"session,omitempty"`
}

// RenderUserData renders the cloud-init script. It validates every value
// (nothing is shell-escaped; unsafe input is refused).
func RenderUserData(p UserDataParams) (string, error) {
	if err := p.check(); err != nil {
		return "", err
	}
	rj, _ := json.Marshal(runJSON{RunID: p.RunID, RunToken: p.RunToken, Bucket: p.Bucket, Prefix: p.Prefix, Port: p.Port,
		Region: p.Region, Family: p.Family, Arch: p.Arch, Manifest: p.ManifestHash, Orphan: p.OrphanMinutes, Prewarm: p.Prewarm, Session: p.Session})
	var b bytes.Buffer
	err := userDataTmpl.Execute(&b, struct {
		UserDataParams
		RunJSON string
	}{p, string(rj)})
	return b.String(), err
}

// RedactUserData replaces the secrets with <redacted> (acceptance output).
func RedactUserData(script string, secrets ...string) string {
	for _, s := range secrets {
		if s != "" {
			script = strings.ReplaceAll(script, s, "<redacted>")
		}
	}
	return script
}

// WorkerBinary uploads the worker build by hash (decision 6). The lifecycle
// rule expires burst/ objects after a day, so EnsureUploaded runs again for
// every launch (one HEAD) and re-uploads when the object has expired.
type WorkerBinary interface {
	EnsureUploaded(ctx context.Context) (hash, key string, err error)
}

// UserDataBuilder builds each run's user-data and remembers its run token.
type UserDataBuilder struct {
	Cfg    UserDataConfig
	Binary WorkerBinary
	// Timeout bounds the per-launch binary check (default 2 minutes).
	Timeout time.Duration

	mu     sync.Mutex
	tokens map[string]string
}

// UserDataConfig is the non-secret static part.
type UserDataConfig struct {
	Bucket        string
	Region        string
	Tag           string
	Prefix        string // "burst/"
	Port          int
	MaxMinutes    int
	OrphanMinutes int
}

func (b *UserDataBuilder) setToken(runID, tok string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.tokens == nil {
		b.tokens = map[string]string{}
	}
	b.tokens[runID] = tok
}

// Token returns the run's HMAC token (held in memory only, never persisted).
func (b *UserDataBuilder) Token(runID string) (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	t, ok := b.tokens[runID]
	return t, ok
}

// Forget drops the token when the run ends.
func (b *UserDataBuilder) Forget(runID string) {
	b.mu.Lock()
	delete(b.tokens, runID)
	b.mu.Unlock()
}

// RunPrefix is "burst/<runId>" for a configured prefix.
func RunPrefix(prefix, runID string) string {
	p := strings.Trim(prefix, "/")
	if p == "" {
		p = "burst"
	}
	return p + "/" + runID
}

// Build is a UserDataFunc.
func (b *UserDataBuilder) Build(run Run, prof Profile, tsKey string) (string, error) {
	if b.Binary == nil {
		return "", errors.New("burst: no worker binary configured")
	}
	to := b.Timeout
	if to <= 0 {
		to = 2 * time.Minute
	}
	ctx, cancel := context.WithTimeout(context.Background(), to)
	defer cancel()
	hash, key, err := b.Binary.EnsureUploaded(ctx)
	if err != nil {
		return "", fmt.Errorf("burst: worker binary: %w", err)
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	tok := hex.EncodeToString(raw)
	maxMin := b.Cfg.MaxMinutes
	if need := prof.RemoteTimeout/60 + 15; need > maxMin {
		maxMin = need
	}
	script, err := RenderUserData(UserDataParams{
		RunID: run.ID, Hostname: WorkerHostname(run.ID), Tag: b.Cfg.Tag, TailscaleKey: tsKey, RunToken: tok,
		Bucket: b.Cfg.Bucket, Region: b.Cfg.Region, Prefix: RunPrefix(b.Cfg.Prefix, run.ID), WorkerHash: hash, WorkerKey: key,
		Port: b.Cfg.Port, MaxMinutes: maxMin, OrphanMinutes: b.Cfg.OrphanMinutes, Family: prof.Family, Arch: run.Arch,
		Prewarm: prof.Prewarm, Session: run.Purpose == PurposeLab,
	})
	if err != nil {
		return "", err
	}
	b.setToken(run.ID, tok)
	return script, nil
}
