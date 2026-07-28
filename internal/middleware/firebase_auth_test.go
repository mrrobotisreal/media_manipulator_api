package middleware

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

type stubVerifier struct {
	acceptToken string
}

func (s *stubVerifier) Verify(_ context.Context, idToken string) (*Principal, error) {
	if idToken == s.acceptToken {
		return &Principal{
			UID:           "uid-123",
			Email:         "someone@example.com",
			EmailVerified: true,
			Claims:        map[string]any{"email": "someone@example.com"},
		}, nil
	}
	return nil, errors.New("token rejected")
}

// The handler reports back whether a Principal reached the context, so the
// tests can assert on identity propagation and not only on status codes.
func principalEchoHandler(c *gin.Context) {
	if principal, ok := PrincipalFrom(c); ok {
		c.JSON(http.StatusOK, gin.H{"ok": true, "uid": principal.UID, "email": principal.Email})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "uid": ""})
}

func firebaseAuthTestRouter(enabled bool, verifier TokenVerifier) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	group := r.Group("/api/video-restore")
	group.Use(RequireFirebaseAuth(enabled, verifier))
	group.GET("/capabilities", principalEchoHandler)
	return r
}

func optionalAuthTestRouter(enabled bool, verifier TokenVerifier) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	group := r.Group("/api")
	group.Use(OptionalFirebaseAuth(enabled, verifier))
	group.GET("/upload", principalEchoHandler)
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

	t.Run("valid bearer token passes and carries the principal", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/video-restore/capabilities", nil)
		req.Header.Set("Authorization", "Bearer good-token")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 with valid token, got %d (%s)", w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), `"uid":"uid-123"`) {
			t.Fatalf("expected the verified principal in the handler context, got %s", w.Body.String())
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

func TestOptionalFirebaseAuth(t *testing.T) {
	verifier := &stubVerifier{acceptToken: "good-token"}
	r := optionalAuthTestRouter(true, verifier)

	// The whole point of this middleware: a public route stays public.
	cases := []struct {
		name      string
		header    string
		expectUID string
	}{
		{name: "no header continues anonymously", header: "", expectUID: `"uid":""`},
		{name: "malformed header continues anonymously", header: "good-token", expectUID: `"uid":""`},
		{name: "invalid token continues anonymously", header: "Bearer forged-token", expectUID: `"uid":""`},
		{name: "valid token attaches the principal", header: "Bearer good-token", expectUID: `"uid":"uid-123"`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/upload", nil)
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			if w.Code != http.StatusOK {
				t.Fatalf("OptionalFirebaseAuth must never reject: got %d (%s)", w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), tc.expectUID) {
				t.Fatalf("expected %s in body, got %s", tc.expectUID, w.Body.String())
			}
		})
	}
}

func TestOptionalFirebaseAuthDisabledPassesThrough(t *testing.T) {
	// Flag off, or verifier missing: still public, still anonymous. There is
	// nothing to fail closed about on a route that is open either way.
	for _, r := range []*gin.Engine{optionalAuthTestRouter(false, &stubVerifier{acceptToken: "good-token"}), optionalAuthTestRouter(true, nil)} {
		req := httptest.NewRequest(http.MethodGet, "/api/upload", nil)
		req.Header.Set("Authorization", "Bearer good-token")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", w.Code)
		}
		if !strings.Contains(w.Body.String(), `"uid":""`) {
			t.Fatalf("expected anonymous request, got %s", w.Body.String())
		}
	}
}
