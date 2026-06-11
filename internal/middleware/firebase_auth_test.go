package middleware

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

type stubVerifier struct {
	acceptToken string
}

func (s *stubVerifier) Verify(_ context.Context, idToken string) error {
	if idToken == s.acceptToken {
		return nil
	}
	return errors.New("token rejected")
}

func firebaseAuthTestRouter(enabled bool, verifier TokenVerifier) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	group := r.Group("/api/video-restore")
	group.Use(RequireFirebaseAuth(enabled, verifier))
	group.GET("/capabilities", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})
	return r
}

func TestRequireFirebaseAuthDisabledPassesThrough(t *testing.T) {
	// The default deployment: flag off, no verifier — requests flow untouched
	// even without any Authorization header.
	r := firebaseAuthTestRouter(false, nil)
	req := httptest.NewRequest(http.MethodGet, "/api/video-restore/capabilities", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 with auth disabled, got %d", w.Code)
	}
}

func TestRequireFirebaseAuthEnabled(t *testing.T) {
	verifier := &stubVerifier{acceptToken: "good-token"}
	r := firebaseAuthTestRouter(true, verifier)

	t.Run("valid bearer token passes", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/video-restore/capabilities", nil)
		req.Header.Set("Authorization", "Bearer good-token")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 with valid token, got %d (%s)", w.Code, w.Body.String())
		}
	})

	t.Run("missing header rejected", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/video-restore/capabilities", nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("expected 401 without header, got %d", w.Code)
		}
	})

	t.Run("malformed header rejected", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/video-restore/capabilities", nil)
		req.Header.Set("Authorization", "good-token") // no Bearer prefix
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("expected 401 for malformed header, got %d", w.Code)
		}
	})

	t.Run("invalid token rejected", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/video-restore/capabilities", nil)
		req.Header.Set("Authorization", "Bearer forged-token")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("expected 401 for invalid token, got %d", w.Code)
		}
	})
}

func TestRequireFirebaseAuthEnabledWithoutVerifierFailsClosed(t *testing.T) {
	// Flag on but the Admin SDK failed to init: reject everything (503)
	// rather than silently letting unauthenticated traffic through.
	r := firebaseAuthTestRouter(true, nil)
	req := httptest.NewRequest(http.MethodGet, "/api/video-restore/capabilities", nil)
	req.Header.Set("Authorization", "Bearer good-token")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 fail-closed, got %d", w.Code)
	}
}
