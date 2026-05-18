package services

import (
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
)

// FaceDetectionSession remembers the face boxes returned by a single
// /api/ai/faces/detect call so the user can pick which faces to obscure and the
// final /api/upload job can reuse the exact boxes (and reject mismatched
// images via the recorded SHA256). The session intentionally stores metadata
// only — never the user's image bytes.
type FaceDetectionSession struct {
	ID               string
	ImageSHA256      string
	OriginalFileName string
	ImageWidth       int
	ImageHeight      int
	Faces            []models.FaceBox
	CreatedAt        time.Time
	ExpiresAt        time.Time
}

// FaceDetectionStore is a process-local TTL cache for face detection sessions.
// Sessions are intentionally short-lived (default 30 min) because they are
// only needed between the preview overlay step and the next /api/upload call
// for the same image. v1 keeps everything in memory because the API is a
// single process; if/when this is sharded, swap to a shared cache.
type FaceDetectionStore struct {
	mu       sync.RWMutex
	ttl      time.Duration
	sessions map[string]*FaceDetectionSession
}

func NewFaceDetectionStore(ttl time.Duration) *FaceDetectionStore {
	if ttl <= 0 {
		ttl = 30 * time.Minute
	}
	return &FaceDetectionStore{
		ttl:      ttl,
		sessions: make(map[string]*FaceDetectionSession),
	}
}

// TTL returns the configured session lifetime so handlers can advertise an
// accurate expiresAt back to the UI.
func (s *FaceDetectionStore) TTL() time.Duration {
	return s.ttl
}

// NewSession builds a session with an ID and expiry filled in; callers add
// faces/metadata and pass it to Store. We mint the ID here so the store
// controls its format ("fd_<uuid>") regardless of caller.
func (s *FaceDetectionStore) NewSession() *FaceDetectionSession {
	now := time.Now().UTC()
	return &FaceDetectionSession{
		ID:        "fd_" + uuid.New().String(),
		CreatedAt: now,
		ExpiresAt: now.Add(s.ttl),
	}
}

// Store persists a session, replacing any existing entry with the same ID, and
// opportunistically removes expired sessions.
func (s *FaceDetectionStore) Store(session *FaceDetectionSession) {
	if session == nil || session.ID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cleanupExpiredLocked()
	s.sessions[session.ID] = session
}

// Get returns the session and reports whether it is still alive. Expired
// sessions are treated as missing and removed in the same call so a follow-up
// Get doesn't hit a stale record.
func (s *FaceDetectionStore) Get(sessionID string) (*FaceDetectionSession, bool) {
	if sessionID == "" {
		return nil, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	session, ok := s.sessions[sessionID]
	if !ok {
		return nil, false
	}
	if time.Now().UTC().After(session.ExpiresAt) {
		delete(s.sessions, sessionID)
		return nil, false
	}
	return session, true
}

func (s *FaceDetectionStore) Delete(sessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, sessionID)
}

// CleanupExpired drops every session whose ExpiresAt is in the past.
func (s *FaceDetectionStore) CleanupExpired() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cleanupExpiredLocked()
}

func (s *FaceDetectionStore) cleanupExpiredLocked() {
	now := time.Now().UTC()
	for id, session := range s.sessions {
		if now.After(session.ExpiresAt) {
			delete(s.sessions, id)
		}
	}
}
