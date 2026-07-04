package middleware

import (
	"context"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// Double Raven partner portal auth. Unlike the restore seam in
// firebase_auth.go — which is a toggleable pass-through gate keyed on
// RESTORE_REQUIRE_FIREBASE_AUTH — the /api/dr/* endpoints are ALWAYS gated and
// fail CLOSED. There is deliberately no "auth disabled" mode: the portal serves
// exactly two manually-provisioned accounts, so unauthenticated traffic must
// never reach the document store. The verifier here also returns identity
// claims (not just a pass/fail) because the portal enforces an email allowlist
// on top of token validity.

// DRContextKey is the gin context key the verified claims are stored under on a
// successful RequireDoubleRavenAuth pass, so downstream handlers can read who is
// calling without re-verifying the token.
const DRContextKey = "drClaims"

// DRClaims are the verified identity claims the DR portal cares about. Extracted
// from the Firebase ID token: UID is the stable account id; Email drives the
// allowlist check; EmailVerified is captured for completeness but is NOT
// required to pass — accounts are created by hand in the Firebase console and
// console-created email/password users are not email-verified by default, so the
// allowlist (not the verified flag) is the real authorization boundary.
type DRClaims struct {
	UID           string
	Email         string
	EmailVerified bool
}

// ClaimsVerifier verifies a Firebase ID token AND returns identity claims. It is
// an interface so RequireDoubleRavenAuth is unit-testable without real Google
// credentials (see dr_auth_test.go).
type ClaimsVerifier interface {
	VerifyWithClaims(ctx context.Context, idToken string) (*DRClaims, error)
}

// VerifyWithClaims lets the existing firebaseVerifier satisfy ClaimsVerifier in
// addition to TokenVerifier. email / email_verified live in the token's custom
// claims map; UID is a first-class field.
func (v *firebaseVerifier) VerifyWithClaims(ctx context.Context, idToken string) (*DRClaims, error) {
	token, err := v.client.VerifyIDToken(ctx, idToken)
	if err != nil {
		return nil, err
	}
	email, _ := token.Claims["email"].(string)
	emailVerified, _ := token.Claims["email_verified"].(bool)
	return &DRClaims{
		UID:           token.UID,
		Email:         email,
		EmailVerified: emailVerified,
	}, nil
}

// NewFirebaseClaimsVerifier builds a ClaimsVerifier backed by the Firebase Admin
// SDK, sharing the same app/client construction as NewFirebaseVerifier. Called
// unconditionally at startup (see cmd/api/main.go); on init failure main.go
// leaves the verifier nil and RequireDoubleRavenAuth fails closed with 503.
func NewFirebaseClaimsVerifier(ctx context.Context, projectID string) (ClaimsVerifier, error) {
	client, err := newFirebaseAuthClient(ctx, projectID)
	if err != nil {
		return nil, err
	}
	return &firebaseVerifier{client: client}, nil
}

// RequireDoubleRavenAuth gates /api/dr/* — ALWAYS enforced, fail-closed.
//
//   - nil verifier (Admin SDK failed to init) → 503 for every request. There is
//     no pass-through mode.
//   - missing / malformed Authorization header → 401.
//   - token rejected by the verifier → 401.
//   - allowedEmails non-empty and the token's email is not in it (case-insensitive)
//     → 403.
//   - allowedEmails empty → any verified Firebase user passes (accounts are
//     manual-only); main.go logs a startup warning recommending the allowlist.
//
// On success the verified DRClaims are stored in the gin context under
// DRContextKey. Error bodies use the house gin.H{"error": ...} shape with
// generic messages — never echo the token or reveal whether an email exists.
func RequireDoubleRavenAuth(verifier ClaimsVerifier, allowedEmails []string) gin.HandlerFunc {
	return func(c *gin.Context) {
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
		claims, err := verifier.VerifyWithClaims(c.Request.Context(), token)
		if err != nil || claims == nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "Invalid or expired credentials"})
			return
		}
		if !emailAllowed(claims.Email, allowedEmails) {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "Access denied"})
			return
		}
		c.Set(DRContextKey, claims)
		c.Next()
	}
}

// emailAllowed reports whether email passes the allowlist. An empty allowlist
// permits any authenticated user (accounts are manual-only). allowedEmails is
// pre-lowercased at config load, so we only need to lowercase the claim.
func emailAllowed(email string, allowedEmails []string) bool {
	if len(allowedEmails) == 0 {
		return true
	}
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" {
		return false
	}
	for _, allowed := range allowedEmails {
		if email == allowed {
			return true
		}
	}
	return false
}
