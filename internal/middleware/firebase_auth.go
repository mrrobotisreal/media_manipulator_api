package middleware

import (
	"context"
	"net/http"
	"strings"

	firebase "firebase.google.com/go/v4"
	"firebase.google.com/go/v4/auth"
	"github.com/gin-gonic/gin"
)

// Firebase auth seam for the restricted AI Video Restoration deployment
// (dr.media-manipulator.com). When RESTORE_REQUIRE_FIREBASE_AUTH is on, every
// /api/video-restore/* request must carry "Authorization: Bearer <ID token>"
// from a Firebase Authentication user (accounts are created manually in the
// Firebase console). When the flag is off — the default — the middleware
// passes everything through untouched, so current public behavior is
// unchanged. Firebase only; this project never uses Supabase.

// TokenVerifier verifies a raw Firebase ID token. It is an interface so the
// "auth on" path is unit-testable without real Google credentials.
type TokenVerifier interface {
	Verify(ctx context.Context, idToken string) error
}

// firebaseVerifier adapts the Firebase Admin SDK auth client.
type firebaseVerifier struct {
	client *auth.Client
}

func (v *firebaseVerifier) Verify(ctx context.Context, idToken string) error {
	_, err := v.client.VerifyIDToken(ctx, idToken)
	return err
}

// NewFirebaseVerifier builds a TokenVerifier backed by the Firebase Admin
// SDK. Credentials come from GOOGLE_APPLICATION_CREDENTIALS (the standard
// Google env var, read by the SDK itself); projectID comes from
// FIREBASE_PROJECT_ID.
func NewFirebaseVerifier(ctx context.Context, projectID string) (TokenVerifier, error) {
	app, err := firebase.NewApp(ctx, &firebase.Config{ProjectID: strings.TrimSpace(projectID)})
	if err != nil {
		return nil, err
	}
	client, err := app.Auth(ctx)
	if err != nil {
		return nil, err
	}
	return &firebaseVerifier{client: client}, nil
}

// RequireFirebaseAuth gates a route group behind Firebase Authentication.
// enabled=false → pass-through (the default deployment). enabled=true with a
// nil verifier (init failed) fails CLOSED with 503 rather than letting
// unauthenticated traffic through.
func RequireFirebaseAuth(enabled bool, verifier TokenVerifier) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !enabled {
			c.Next()
			return
		}
		if verifier == nil {
			c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": "Authentication is unavailable"})
			return
		}
		header := strings.TrimSpace(c.GetHeader("Authorization"))
		token, ok := strings.CutPrefix(header, "Bearer ")
		token = strings.TrimSpace(token)
		if !ok || token == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
			return
		}
		if err := verifier.Verify(c.Request.Context(), token); err != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "Invalid or expired credentials"})
			return
		}
		c.Next()
	}
}
