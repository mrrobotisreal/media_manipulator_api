package lab

import (
	"fmt"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/mrrobotisreal/media_manipulator_api/internal/middleware"
	"github.com/mrrobotisreal/media_manipulator_api/internal/tiers"
)

// notFoundBody is gin's own body for an unmatched route, so a gated Lab route is
// byte-identical to a route that does not exist (an engine without a custom
// NoRoute handler; the test compares against the live engine).
const notFoundBody = "404 page not found"

// Gate decides who reaches a Lab route (ticket decision 1). It passes only when
// ALL of these hold, each derived per request from the verified Firebase token
// (never from a header, a cookie or a stored flag):
//
//  1. a verified Firebase session;
//  2. email_verified;
//  3. the tier resolves to operator (ADR-0005, tiers.Registry.ResolveFor);
//  4. the lowercase email is in LAB_EMAILS.
//
// An empty LAB_EMAILS turns the Lab off for everyone.
type Gate struct {
	emails map[string]struct{}
}

// NewGate builds the gate from LAB_EMAILS (already lowercased by the loader;
// lowercased again here).
func NewGate(emails []string) *Gate {
	g := &Gate{emails: map[string]struct{}{}}
	for _, e := range emails {
		if e = strings.ToLower(strings.TrimSpace(e)); e != "" {
			g.emails[e] = struct{}{}
		}
	}
	return g
}

// Count is how many accounts are allowlisted (for the start-up line only; the
// addresses are never logged).
func (g *Gate) Count() int {
	if g == nil {
		return 0
	}
	return len(g.emails)
}

// StartupLine is the one log line printed at boot.
func (g *Gate) StartupLine() string {
	if g.Count() == 0 {
		return "lab: disabled"
	}
	return fmt.Sprintf("lab: enabled for %d account(s)", g.Count())
}

// Passes reports whether the request may use the Lab.
func (g *Gate) Passes(c *gin.Context) bool {
	if g == nil || len(g.emails) == 0 || c == nil {
		return false
	}
	p, signedIn := middleware.PrincipalFrom(c)
	if !signedIn || p == nil || !p.EmailVerified {
		return false
	}
	res, ok := middleware.ResolveTier(c)
	if !ok || res.Tier != tiers.TierOperator {
		return false
	}
	_, listed := g.emails[strings.ToLower(strings.TrimSpace(p.Email))]
	return listed
}

// NotFound answers the request exactly like a missing route and stops the chain.
// It logs nothing: a probe must leave no trace that names the route.
func NotFound(c *gin.Context) {
	c.Data(404, "text/plain", []byte(notFoundBody))
	c.Abort()
}

// RequireLab is the middleware on every /api/lab route.
func (g *Gate) RequireLab() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !g.Passes(c) {
			NotFound(c)
			return
		}
		c.Next()
	}
}
