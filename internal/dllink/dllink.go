// Package dllink mints and opens MM80 signed download links: short-lived,
// job-bound credentials that let a plain browser navigation (an <a href> or
// window.location, which cannot send X-MM-Session-ID or Authorization) fetch
// GET /api/download/:jobId?token=... straight into the browser's download
// manager instead of buffering the whole file in page memory.
//
// Security model:
//
//   - A token is minted only by POST /api/download/:jobId/link, which applies
//     the normal MM05 ownership check to the caller's headers. Holding a token
//     therefore proves an owner asked for THIS job within the last few minutes.
//   - The token is sealed with AES-256-GCM (an AEAD: the GCM tag authenticates
//     like an HMAC, and the encryption keeps the payload unreadable). The job id
//     is the additional authenticated data, so a token for job A fails to open
//     against job B, and any edited byte fails the tag.
//   - The payload carries the expiry plus the minting caller's analytics ids
//     (session, visitor, tier) so server_download_served stays attributed on a
//     header-less request. Sealing keeps the session id out of readable URLs,
//     unlike /result's ?sid=.
//   - No Firebase ID token ever enters a URL; the link is useless after its TTL
//     (default 5 minutes) and grants read access to one file only.
//
// Pure: no HTTP, no clock of its own (callers pass now), no database.
package dllink

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

const (
	prefix   = "v1."
	keyLabel = "mm-download-link-v1\x00"
	// MaxTokenLen bounds what Open will even try to decode.
	MaxTokenLen = 1024
)

// ErrInvalid covers every refusal: malformed, tampered, wrong job, expired.
// Callers answer all of them identically.
var ErrInvalid = errors.New("dllink: invalid or expired token")

// Claims is what a token carries besides its job binding.
type Claims struct {
	Expires   int64  `json:"e"`
	SessionID string `json:"s,omitempty"`
	VisitorID string `json:"v,omitempty"`
	Tier      string `json:"t,omitempty"`
	// Name is the owner's chosen save-as name (already sanitized by the
	// handler); sealed so a link holder cannot rewrite Content-Disposition.
	Name string `json:"n,omitempty"`
	// Variant (MM81) names a derived file to serve instead of the result:
	// "mp3" = the cached MP3 transcode of a WAV result. Empty = the result
	// itself, so every token minted before MM81 means what it meant.
	Variant string `json:"x,omitempty"`
}

// Signer seals and opens tokens with one server secret.
type Signer struct {
	aead cipher.AEAD
}

// New derives the AEAD key from secret. An empty secret gets a random
// per-process key: links then simply stop working across a restart, which is
// harmless because they live for minutes and the UI mints one per click.
func New(secret string) (*Signer, error) {
	var material []byte
	if strings.TrimSpace(secret) == "" {
		material = make([]byte, 32)
		if _, err := rand.Read(material); err != nil {
			return nil, err
		}
	} else {
		material = []byte(secret)
	}
	key := sha256.Sum256(append([]byte(keyLabel), material...))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Signer{aead: aead}, nil
}

// Mint returns a token for jobID valid until c.Expires.
func (s *Signer) Mint(jobID string, c Claims) (string, error) {
	if s == nil || jobID == "" {
		return "", ErrInvalid
	}
	plain, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	sealed := s.aead.Seal(nonce, nonce, plain, []byte(jobID))
	return prefix + base64.RawURLEncoding.EncodeToString(sealed), nil
}

// Open verifies token against jobID and now, returning its claims.
func (s *Signer) Open(jobID, token string, now time.Time) (Claims, error) {
	if s == nil || jobID == "" || len(token) > MaxTokenLen || !strings.HasPrefix(token, prefix) {
		return Claims{}, ErrInvalid
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(token, prefix))
	ns := s.aead.NonceSize()
	if err != nil || len(raw) < ns+s.aead.Overhead() {
		return Claims{}, ErrInvalid
	}
	plain, err := s.aead.Open(nil, raw[:ns], raw[ns:], []byte(jobID))
	if err != nil {
		return Claims{}, ErrInvalid
	}
	var c Claims
	if err := json.Unmarshal(plain, &c); err != nil || c.Expires <= 0 {
		return Claims{}, ErrInvalid
	}
	if now.Unix() >= c.Expires {
		return Claims{}, ErrInvalid
	}
	return c, nil
}
