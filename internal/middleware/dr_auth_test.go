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

// fakeClaimsVerifier accepts exactly one token and returns the configured
// claims for it; every other token is rejected. It stands in for the Firebase
// Admin SDK so these tests never touch real Google credentials.
type fakeClaimsVerifier struct {
	acceptToken string
	claims      *DRClaims
}

func (f *fakeClaimsVerifier) VerifyWithClaims(_ context.Context, idToken string) (*DRClaims, error) {
	if idToken == f.acceptToken {
		return f.claims, nil
	}
	return nil, errors.New("token rejected")
}

// drAuthTestRouter mounts a single guarded endpoint that echoes whether the
// verified claims made it into the gin context, so tests can assert both the
// status code and that RequireDoubleRavenAuth stored the claims on success.
func drAuthTestRouter(verifier ClaimsVerifier, allowedEmails []string) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	group := r.Group("/api/dr")
	group.Use(RequireDoubleRavenAuth(verifier, allowedEmails))
	group.GET("/docs", func(c *gin.Context) {
		claims, ok := c.Get(DRContextKey)
		email := ""
		if dr, isDR := claims.(*DRClaims); ok && isDR {
			email = dr.Email
		}
		c.JSON(http.StatusOK, gin.H{"ok": true, "email": email})
	})
	return r
}

func drRequest(t *testing.T, r *gin.Engine, authHeader string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/dr/docs", nil)
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestRequireDoubleRavenAuth(t *testing.T) {
	validClaims := &DRClaims{UID: "uid-1", Email: "Owner@Example.com", EmailVerified: false}

	tests := []struct {
		name         string
		verifier     ClaimsVerifier
		allowed      []string
		authHeader   string
		wantStatus   int
		wantEmailSet bool
	}{
		{
			// Admin SDK failed to init: reject everything rather than letting
			// unauthenticated traffic reach the document store.
			name:       "nil verifier fails closed with 503",
			verifier:   nil,
			authHeader: "Bearer good-token",
			wantStatus: http.StatusServiceUnavailable,
		},
		{
			name:       "missing header rejected",
			verifier:   &fakeClaimsVerifier{acceptToken: "good-token", claims: validClaims},
			authHeader: "",
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "malformed header rejected",
			verifier:   &fakeClaimsVerifier{acceptToken: "good-token", claims: validClaims},
			authHeader: "good-token", // no Bearer prefix
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "verifier error rejected",
			verifier:   &fakeClaimsVerifier{acceptToken: "good-token", claims: validClaims},
			authHeader: "Bearer forged-token",
			wantStatus: http.StatusUnauthorized,
		},
		{
			// Allowlist compare is case-insensitive: config lowercases the list,
			// the middleware lowercases the claim.
			name:         "valid and allowlisted passes with claims in context",
			verifier:     &fakeClaimsVerifier{acceptToken: "good-token", claims: validClaims},
			allowed:      []string{"owner@example.com"},
			authHeader:   "Bearer good-token",
			wantStatus:   http.StatusOK,
			wantEmailSet: true,
		},
		{
			name:       "valid but not allowlisted forbidden",
			verifier:   &fakeClaimsVerifier{acceptToken: "good-token", claims: validClaims},
			allowed:    []string{"someone-else@example.com"},
			authHeader: "Bearer good-token",
			wantStatus: http.StatusForbidden,
		},
		{
			// Empty allowlist => any verified user passes (accounts are manual-only).
			name:         "valid with empty allowlist passes",
			verifier:     &fakeClaimsVerifier{acceptToken: "good-token", claims: validClaims},
			allowed:      nil,
			authHeader:   "Bearer good-token",
			wantStatus:   http.StatusOK,
			wantEmailSet: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := drAuthTestRouter(tc.verifier, tc.allowed)
			w := drRequest(t, r, tc.authHeader)
			if w.Code != tc.wantStatus {
				t.Fatalf("expected status %d, got %d (%s)", tc.wantStatus, w.Code, w.Body.String())
			}
			if tc.wantEmailSet && !strings.Contains(w.Body.String(), validClaims.Email) {
				t.Fatalf("expected verified claims (%s) echoed from context, got %s", validClaims.Email, w.Body.String())
			}
		})
	}
}
