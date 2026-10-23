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

// CtxPrincipal is the gin context key under which a verified caller identity
// is stored. Read it through PrincipalFrom rather than by key.
const CtxPrincipal = "mm_principal"

// Principal is the verified identity of a caller: who they are, taken from a
// Firebase ID token whose signature we checked.
//
// The verifier used to discard the decoded token and return only an error, so
// every downstream handler was blind to who was calling. Tiers and quotas need
// that identity, so it now travels in the request context.
type Principal struct {
	// UID is the Firebase user id — stable, and the join key to mm_users.
	UID string
	// Email may be empty; not every provider supplies one.
	Email string
	// EmailVerified as asserted by Firebase.
	EmailVerified bool
	// Claims is the full custom-claims map, for callers that need more.
	Claims map[string]any
	// AuthTime is when the user last actively authenticated (the token's
	// auth_time claim, unix seconds; 0 = unknown). Account deletion requires it
	// to be recent, so a stolen long-lived session cannot delete an account.
	AuthTime int64
}

// TokenVerifier verifies a raw Firebase ID token and returns the identity it
// carries. It is an interface so the "auth on" path is unit-testable without
// real Google credentials.
type TokenVerifier interface {
	Verify(ctx context.Context, idToken string) (*Principal, error)
}

// firebaseVerifier adapts the Firebase Admin SDK auth client.
type firebaseVerifier struct {
	client *auth.Client
}

func (v *firebaseVerifier) Verify(ctx context.Context, idToken string) (*Principal, error) {
	token, err := v.client.VerifyIDToken(ctx, idToken)
	if err != nil {
		return nil, err
	}
	return principalFromToken(token), nil
}

// principalFromToken maps a decoded Firebase token onto our own struct. Email
// and email_verified live in the standard claims map rather than on the token
// struct, so they are read defensively.
func principalFromToken(token *auth.Token) *Principal {
	p := &Principal{UID: token.UID, Claims: token.Claims, AuthTime: token.AuthTime}
	if token.Claims != nil {
		if email, ok := token.Claims["email"].(string); ok {
			p.Email = email
		}
		if verified, ok := token.Claims["email_verified"].(bool); ok {
			p.EmailVerified = verified
		}
	}
	return p
}

// PrincipalFrom returns the verified caller identity, if the request carried
// one. Anonymous requests — every request on a route behind
// OptionalFirebaseAuth without a valid token — return (nil, false).
func PrincipalFrom(c *gin.Context) (*Principal, bool) {
	value, exists := c.Get(CtxPrincipal)
	if !exists {
		return nil, false
	}
	principal, ok := value.(*Principal)
	if !ok || principal == nil {
		return nil, false
	}
	return principal, true
}

// newFirebaseAuthClient constructs the Firebase Admin auth client shared by the
// restore verifier (NewFirebaseVerifier) and the Double Raven claims verifier
// (NewFirebaseClaimsVerifier). Credentials come from
// GOOGLE_APPLICATION_CREDENTIALS (the standard Google env var, read by the SDK
// itself); projectID comes from FIREBASE_PROJECT_ID.
func newFirebaseAuthClient(ctx context.Context, projectID string) (*auth.Client, error) {
	app, err := firebase.NewApp(ctx, &firebase.Config{ProjectID: strings.TrimSpace(projectID)})
	if err != nil {
		return nil, err
	}
	return app.Auth(ctx)
}

// NewFirebaseVerifier builds a TokenVerifier backed by the Firebase Admin
// SDK. Credentials come from GOOGLE_APPLICATION_CREDENTIALS (the standard
// Google env var, read by the SDK itself); projectID comes from
// FIREBASE_PROJECT_ID.
func NewFirebaseVerifier(ctx context.Context, projectID string) (TokenVerifier, error) {
	client, err := newFirebaseAuthClient(ctx, projectID)
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
		principal, err := verifier.Verify(c.Request.Context(), token)
		if err != nil || principal == nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "Invalid or expired credentials"})
			return
		}
		c.Set(CtxPrincipal, principal)
		c.Next()
	}
}

// OptionalFirebaseAuth attaches a Principal when the request carries a valid
// Firebase ID token, and otherwise lets the request continue anonymously. It
// never returns 401.
//
// This is what goes on the public routes. Anonymous use is the product's front
// door — every SEO landing page has to convert a first-time visitor with zero
// friction — so the tier system identifies callers when it can and falls back
// to a session or hashed-IP subject when it cannot. An invalid or expired token
// is treated as no token: a stale token in a long-open tab must degrade to the
// anonymous quota, not to an error page.
//
// `enabled` mirrors RequireFirebaseAuth. With the flag off, or with the flag on
// but no verifier, every request simply passes through unauthenticated — there
// is nothing to fail closed about when the route is public either way.
func OptionalFirebaseAuth(enabled bool, verifier TokenVerifier) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !enabled || verifier == nil {
			c.Next()
			return
		}
		header := strings.TrimSpace(c.GetHeader("Authorization"))
		token, ok := strings.CutPrefix(header, "Bearer ")
		token = strings.TrimSpace(token)
		if !ok || token == "" {
			c.Next()
			return
		}
		principal, err := verifier.Verify(c.Request.Context(), token)
		if err == nil && principal != nil {
			c.Set(CtxPrincipal, principal)
		}
		c.Next()
	}
}
